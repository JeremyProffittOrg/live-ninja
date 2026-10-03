package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) *Service { return &Service{store: store, now: time.Now} }

func validateInput(in *Input, now time.Time, creating bool) error {
	in.Title = strings.TrimSpace(in.Title)
	in.Instructions = strings.TrimSpace(in.Instructions)
	if len(in.Title) < 1 || len(in.Title) > 160 || len(in.Instructions) > 2000 {
		return fmt.Errorf("%w: title is required (160 bytes maximum), instructions maximum 2000 bytes", ErrValidation)
	}
	if in.Kind != "reminder" && in.Kind != "review" {
		return fmt.Errorf("%w: only in-app reminder and human review are available", ErrValidation)
	}
	if in.Schedule.Kind == "" {
		in.Schedule.Kind = "once"
	}
	if err := validateSchedule(in.Schedule); err != nil {
		return err
	}
	if creating && in.Schedule.Kind == "once" && in.Schedule.At != "" && !parse(in.Schedule.At).After(now) {
		return fmt.Errorf("%w: one-time schedule must be in the future", ErrValidation)
	}
	return nil
}
func validIdentity(uid, requestID string) error {
	if uid == "" {
		return ErrForbidden
	}
	if len(requestID) < 8 || len(requestID) > 128 {
		return fmt.Errorf("%w: requestId must contain 8 through 128 bytes", ErrValidation)
	}
	return nil
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func ident(prefix string, v any) string { return prefix + digest(v)[:32] }
func cleanJob(r *Record) Job {
	j := r.Job
	j.LastRun = nil
	if len(r.Runs) > 0 {
		run := r.Runs[len(r.Runs)-1]
		j.LastRun = &run
	}
	return j
}
func (s *Service) Get(ctx context.Context, uid, id string) (*Job, error) {
	if uid == "" {
		return nil, ErrForbidden
	}
	r, e := s.read(ctx, uid, id)
	if e != nil {
		return nil, e
	}
	j := cleanJob(r)
	return &j, nil
}
func (s *Service) List(ctx context.Context, uid string, limit int, cursor string) (Page, error) {
	p := Page{Jobs: []Job{}}
	if uid == "" {
		return p, ErrForbidden
	}
	limit = pageLimit(limit)
	r, c, e := s.store.List(ctx, uid, limit, cursor)
	if e != nil {
		return p, e
	}
	p.NextCursor = c
	for i := range r {
		record := &r[i]
		if hasExpired(record, s.now()) {
			record, e = s.read(ctx, uid, record.Job.ID)
			if e != nil {
				return p, e
			}
		}
		p.Jobs = append(p.Jobs, cleanJob(record))
	}
	return p, nil
}
func pageLimit(n int) int {
	if n <= 0 {
		return 25
	}
	if n > 100 {
		return 100
	}
	return n
}
func (s *Service) Create(ctx context.Context, uid string, in Input, requestID string) (*Job, error) {
	if err := validIdentity(uid, requestID); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if err := validateInput(&in, now, false); err != nil {
		return nil, err
	}
	id := ident("job_", []string{uid, requestID})
	fp := digest(in)
	if old, e := s.store.Get(ctx, uid, id); e == nil {
		if old.CreateFingerprint != fp {
			return nil, ErrConflict
		}
		j := cleanJob(old)
		return &j, nil
	} else if !errors.Is(e, ErrNotFound) {
		return nil, e
	}
	if err := validateInput(&in, now, true); err != nil {
		return nil, err
	}
	r := &Record{UserID: uid, Generation: 1, CreateFingerprint: fp, Runs: []Run{}, Receipts: []Receipt{}, Job: Job{ID: id, Title: in.Title, Instructions: in.Instructions, Kind: in.Kind, Schedule: in.Schedule, Status: "active", Version: 1, NextRunAt: next(in.Schedule, now), CreatedAt: stamp(now), UpdatedAt: stamp(now)}}
	if err := s.store.CompareAndSwap(ctx, r, 0); err != nil {
		if errors.Is(err, ErrConflict) {
			return s.Create(ctx, uid, in, requestID)
		}
		return nil, err
	}
	j := cleanJob(r)
	return &j, nil
}

// mutate checks the idempotency receipt before expectedVersion: a response lost
// after commit can be replayed safely with its original version and request ID.
func (s *Service) mutate(ctx context.Context, uid, id, requestID, fp string, version int64, fn func(*Record, time.Time) (string, error)) (*Record, string, error) {
	if e := validIdentity(uid, requestID); e != nil {
		return nil, "", e
	}
	r, e := s.store.Get(ctx, uid, id)
	if e != nil {
		return nil, "", e
	}
	for _, receipt := range r.Receipts {
		if receipt.ID == requestID {
			if receipt.Fingerprint != fp {
				return nil, "", ErrConflict
			}
			return r, receipt.RunID, nil
		}
	}
	if version < 1 || r.Job.Version != version {
		return nil, "", ErrConflict
	}
	now := s.now().UTC()
	runID, e := fn(r, now)
	if e != nil {
		return nil, "", e
	}
	r.Receipts = append(r.Receipts, Receipt{ID: requestID, Fingerprint: fp, RunID: runID})
	if len(r.Receipts) > MaxReceipts {
		r.Receipts = r.Receipts[len(r.Receipts)-MaxReceipts:]
	}
	r.Job.Version++
	r.Job.UpdatedAt = stamp(now)
	r.Job.LastRun = nil
	if e = compact(r, runID); e != nil {
		return nil, "", e
	}
	if e = s.store.CompareAndSwap(ctx, r, version); e != nil {
		if errors.Is(e, ErrConflict) {
			fresh, readErr := s.store.Get(ctx, uid, id)
			if readErr == nil {
				for _, rc := range fresh.Receipts {
					if rc.ID == requestID && rc.Fingerprint == fp {
						return fresh, rc.RunID, nil
					}
				}
			}
		}
		return nil, "", e
	}
	return r, runID, nil
}
func (s *Service) Update(ctx context.Context, uid, id string, in Input, version int64, requestID string) (*Job, error) {
	if e := validateInput(&in, s.now(), false); e != nil {
		return nil, e
	}
	fp := digest([]any{"update", in, version})
	r, _, e := s.mutate(ctx, uid, id, requestID, fp, version, func(r *Record, now time.Time) (string, error) {
		if r.Job.Status == "cancelled" {
			return "", ErrInvalidState
		}
		if in.Schedule != r.Job.Schedule {
			if e := validateInput(&in, now, true); e != nil {
				return "", e
			}
		}
		cancelPending(r, now, "Job edited; previous pending intent cancelled.")
		r.Generation++
		scheduleChanged := in.Schedule != r.Job.Schedule
		r.Job.Title = in.Title
		r.Job.Instructions = in.Instructions
		r.Job.Kind = in.Kind
		r.Job.Schedule = in.Schedule
		if scheduleChanged {
			r.Job.NextRunAt = next(in.Schedule, now)
		}
		return "", nil
	})
	if e != nil {
		return nil, e
	}
	j := cleanJob(r)
	return &j, nil
}
func (s *Service) Action(ctx context.Context, uid, id, action string, version int64, requestID string) (*Job, error) {
	fp := digest([]any{action, version})
	r, _, e := s.mutate(ctx, uid, id, requestID, fp, version, func(r *Record, now time.Time) (string, error) {
		switch action {
		case "pause":
			if r.Job.Status != "active" {
				return "", ErrInvalidState
			}
			r.Job.Status = "paused"
			cancelPending(r, now, "Job paused.")
			r.Generation++
		case "resume":
			if r.Job.Status != "paused" {
				return "", ErrInvalidState
			}
			r.Job.Status = "active"
			r.Job.NextRunAt = next(r.Job.Schedule, now)
			r.Generation++
		case "cancel":
			if r.Job.Status == "cancelled" {
				return "", ErrInvalidState
			}
			r.Job.Status = "cancelled"
			r.Job.NextRunAt = ""
			cancelPending(r, now, "Job cancelled.")
			r.Generation++
		default:
			return "", fmt.Errorf("%w: unknown action", ErrValidation)
		}
		return "", nil
	})
	if e != nil {
		return nil, e
	}
	j := cleanJob(r)
	return &j, nil
}
func cancelPending(r *Record, now time.Time, reason string) {
	for i := range r.Runs {
		v := &r.Runs[i]
		if v.Status == "queued" || v.Status == "waiting_approval" {
			finish(v, "cancelled", reason, now)
		}
	}
}
func finish(r *Run, status, result string, now time.Time) {
	r.Status = status
	r.Progress = result
	r.UpdatedAt = stamp(now)
	r.FinishedAt = stamp(now)
	if status == "succeeded" {
		r.Result = result
	}
}
func reserveRun(r *Record) error {
	for len(r.Runs) >= MaxRuns {
		found := -1
		for i, v := range r.Runs {
			if v.Status != "queued" && v.Status != "waiting_approval" {
				found = i
				break
			}
		}
		if found < 0 {
			return ErrLimit
		}
		r.Runs = append(r.Runs[:found], r.Runs[found+1:]...)
	}
	return nil
}

// JSON escaping can expand input bytes sixfold. Bound the ACTUAL encoded record
// as well as run count; preserve unresolved work and the mutation's result.
func compact(r *Record, keepID string) error {
	for {
		b, e := json.Marshal(r)
		if e != nil {
			return e
		}
		if len(b) <= 250000 {
			return nil
		}
		found := -1
		for i, v := range r.Runs {
			if v.ID != keepID && v.Status != "queued" && v.Status != "waiting_approval" {
				found = i
				break
			}
		}
		if found < 0 {
			return ErrLimit
		}
		r.Runs = append(r.Runs[:found], r.Runs[found+1:]...)
	}
}
func newRun(r *Record, id string, now time.Time) Run {
	return Run{ID: id, JobID: r.Job.ID, Title: r.Job.Title, Instructions: r.Job.Instructions, Kind: r.Job.Kind, Status: "queued", Progress: "Queued for the in-app worker.", Attempt: 1, Generation: r.Generation, CreatedAt: stamp(now), UpdatedAt: stamp(now)}
}
func runByID(r *Record, id string) (*Run, error) {
	for i := range r.Runs {
		if r.Runs[i].ID == id {
			return &r.Runs[i], nil
		}
	}
	return nil, ErrNotFound
}
func resultRun(r *Record, id string, e error) (*Run, error) {
	if e != nil {
		return nil, e
	}
	v, e := runByID(r, id)
	if e != nil {
		return nil, e
	}
	out := *v
	return &out, nil
}
func (s *Service) RunNow(ctx context.Context, uid, id string, version int64, requestID string) (*Run, error) {
	r, rid, e := s.mutate(ctx, uid, id, requestID, digest([]any{"run", version}), version, func(r *Record, now time.Time) (string, error) {
		if r.Job.Status != "active" {
			return "", ErrInvalidState
		}
		if e := reserveRun(r); e != nil {
			return "", e
		}
		run := newRun(r, ident("run_", []string{id, requestID}), now)
		executeLocal(&run, now)
		r.Runs = append(r.Runs, run)
		return run.ID, nil
	})
	return resultRun(r, rid, e)
}
func (s *Service) RetryRun(ctx context.Context, uid, id, runID string, version int64, requestID string) (*Run, error) {
	r, rid, e := s.mutate(ctx, uid, id, requestID, digest([]any{"retry", runID, version}), version, func(r *Record, now time.Time) (string, error) {
		old, e := runByID(r, runID)
		if e != nil {
			return "", e
		}
		if r.Job.Status != "active" || (old.Status != "failed" && old.Status != "cancelled") || old.Generation != r.Generation || old.Attempt >= 3 {
			return "", ErrInvalidState
		}
		if old.Kind != "review" && old.Kind != "reminder" {
			return "", ErrInvalidState
		}
		for _, other := range r.Runs {
			if other.RetryOf == old.ID {
				return "", ErrInvalidState
			}
		}
		copy := *old
		if e := reserveRun(r); e != nil {
			return "", e
		}
		run := newRun(r, ident("run_", []string{id, requestID}), now)
		run.Attempt = copy.Attempt + 1
		run.RetryOf = copy.ID
		executeLocal(&run, now)
		r.Runs = append(r.Runs, run)
		return run.ID, nil
	})
	return resultRun(r, rid, e)
}

// ApproveRun must only be exposed through authenticated human routes. It marks
// the exact retained checkpoint acknowledged; it grants NO external authority.
func (s *Service) ApproveRun(ctx context.Context, uid, id, runID string, version int64, requestID string) (*Run, error) {
	r, rid, e := s.mutate(ctx, uid, id, requestID, digest([]any{"approve", runID, version}), version, func(r *Record, now time.Time) (string, error) {
		v, e := runByID(r, runID)
		if e != nil {
			return "", e
		}
		if r.Job.Status != "active" || v.Status != "waiting_approval" || v.Generation != r.Generation || !parse(v.ApprovalExpiresAt).After(now) {
			return "", ErrInvalidState
		}
		v.ApprovedBy = uid
		finish(v, "succeeded", "You acknowledged this review checkpoint. No external action was performed.", now)
		return runID, nil
	})
	return resultRun(r, rid, e)
}
func (s *Service) CancelRun(ctx context.Context, uid, id, runID string, version int64, requestID string) (*Run, error) {
	r, rid, e := s.mutate(ctx, uid, id, requestID, digest([]any{"cancel-run", runID, version}), version, func(r *Record, now time.Time) (string, error) {
		v, e := runByID(r, runID)
		if e != nil {
			return "", e
		}
		if v.Status != "queued" && v.Status != "waiting_approval" {
			return "", ErrInvalidState
		}
		finish(v, "cancelled", "Run cancelled before any external action.", now)
		return runID, nil
	})
	return resultRun(r, rid, e)
}
func (s *Service) ListRuns(ctx context.Context, uid, id string, limit int, cursor string) (RunPage, error) {
	p := RunPage{Runs: []Run{}}
	if uid == "" {
		return p, ErrForbidden
	}
	r, e := s.read(ctx, uid, id)
	if e != nil {
		return p, e
	}
	start := len(r.Runs) - 1
	if cursor != "" {
		found := false
		for i := range r.Runs {
			if r.Runs[i].ID == cursor {
				start = i - 1
				found = true
				break
			}
		}
		if !found {
			return p, fmt.Errorf("%w: run cursor expired; refresh history", ErrValidation)
		}
	}
	limit = pageLimit(limit)
	for i := start; i >= 0; i-- {
		p.Runs = append(p.Runs, r.Runs[i])
		if len(p.Runs) == limit {
			if i > 0 {
				p.NextCursor = r.Runs[i].ID
			}
			break
		}
	}
	return p, nil
}

func hasExpired(r *Record, now time.Time) bool {
	for _, v := range r.Runs {
		if v.Status == "waiting_approval" && !parse(v.ApprovalExpiresAt).After(now) {
			return true
		}
	}
	return false
}

// Manual jobs remain usable when the scheduler is disabled: a read persists
// expired review receipts via CAS before exposing Retry in the interface.
func (s *Service) read(ctx context.Context, uid, id string) (*Record, error) {
	for n := 0; n < 4; n++ {
		r, e := s.store.Get(ctx, uid, id)
		if e != nil {
			return nil, e
		}
		now := s.now().UTC()
		if !hasExpired(r, now) {
			return r, nil
		}
		version := r.Job.Version
		for i := range r.Runs {
			v := &r.Runs[i]
			if v.Status == "waiting_approval" && !parse(v.ApprovalExpiresAt).After(now) {
				v.Error = "Review expired after 24 hours without acknowledgment."
				finish(v, "failed", v.Error, now)
			}
		}
		r.Job.Version++
		r.Job.UpdatedAt = stamp(now)
		r.RetryAfter = ""
		r.Job.Error = ""
		if e = s.store.CompareAndSwap(ctx, r, version); errors.Is(e, ErrConflict) {
			continue
		} else if e != nil {
			return nil, e
		}
		return r, nil
	}
	return nil, ErrConflict
}

func dueAt(r *Record) time.Time {
	if r.Job.Status != "active" {
		return time.Time{}
	}
	at := parse(r.Job.NextRunAt)
	for _, run := range r.Runs {
		var candidate time.Time
		switch run.Status {
		case "queued":
			candidate = parse(run.CreatedAt)
		case "waiting_approval":
			candidate = parse(run.ApprovalExpiresAt)
		}
		if !candidate.IsZero() && (at.IsZero() || candidate.Before(at)) {
			at = candidate
		}
	}
	if retry := parse(r.RetryAfter); !at.IsZero() && retry.After(at) {
		at = retry
	}
	return at
}

// Tick uses a due-index query and current strongly consistent job records.
// Pure in-app execution and its completion receipt commit atomically. Recurring
// backlog coalesces to one overdue occurrence, then advances beyond now.
func (s *Service) Tick(ctx context.Context, now time.Time, limit int) (TickResult, error) {
	result := TickResult{}
	refs, e := s.store.Due(ctx, now, pageLimit(limit))
	if e != nil {
		return result, e
	}
	var failures []error
	for _, ref := range refs {
		if e := ctx.Err(); e != nil {
			return result, e
		}
		r, e := s.store.Get(ctx, ref.UserID, ref.ID)
		if errors.Is(e, ErrNotFound) {
			result.Skipped++
			continue
		}
		if e != nil {
			if errors.Is(e, ErrCorrupt) {
				if q, ok := s.store.(interface {
					Quarantine(context.Context, Ref) error
				}); ok {
					if qe := q.Quarantine(ctx, ref); qe != nil && !errors.Is(qe, ErrConflict) {
						failures = append(failures, qe)
					}
				}
			}
			failures = append(failures, e)
			result.Failed++
			continue
		}
		at := dueAt(r)
		if at.IsZero() || at.After(now) {
			result.Skipped++
			continue
		}
		version := r.Job.Version
		r.RetryAfter = ""
		r.Job.Error = ""
		for i := range r.Runs {
			v := &r.Runs[i]
			if v.Status == "waiting_approval" && !parse(v.ApprovalExpiresAt).After(now) {
				v.Error = "Review expired after 24 hours without acknowledgment."
				finish(v, "failed", v.Error, now)
			}
		}
		for i := range r.Runs {
			v := &r.Runs[i]
			if v.Status != "queued" {
				continue
			}
			if v.Generation != r.Generation {
				finish(v, "cancelled", "Job changed before execution.", now)
				continue
			}
			executeLocal(v, now)
		}
		if r.Job.NextRunAt != "" && !parse(r.Job.NextRunAt).After(now) {
			capacityErr := reserveRun(r)
			if capacityErr == nil {
				run := newRun(r, ident("run_", []string{r.Job.ID, strconv.FormatInt(r.Generation, 10), r.Job.NextRunAt}), now)
				run.ScheduledFor = r.Job.NextRunAt
				executeLocal(&run, now)
				r.Runs = append(r.Runs, run)
				capacityErr = compact(r, run.ID)
				if capacityErr != nil {
					r.Runs = r.Runs[:len(r.Runs)-1]
				}
			}
			if capacityErr != nil {
				r.Job.Error = "Pending review capacity reached. Resolve or cancel pending reviews; scheduling retries in 15 minutes."
				r.RetryAfter = stamp(now.Add(15 * time.Minute))
				result.Failed++
			} else {
				r.Job.NextRunAt = next(r.Job.Schedule, now)
			}
		}
		r.Job.Version++
		r.Job.UpdatedAt = stamp(now)
		r.Job.LastRun = nil
		if e = s.store.CompareAndSwap(ctx, r, version); e != nil {
			if errors.Is(e, ErrConflict) {
				result.Conflicts++
				continue
			}
			if errors.Is(e, ErrForbidden) {
				result.Skipped++
				if f, ok := s.store.(interface {
					SuspendInactive(context.Context, Ref) error
				}); ok {
					if suspendErr := f.SuspendInactive(ctx, ref); suspendErr != nil && !errors.Is(suspendErr, ErrConflict) {
						failures = append(failures, suspendErr)
					}
				}
				continue
			}
			result.Failed++
			failures = append(failures, e)
			continue
		}
		result.Processed++
	}
	return result, errors.Join(failures...)
}

// Both supported providers have no external side effects. The same atomic job
// write commits execution and its receipt, so manual runs need no live worker.
func executeLocal(v *Run, now time.Time) {
	switch v.Kind {
	case "reminder":
		finish(v, "succeeded", "Reminder is available in your Jobs inbox. No email or device notification was sent.", now)
	case "review":
		v.Status = "waiting_approval"
		v.Progress = "Waiting for your acknowledgment of this exact review. No external action will run."
		v.ApprovalExpiresAt = stamp(now.Add(24 * time.Hour))
		v.UpdatedAt = stamp(now)
	default:
		v.Error = "Execution provider is not supported."
		finish(v, "failed", v.Error, now)
	}
}
