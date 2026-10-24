package alexa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/google/uuid"
)

var (
	ErrUnauthorized = errors.New("Alexa account or device is not authorized")
	ErrExpired      = errors.New("Alexa conversation expired")
	ErrBusy         = errors.New("another Alexa turn is still working")
	ErrCancelled    = errors.New("Alexa turn cancelled")
)

type Identity struct{ Owner, Device, Grant, Generation string }
type Settings struct {
	Version        int64
	Persona        string
	KeepTranscript bool
	RetentionDays  int
}
type Job struct {
	Owner string `json:"owner"`
	Turn  string `json:"turn"`
}
type Work struct {
	Identity           Identity
	Receipt, SessionID string
	Deadline           time.Time
	StartedAt          time.Time
	Sequence           int
	Messages           []realtime.ChatMessage
	Settings           Settings
	Check              func(context.Context) error
}
type RunResult struct {
	Text     string
	Messages []realtime.ChatMessage
	Limited  bool
}
type View struct {
	State, Text, Receipt string
	More                 bool
}
type exchange struct {
	Receipt  string
	Messages []realtime.ChatMessage
}
type session struct {
	ID, AskSession, Grant, Generation, LastTurn string
	History                                     []exchange
	SpeechStart, SpeechEnd                      int
	StartedAt                                   int64
	Turns                                       int
}
type turn struct {
	Identity                             Identity
	ID, SessionID, Input, State, Failure string
	Settings                             Settings
	Deadline                             int64
}
type ownerLock struct{ Turn string }

type Service struct {
	Log       *slog.Logger
	Repo      Repository
	Now       func() time.Time
	Authorize func(context.Context, Identity) error
	Settings  func(context.Context, Identity) (Settings, error)
	Publish   func(context.Context, Job) error
	Run       func(context.Context, Work) (RunResult, error)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func sessionKey(id Identity) string { return "SESSION#" + id.Device }
func turnKey(id string) string      { return "TURN#" + id }
func (s *Service) read(ctx context.Context, owner, key string, value any) (*Entry, error) {
	e, err := s.Repo.Read(ctx, owner, key)
	if err != nil || e == nil {
		return e, err
	}
	if err = json.Unmarshal(e.Data, value); err != nil {
		return nil, err
	}
	return e, nil
}
func (s *Service) authorized(ctx context.Context, id Identity) error {
	if id.Owner == "" || id.Device == "" || id.Grant == "" || id.Generation == "" || s.Authorize == nil {
		return ErrUnauthorized
	}
	return s.Authorize(ctx, id)
}

func (s *Service) Submit(ctx context.Context, id Identity, request, askSession, text string) (view View, resultErr error) {
	started := time.Now()
	logger := s.Log
	if logger != nil {
		logger = logger.With("requestRef", DiagnosticRef(request))
	}
	phase := "authorization"
	LogDiagnostic(ctx, logger, "submission_started", nil, slog.Int("inputBytes", len(text)))
	defer func() {
		LogDiagnostic(ctx, logger, "submission_completed", resultErr, slog.String("operationStage", phase), slog.String("receipt", view.Receipt), slog.String("outcome", view.State), slog.Int64("elapsedMs", time.Since(started).Milliseconds()))
	}()
	if err := s.authorized(ctx, id); err != nil {
		return View{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 4000 || request == "" || askSession == "" {
		return View{}, ErrEnvelope
	}
	receipt := digest(id.Device + "\x00" + request)
	phase = "duplicate_check"
	var prior turn
	priorRow, err := s.read(ctx, id.Owner, turnKey(receipt), &prior)
	if err != nil {
		return View{}, err
	}
	if priorRow != nil {
		return s.viewTurn(ctx, id, prior, priorRow)
	}
	phase = "settings"
	settings, err := s.Settings(ctx, id)
	if err != nil {
		return View{}, err
	}
	LogDiagnostic(ctx, logger, "settings_loaded", nil, slog.Int64("settingsVersion", settings.Version), slog.Bool("keepTranscript", settings.KeepTranscript))
	phase = "session"
	var state session
	stateRow, err := s.read(ctx, id.Owner, sessionKey(id), &state)
	if err != nil {
		return View{}, err
	}
	if stateRow == nil || stateRow.Expires <= s.now().Unix() || state.AskSession != askSession || state.Grant != id.Grant || state.Generation != id.Generation {
		// A new launch must be explicit when the previous device still has work.
		if err = s.Reset(ctx, id, askSession, true); err != nil {
			return View{}, err
		}
		stateRow, err = s.read(ctx, id.Owner, sessionKey(id), &state)
		if err != nil {
			return View{}, err
		}
	}
	var lock ownerLock
	phase = "owner_lock"
	lockRow, err := s.read(ctx, id.Owner, "LOCK", &lock)
	if err != nil {
		return View{}, err
	}
	if lockRow != nil && lockRow.Expires > s.now().Unix() {
		return View{}, ErrBusy
	}
	now := s.now()
	current := turn{Identity: id, ID: receipt, SessionID: state.ID, Input: text, State: "pending", Settings: settings, Deadline: now.Add(WorkerBudget).UnixMilli()}
	changes := []Change{}
	// Remove an expired worker's input before replacing its last-turn pointer.
	// A later Stop must not leave a stranded plaintext input outside the session.
	if state.LastTurn != "" {
		var previous turn
		previousRow, e := s.read(ctx, id.Owner, turnKey(state.LastTurn), &previous)
		if e != nil {
			return View{}, e
		}
		if previousRow != nil && (previous.State == "pending" || previous.State == "running") {
			if previous.Deadline > now.UnixMilli() {
				return View{}, ErrBusy
			}
			previous.Input = ""
			previous.State = "failed"
			previous.Failure = "worker_interrupted"
			changes = append(changes, changed(id.Owner, turnKey(previous.ID), previous, previousRow.Expires, previousRow))
		}
	}
	state.LastTurn = receipt
	state.SpeechStart = 0
	state.SpeechEnd = 0
	changes = append(changes, changed(id.Owner, turnKey(receipt), current, now.Add(RecoveryWindow).Unix(), nil), changed(id.Owner, sessionKey(id), state, now.Add(RecoveryWindow).Unix(), stateRow), changed(id.Owner, "LOCK", ownerLock{Turn: receipt}, now.Add(WorkerBudget+5*time.Second).Unix(), lockRow))
	phase = "receipt_commit"
	err = s.Repo.Commit(ctx, changes)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			if row, e := s.read(ctx, id.Owner, turnKey(receipt), &prior); e == nil && row != nil {
				return s.viewTurn(ctx, id, prior, row)
			}
			return View{}, ErrBusy
		}
		return View{}, err
	}
	phase = "queue_publish"
	publishStarted := time.Now()
	LogDiagnostic(ctx, logger, "queue_publish_started", nil, slog.String("receipt", receipt))
	if s.Publish == nil {
		err = errors.New("Alexa queue is not configured")
	} else {
		err = s.Publish(ctx, Job{Owner: id.Owner, Turn: receipt})
	}
	LogDiagnostic(ctx, logger, "queue_publish_completed", err, slog.String("receipt", receipt), slog.Int64("elapsedMs", time.Since(publishStarted).Milliseconds()))
	if err != nil {
		var latest turn
		row, readErr := s.read(ctx, id.Owner, turnKey(receipt), &latest)
		if readErr == nil && row != nil && latest.State == "pending" {
			_ = s.finish(ctx, latest, row, RunResult{}, "failed", "queue_unavailable")
		} else if readErr == nil && row != nil {
			return s.viewTurn(ctx, id, latest, row)
		}
		return View{State: "failed", Receipt: receipt}, err
	}
	return View{State: "pending", Receipt: receipt}, nil
}

// Reset clears recoverable content and invalidates any active work on this Echo.
// It does not stop a turn on another Echo belonging to the same owner.
func (s *Service) Reset(ctx context.Context, id Identity, askSession string, start bool) error {
	if err := s.authorized(ctx, id); err != nil {
		return err
	}
	var old session
	row, err := s.read(ctx, id.Owner, sessionKey(id), &old)
	if err != nil {
		return err
	}
	next := session{ID: uuid.NewString(), AskSession: askSession, Grant: id.Grant, Generation: id.Generation, StartedAt: s.now().UnixMilli()}
	expires := s.now().Add(RecoveryWindow).Unix()
	if !start {
		expires = s.now().Unix()
	}
	changes := []Change{changed(id.Owner, sessionKey(id), next, expires, row)}
	if old.LastTurn != "" {
		var active turn
		activeRow, e := s.read(ctx, id.Owner, turnKey(old.LastTurn), &active)
		if e != nil {
			return e
		}
		if activeRow != nil && (active.State == "pending" || active.State == "running") {
			active.State = "cancelled"
			active.Input = ""
			active.Failure = "stopped"
			changes = append(changes, changed(id.Owner, turnKey(active.ID), active, activeRow.Expires, activeRow))
		}
		var lock ownerLock
		lockRow, e := s.read(ctx, id.Owner, "LOCK", &lock)
		if e != nil {
			return e
		}
		if lockRow != nil && lock.Turn == old.LastTurn {
			changes = append(changes, Change{Before: lockRow})
		}
	}
	return s.Repo.Commit(ctx, changes)
}

func (s *Service) Resume(ctx context.Context, id Identity, askSession string) (View, error) {
	if err := s.authorized(ctx, id); err != nil {
		return View{}, err
	}
	var state session
	row, err := s.read(ctx, id.Owner, sessionKey(id), &state)
	if err != nil {
		return View{}, err
	}
	if !s.validSession(row, state, id) {
		return View{}, ErrExpired
	}
	state.AskSession = askSession
	if err = s.Repo.Commit(ctx, []Change{changed(id.Owner, sessionKey(id), state, s.now().Add(RecoveryWindow).Unix(), row)}); err != nil {
		return View{}, err
	}
	return s.Status(ctx, id)
}
func (s *Service) validSession(row *Entry, state session, id Identity) bool {
	return row != nil && row.Expires > s.now().Unix() && state.Grant == id.Grant && state.Generation == id.Generation
}
func (s *Service) Status(ctx context.Context, id Identity) (View, error) {
	if err := s.authorized(ctx, id); err != nil {
		return View{}, err
	}
	var state session
	row, err := s.read(ctx, id.Owner, sessionKey(id), &state)
	if err != nil {
		return View{}, err
	}
	if !s.validSession(row, state, id) {
		return View{}, ErrExpired
	}
	if state.LastTurn == "" {
		return View{State: "ready"}, nil
	}
	var current turn
	entry, err := s.read(ctx, id.Owner, turnKey(state.LastTurn), &current)
	if err != nil {
		return View{}, err
	}
	if entry == nil {
		return View{}, ErrExpired
	}
	return s.viewTurn(ctx, id, current, entry)
}
func (s *Service) viewTurn(ctx context.Context, id Identity, current turn, row *Entry) (View, error) {
	if current.Identity != id {
		return View{}, ErrUnauthorized
	}
	view := View{State: current.State, Receipt: current.ID}
	if row.Expires <= s.now().Unix() {
		view.State = "expired"
		return view, nil
	}
	if (current.State == "pending" || current.State == "running") && current.Deadline <= s.now().UnixMilli() {
		view.State = "failed"
		view.Text = "That request timed out. Its outcome may be incomplete. I will not repeat it automatically."
		return view, nil
	}
	var state session
	sessionRow, err := s.read(ctx, id.Owner, sessionKey(id), &state)
	if err != nil {
		return View{}, err
	}
	if !s.validSession(sessionRow, state, id) || state.ID != current.SessionID {
		return View{}, ErrExpired
	}
	for _, exchange := range state.History {
		if exchange.Receipt == current.ID {
			for n := len(exchange.Messages) - 1; n >= 0; n-- {
				if exchange.Messages[n].Role == "assistant" && len(exchange.Messages[n].ToolCalls) == 0 {
					view.Text = exchange.Messages[n].Content
					break
				}
			}
		}
	}
	if current.State == "failed" || current.State == "cancelled" {
		view.Text = "That request did not finish. " + map[string]string{"settings_changed": "Your settings or access changed; start a new request.", "queue_unavailable": "The queue was unavailable; no automatic retry will run.", "worker_interrupted": "The worker was interrupted; check any action before repeating it.", "stopped": "You stopped this conversation.", "execution_failed": "Some work may have completed. Check Live Ninja before repeating an action."}[current.Failure]
	}
	if current.State == "complete" && view.Text == "" {
		view.Text = "That request completed, but its answer is no longer in this conversation's recovery window."
	}
	return view, nil
}

func (s *Service) check(ctx context.Context, t turn) error {
	failed := func(stage string, err error) error {
		LogDiagnostic(ctx, s.Log, "authority_failed", err, slog.String("operationStage", stage), slog.String("receipt", t.ID))
		return err
	}
	if err := ctx.Err(); err != nil {
		return failed("context", err)
	}
	if s.now().UnixMilli() >= t.Deadline {
		return failed("deadline", context.DeadlineExceeded)
	}
	if err := s.authorized(ctx, t.Identity); err != nil {
		return failed("account_device", err)
	}
	settings, err := s.Settings(ctx, t.Identity)
	if err != nil {
		return failed("settings_read", err)
	}
	if settings.Version != t.Settings.Version {
		return failed("settings_version", ErrCancelled)
	}
	var latest turn
	row, err := s.read(ctx, t.Identity.Owner, turnKey(t.ID), &latest)
	if err != nil {
		return failed("receipt_read", err)
	}
	if row == nil || latest.State != "running" {
		return failed("receipt_state", ErrCancelled)
	}
	var state session
	stateRow, err := s.read(ctx, t.Identity.Owner, sessionKey(t.Identity), &state)
	if err != nil {
		return failed("session_read", err)
	}
	if !s.validSession(stateRow, state, t.Identity) || state.ID != t.SessionID || state.LastTurn != t.ID {
		return failed("session_state", ErrCancelled)
	}
	LogDiagnostic(ctx, s.Log, "authority_verified", nil, slog.String("receipt", t.ID), slog.Int64("settingsVersion", settings.Version))
	return nil
}

// CheckWork is the broker's independent authority check. The caller sends only
// internal receipt references; identity and deadlines come from stored state.
func (s *Service) CheckWork(ctx context.Context, job Job) (Work, error) {
	if job.Owner == "" || len(job.Owner) > 256 || len(job.Turn) != 64 {
		return Work{}, ErrEnvelope
	}
	var current turn
	row, err := s.read(ctx, job.Owner, turnKey(job.Turn), &current)
	if err != nil {
		return Work{}, err
	}
	if row == nil || row.Expires <= s.now().Unix() || current.ID != job.Turn || current.Identity.Owner != job.Owner {
		return Work{}, ErrUnauthorized
	}
	if err = s.check(ctx, current); err != nil {
		return Work{}, err
	}
	return Work{Identity: current.Identity, Receipt: current.ID, SessionID: current.SessionID,
		Deadline: time.UnixMilli(current.Deadline), Settings: current.Settings,
		Check: func(c context.Context) error { return s.check(c, current) }}, nil
}

// Execute claims a receipt once. A redelivery of running work is never replayed:
// its model or tool side effect may have completed before the worker disappeared.
func (s *Service) Execute(ctx context.Context, job Job) (resultErr error) {
	started := time.Now()
	LogDiagnostic(ctx, s.Log, "execution_received", nil, slog.String("receiptRef", DiagnosticRef(job.Turn)))
	defer func() {
		LogDiagnostic(ctx, s.Log, "execution_returned", resultErr, slog.String("receiptRef", DiagnosticRef(job.Turn)), slog.Int64("elapsedMs", time.Since(started).Milliseconds()))
	}()
	if job.Owner == "" || len(job.Owner) > 256 || len(job.Turn) != 64 {
		return ErrEnvelope
	}
	var current turn
	row, err := s.read(ctx, job.Owner, turnKey(job.Turn), &current)
	if err != nil {
		return err
	}
	if row == nil || row.Expires <= s.now().Unix() {
		LogDiagnostic(ctx, s.Log, "execution_skipped", nil, slog.String("receipt", job.Turn), slog.String("reason", "expired_or_missing"))
		return nil
	}
	if current.ID != job.Turn || current.Identity.Owner != job.Owner {
		return ErrUnauthorized
	}
	if current.State != "pending" {
		LogDiagnostic(ctx, s.Log, "execution_skipped", nil, slog.String("receipt", job.Turn), slog.String("reason", "already_claimed"))
		return nil
	}
	if current.Deadline <= s.now().UnixMilli() {
		return s.finish(ctx, current, row, RunResult{}, "failed", "worker_interrupted")
	}
	current.State = "running"
	change := changed(job.Owner, turnKey(job.Turn), current, row.Expires, row)
	if err = s.Repo.Commit(ctx, []Change{change}); err != nil {
		if errors.Is(err, ErrConflict) {
			return nil
		}
		return err
	}
	row = change.After
	LogDiagnostic(ctx, s.Log, "execution_claimed", nil, slog.String("receipt", current.ID), slog.Int64("deadlineRemainingMs", current.Deadline-s.now().UnixMilli()))
	workCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(current.Deadline))
	defer cancel()
	if err = s.check(workCtx, current); err != nil {
		return s.finish(ctx, current, row, RunResult{}, "cancelled", "settings_changed")
	}
	var state session
	_, err = s.read(workCtx, job.Owner, sessionKey(current.Identity), &state)
	if err != nil {
		return err
	}
	messages := []realtime.ChatMessage{}
	for _, x := range state.History {
		messages = append(messages, x.Messages...)
	}
	messages = append(messages, realtime.ChatMessage{Role: "user", Content: current.Input})
	LogDiagnostic(ctx, s.Log, "history_loaded", nil, slog.String("receipt", current.ID), slog.Int("messageCount", len(messages)))
	if s.Run == nil {
		return s.finish(ctx, current, row, RunResult{}, "failed", "execution_failed")
	}
	result, runErr := s.Run(workCtx, Work{Identity: current.Identity, Receipt: current.ID, SessionID: current.SessionID, Deadline: time.UnixMilli(current.Deadline), StartedAt: time.UnixMilli(state.StartedAt), Sequence: state.Turns*2 + 1, Messages: messages, Settings: current.Settings, Check: func(c context.Context) error { return s.check(c, current) }})
	LogDiagnostic(ctx, s.Log, "execution_result", runErr, slog.String("receipt", current.ID), slog.Int("outputBytes", len(result.Text)), slog.Bool("limited", result.Limited))
	// Persist only the new exchange, not the already stored context supplied
	// to the runner. Failed/incomplete tool groups are never fed to a later turn.
	if len(result.Messages) >= len(messages) {
		result.Messages = result.Messages[len(messages)-1:]
	} else {
		result.Messages = nil
	}
	if runErr != nil {
		return s.finish(ctx, current, row, result, "failed", "execution_failed")
	}
	if err = s.check(workCtx, current); err != nil {
		return s.finish(ctx, current, row, RunResult{}, "cancelled", "settings_changed")
	}
	if strings.TrimSpace(result.Text) == "" || result.Limited {
		return s.finish(ctx, current, row, result, "failed", "execution_failed")
	}
	return s.finish(ctx, current, row, result, "complete", "")
}

func (s *Service) finish(ctx context.Context, current turn, row *Entry, result RunResult, status, reason string) (resultErr error) {
	started := time.Now()
	applied := false
	outcome := status
	defer func() {
		LogDiagnostic(ctx, s.Log, "turn_persisted", resultErr, slog.String("receipt", current.ID), slog.String("outcome", outcome), slog.String("reason", reason), slog.Bool("applied", applied), slog.Int64("elapsedMs", time.Since(started).Milliseconds()))
	}()
	var state session
	stateRow, err := s.read(ctx, current.Identity.Owner, sessionKey(current.Identity), &state)
	if err != nil {
		return err
	}
	if stateRow == nil || state.ID != current.SessionID || state.LastTurn != current.ID {
		outcome = "superseded"
		return nil
	}
	current.Input = ""
	current.State = status
	current.Failure = reason
	if status == "complete" && len(result.Messages) > 0 {
		state.Turns++
		state.History = append(state.History, exchange{Receipt: current.ID, Messages: append([]realtime.ChatMessage(nil), result.Messages...)})
		state.History = trimHistory(state.History)
	}
	state.SpeechStart = 0
	state.SpeechEnd = 0
	changes := []Change{changed(current.Identity.Owner, turnKey(current.ID), current, row.Expires, row), changed(current.Identity.Owner, sessionKey(current.Identity), state, s.now().Add(RecoveryWindow).Unix(), stateRow)}
	var lock ownerLock
	lockRow, err := s.read(ctx, current.Identity.Owner, "LOCK", &lock)
	if err != nil {
		return err
	}
	if lockRow != nil && lock.Turn == current.ID {
		changes = append(changes, Change{Before: lockRow})
	}
	err = s.Repo.Commit(ctx, changes)
	if errors.Is(err, ErrConflict) {
		outcome = "superseded"
		return nil
	}
	applied = err == nil
	return err
}

func trimHistory(history []exchange) []exchange {
	for len(history) > 0 {
		count := 0
		for _, x := range history {
			count += len(x.Messages)
		}
		encoded, _ := json.Marshal(history)
		if count <= MaxMessages && len(encoded) <= MaxContextBytes {
			return history
		}
		if len(history) == 1 {
			// Keep the final exchange's user and final answer; discard complete
			// intermediate tool groups together, never an orphan tool result.
			messages := history[0].Messages
			if len(messages) > 2 {
				history[0].Messages = []realtime.ChatMessage{messages[0], messages[len(messages)-1]}
				continue
			}
			return nil
		}
		history = history[1:]
	}
	return history
}
