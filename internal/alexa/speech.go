package alexa

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
)

var speechCode = regexp.MustCompile("(?s)```.*?```")
var speechMarkup = regexp.MustCompile(`<[^>]*>`)
var speechURL = regexp.MustCompile(`https?://[^\s)]+`)
var speechCredential = regexp.MustCompile(`(?i)(Bearer\s+[A-Za-z0-9._~+/-]{8,}|sk-[A-Za-z0-9_-]{12,}|ln_mcp_[ar]_[A-Za-z0-9_-]+|amzn1\.oa2\.[A-Za-z0-9._-]+)`)

func safeSpeech(text string) string {
	if strings.Contains(text, "PRIVATE KEY-----") {
		return "That answer contains sensitive key material. View it privately in Live Ninja."
	}
	text = speechCode.ReplaceAllString(text, " Code is available in Live Ninja. ")
	text = speechMarkup.ReplaceAllString(text, "")
	text = speechURL.ReplaceAllString(text, "link in Live Ninja")
	text = speechCredential.ReplaceAllString(text, "sensitive value omitted")
	return strings.Join(strings.Fields(strings.NewReplacer("<", "", ">", "", "`", "").Replace(text)), " ")
}
func speechChunk(text string, start int) (string, int, bool) {
	runes := []rune(text)
	if start >= len(runes) {
		return "That is the full answer.", len(runes), false
	}
	// Reserve space for the spoken "say more" instruction in the 600-character response.
	end := start + 545
	if end >= len(runes) {
		end = len(runes)
	} else {
		for cut := end; cut > start+300; cut-- {
			if unicode.IsSpace(runes[cut-1]) {
				end = cut
				break
			}
		}
	}
	return strings.TrimSpace(string(runes[start:end])), end, end < len(runes)
}
func chatAnswer(text string) realtime.ChatMessage {
	return realtime.ChatMessage{Role: "assistant", Content: text}
}

// BoundMessages drops complete older user exchanges, preserving paired tool
// calls/results. Oversized current work fails explicitly rather than truncating
// a tool result into a fabricated successful response.
func BoundMessages(messages []realtime.ChatMessage) ([]realtime.ChatMessage, error) {
	for len(messages) > 0 {
		raw, err := json.Marshal(messages)
		if err != nil {
			return nil, err
		}
		if len(messages) <= MaxMessages && len(raw) <= MaxContextBytes {
			return messages, nil
		}
		next := -1
		for i := 1; i < len(messages); i++ {
			if messages[i].Role == "user" {
				next = i
				break
			}
		}
		if next < 0 {
			return nil, errors.New("Alexa current turn exceeds its context limit")
		}
		messages = messages[next:]
	}
	return nil, ErrEnvelope
}

func (s *Service) Receipt(ctx context.Context, id Identity, receipt string) (View, error) {
	if err := s.authorized(ctx, id); err != nil {
		return View{}, err
	}
	if len(receipt) != 64 {
		return View{}, ErrEnvelope
	}
	var current turn
	row, err := s.read(ctx, id.Owner, turnKey(receipt), &current)
	if err != nil {
		return View{}, err
	}
	if row == nil {
		return View{}, ErrExpired
	}
	return s.viewTurn(ctx, id, current, row)
}

func (s *Service) Speech(ctx context.Context, id Identity, mode string) (View, error) {
	if err := s.authorized(ctx, id); err != nil {
		return View{}, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		view, err := s.speechOnce(ctx, id, mode)
		if !errors.Is(err, ErrConflict) {
			return view, err
		}
	}
	return View{}, ErrConflict
}

func (s *Service) speechOnce(ctx context.Context, id Identity, mode string) (View, error) {
	var state session
	row, err := s.read(ctx, id.Owner, sessionKey(id), &state)
	if err != nil {
		return View{}, err
	}
	if !s.validSession(row, state, id) {
		return View{}, ErrExpired
	}
	state.prepareReplies()
	if mode == "repeat" && state.LastSpeech != "" {
		view := View{State: state.LastSpeechState, Text: state.LastSpeech, Receipt: state.LastSpeechReceipt, More: state.LastSpeechMore, PendingReplies: len(state.Replies)}
		if view.More && view.PendingReplies > 0 {
			view.PendingReplies--
		}
		err = s.Repo.Commit(ctx, []Change{changed(id.Owner, sessionKey(id), state, s.now().Add(RecoveryWindow).Unix(), row)})
		return view, err
	}
	if len(state.Replies) == 0 {
		return View{State: "ready"}, nil
	}
	reply := &state.Replies[0]
	if reply.State == "pending" || reply.State == "running" {
		view, readErr := s.Receipt(ctx, id, reply.Receipt)
		if errors.Is(readErr, ErrExpired) || (readErr == nil && view.State == "expired") {
			reply.State, reply.Text = "failed", "An earlier answer expired. Check Live Ninja before repeating an action."
		} else if readErr != nil {
			return View{}, readErr
		} else if view.State == "pending" || view.State == "running" {
			return view, nil
		} else {
			reply.State, reply.Text = view.State, safeSpeech(view.Text)
		}
	}
	view := View{State: reply.State, Receipt: reply.Receipt}
	view.Text, reply.Offset, view.More = speechChunk(reply.Text, reply.Offset)
	state.LastSpeech, state.LastSpeechReceipt, state.LastSpeechState, state.LastSpeechMore = view.Text, view.Receipt, view.State, view.More
	if !view.More {
		state.Replies = state.Replies[1:]
	}
	view.PendingReplies = len(state.Replies)
	if view.More {
		view.PendingReplies--
	}
	err = s.Repo.Commit(ctx, []Change{changed(id.Owner, sessionKey(id), state, s.now().Add(RecoveryWindow).Unix(), row)})
	if err == nil {
		LogDiagnostic(ctx, s.Log, "answer_selected", nil, slog.String("receipt", view.Receipt), slog.Int("pendingReplies", view.PendingReplies), slog.Bool("more", view.More), slog.Int("outputBytes", len(view.Text)))
	}
	return view, err
}

// Upgrade existing short-lived sessions without replaying their tools. The
// old cursor described only LastTurn; older delivery state cannot be invented.
func (state *session) prepareReplies() {
	if state.Replies != nil {
		return
	}
	state.Replies = []queuedReply{}
	if state.LastTurn == "" {
		return
	}
	reply := queuedReply{Receipt: state.LastTurn, State: "pending"}
	for _, exchange := range state.History {
		if exchange.Receipt != state.LastTurn {
			continue
		}
		for n := len(exchange.Messages) - 1; n >= 0; n-- {
			message := exchange.Messages[n]
			if message.Role == "assistant" && len(message.ToolCalls) == 0 {
				reply.State, reply.Text = "complete", safeSpeech(message.Content)
				break
			}
		}
	}
	if reply.State == "complete" && state.SpeechEnd > 0 {
		runes := []rune(reply.Text)
		start, end := min(state.SpeechStart, len(runes)), min(state.SpeechEnd, len(runes))
		state.LastSpeech, state.LastSpeechReceipt, state.LastSpeechState = string(runes[start:end]), reply.Receipt, "complete"
		state.LastSpeechMore = end < len(runes)
		reply.Offset = end
		if end == len(runes) {
			return
		}
	}
	state.Replies = append(state.Replies, reply)
}

// quotaReason stores a usage-limit failure as "<name>[_after_action]:<reset
// date>" in the existing turn Failure string, so a later status replay can
// speak the same reset date without a schema change. A monthly reset that the
// broker did not report is the first day of the next UTC month, which is when
// the monthly counter rolls over.
func quotaReason(qe *QuotaError, now time.Time) string {
	name := "usage_limit"
	if qe.Kind == "monthly_tokens" {
		name = "monthly_limit"
	}
	if qe.ToolRan {
		name += "_after_action"
	}
	reset := qe.ResetAt
	if reset.IsZero() && qe.Kind == "monthly_tokens" {
		n := now.UTC()
		reset = time.Date(n.Year(), n.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	}
	if !reset.IsZero() {
		name += ":" + reset.UTC().Format(time.DateOnly)
	}
	return name
}

func limitSpeech(reason string) string {
	name, date, _ := strings.Cut(reason, ":")
	afterAction := strings.HasSuffix(name, "_after_action")
	scope := map[string]string{"monthly_limit": "monthly usage limit", "usage_limit": "usage limit"}[strings.TrimSuffix(name, "_after_action")]
	if scope == "" {
		return ""
	}
	resets := ""
	if day, err := time.Parse(time.DateOnly, date); err == nil {
		resets = " It resets on " + day.Format("January 2") + "."
	}
	if afterAction {
		return "That request did not finish. Some work may have completed. Check Live Ninja before repeating an action. Your Live Ninja " + scope + " is reached." + resets
	}
	return "Your Live Ninja " + scope + " is reached, so nothing was done." + resets
}

func failureSpeech(reason string) string {
	if speech := limitSpeech(reason); speech != "" {
		return speech
	}
	return "That request did not finish. " + map[string]string{"settings_changed": "Your settings or access changed; start a new request.", "queue_unavailable": "The queue was unavailable; no automatic retry will run.", "worker_interrupted": "The worker was interrupted; check any action before repeating it.", "stopped": "You stopped this conversation.", "execution_failed": "Some work may have completed. Check Live Ninja before repeating an action.", "reply_limit": "Its answer was too large to retain. Check Live Ninja before repeating an action."}[reason]
}
