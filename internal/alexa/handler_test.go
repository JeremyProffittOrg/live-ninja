package alexa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/stretchr/testify/require"
)

func handlerEvent(now time.Time, intent, query string) Event {
	var e Event
	_ = json.Unmarshal([]byte(`{"version":"1.0","session":{"sessionId":"ask"},"context":{"System":{"application":{"applicationId":"skill"},"device":{"deviceId":"echo"},"user":{"accessToken":"synthetic-linked-token"}}},"request":{"type":"IntentRequest","requestId":"request","locale":"en-US","intent":{"slots":{}}}}`), &e)
	e.Request.Timestamp = now.Format(time.RFC3339)
	e.Request.Intent.Name = intent
	e.Request.Intent.Slots["query"] = struct {
		Value string `json:"value"`
	}{Value: query}
	return e
}

func TestContractHandlerUsesPendingReceiptAndClearsElicitedSlot(t *testing.T) {
	s, id, now, published, ran := serviceFixture(t)
	h := Handler{SkillID: "skill", Service: s, Now: func() time.Time { return *now }, WaitBudget: 20 * time.Millisecond, Resolve: func(context.Context, string, string) (Identity, error) { return id, nil }}
	e := handlerEvent(*now, "ChatIntent", "Which city?")
	response, err := h.Handle(t.Context(), e)
	require.NoError(t, err)
	require.Contains(t, response.Response.OutputSpeech.Text, "status")
	require.False(t, response.Response.ShouldEndSession)
	require.Equal(t, 1, *published)
	require.Zero(t, *ran)
	raw, _ := json.Marshal(response)
	require.NotContains(t, string(raw), "Which city?")
	require.Contains(t, string(raw), "Dialog.ElicitSlot")
	e.Request.ID = "status"
	e.Request.Intent.Name = "StatusIntent"
	_, err = h.Handle(t.Context(), e)
	require.NoError(t, err)
	require.Equal(t, 1, *published)
	e.Request.ID = "stop"
	e.Request.Intent.Name = "ChatIntent"
	e.Request.Intent.Slots["query"] = struct {
		Value string `json:"value"`
	}{Value: "stop"}
	response, err = h.Handle(t.Context(), e)
	require.NoError(t, err)
	require.True(t, response.Response.ShouldEndSession)
	require.Equal(t, 1, *published)
}

func TestContractHandlerRejectsWrongSkillAndOffersAccountLink(t *testing.T) {
	s, _, now, published, _ := serviceFixture(t)
	h := Handler{SkillID: "skill", Service: s, Now: func() time.Time { return *now }, Resolve: func(context.Context, string, string) (Identity, error) { return Identity{}, ErrUnauthorized }}
	e := handlerEvent(*now, "ChatIntent", "hello")
	response, err := h.Handle(t.Context(), e)
	require.NoError(t, err)
	require.Equal(t, "LinkAccount", response.Response.Card.Type)
	require.Zero(t, *published)
	e.Context.System.Application.ID = "foreign"
	_, err = h.Handle(t.Context(), e)
	require.ErrorIs(t, err, ErrEnvelope)
	require.Zero(t, *published)
}

func TestAlexaDiagnosticsTraceRequestsWithoutPrivateContent(t *testing.T) {
	for _, mode := range []string{"launch", "invalid", "identity-failure"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			s, id, now, _, _ := serviceFixture(t)
			h := Handler{Log: slog.New(slog.NewJSONHandler(&logs, nil)), SkillID: "skill", Service: s, Now: func() time.Time { return *now },
				Resolve: func(context.Context, string, string) (Identity, error) {
					if mode == "identity-failure" {
						return Identity{}, errors.New("Bearer PRIVATE-PROVIDER-CREDENTIAL")
					}
					return id, nil
				}}
			e := handlerEvent(*now, "ChatIntent", "PRIVATE-USER-UTTERANCE")
			e.Request.Type = "LaunchRequest"
			e.Context.System.Device.ID = "PRIVATE-PROVIDER-DEVICE"
			e.Session.ID = "PRIVATE-ASK-SESSION"
			if mode == "invalid" {
				e.Request.Type = "PRIVATE-UNKNOWN-REQUEST"
			}
			_, err := h.Handle(t.Context(), e)
			if mode == "invalid" {
				require.ErrorIs(t, err, ErrEnvelope)
			} else {
				require.NoError(t, err)
			}
			out := logs.String()
			for _, stage := range []string{"request_received", "response_completed"} {
				require.Contains(t, out, stage)
			}
			switch mode {
			case "launch":
				require.Contains(t, out, "identity_resolved")
				require.Contains(t, out, "operation_started")
			case "invalid":
				require.Contains(t, out, "invalid_envelope")
			case "identity-failure":
				require.Contains(t, out, "identity_failed")
				require.Contains(t, out, "dependency_failed")
			}
			for _, private := range []string{"PRIVATE-", "synthetic-linked-token", `"accessToken"`, `"outputSpeech"`} {
				require.NotContains(t, out, private)
			}
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				var record map[string]any
				require.NoError(t, json.Unmarshal([]byte(line), &record))
				require.Len(t, record["requestRef"].(string), 64)
			}
		})
	}
}

func TestContractSpeechIsPlainBoundedAndDoesNotRepeatModelWork(t *testing.T) {
	s, id, _, published, ran := serviceFixture(t)
	ctx := t.Context()
	view, err := s.Submit(ctx, id, "one", "session", "answer")
	require.NoError(t, err)
	text := strings.Repeat("A safe sentence. ", 100)
	s.Run = func(_ context.Context, w Work) (RunResult, error) {
		r := RunResult{Text: text, Messages: w.Messages}
		r.Messages = append(r.Messages, chatAnswer(text))
		return r, nil
	}
	require.NoError(t, s.Execute(ctx, Job{Owner: id.Owner, Turn: view.Receipt}))
	first, err := s.Speech(ctx, id, "status")
	require.NoError(t, err)
	require.LessOrEqual(t, len([]rune(first.Text)), 600)
	require.True(t, first.More)
	repeated, err := s.Speech(ctx, id, "repeat")
	require.NoError(t, err)
	require.Equal(t, first.Text, repeated.Text)
	more, err := s.Speech(ctx, id, "more")
	require.NoError(t, err)
	require.LessOrEqual(t, len([]rune(more.Text)), 600)
	require.Equal(t, 1, *published)
	require.Zero(t, *ran)
	speech := safeSpeech("<speak>Hi</speak> https://example.com/long/path Bearer token12345678901234567890 ```secret code``` ")
	require.NotContains(t, speech, "<")
	require.NotContains(t, speech, "https:")
	require.NotContains(t, speech, "token123")
	require.NotContains(t, speech, "secret code")
}

func TestAlexaDeliversThreeCompletedRequestsInOrder(t *testing.T) {
	s, id, _, published, _ := serviceFixture(t)
	ctx := t.Context()
	executions := 0
	answers := []string{"Office PC summary.", "World news summary.", "AI news summary."}
	s.Run = func(_ context.Context, w Work) (RunResult, error) {
		answer := answers[executions]
		executions++
		messages := append([]realtime.ChatMessage{}, w.Messages...)
		for index := 0; index < 2; index++ {
			callID := fmt.Sprintf("call_%d_%d", executions, index)
			messages = append(messages, realtime.ChatMessage{Role: "assistant", ToolCalls: []realtime.ChatToolCall{{ID: callID, Name: "web_research", Arguments: "{}"}}}, realtime.ChatMessage{Role: "tool", ToolCallID: callID, Content: "Lookup completed."})
		}
		return RunResult{Text: answer, Messages: append(messages, chatAnswer(answer))}, nil
	}
	for _, request := range []string{"office", "world", "ai"} {
		v, err := s.Submit(ctx, id, request, "session", request)
		require.NoError(t, err)
		require.NoError(t, s.Execute(ctx, Job{Owner: id.Owner, Turn: v.Receipt}))
	}
	var stored session
	_, err := s.read(ctx, id.Owner, sessionKey(id), &stored)
	require.NoError(t, err)
	require.Len(t, stored.History, 2, "the first answer must survive model-context trimming")
	for _, expected := range answers {
		v, err := s.Speech(ctx, id, "status")
		require.NoError(t, err)
		require.Equal(t, expected, v.Text)
		repeated, err := s.Speech(ctx, id, "repeat")
		require.NoError(t, err)
		require.Equal(t, expected, repeated.Text)
	}
	require.Equal(t, 3, executions)
	require.Equal(t, 3, *published)
	empty, err := s.Speech(ctx, id, "more")
	require.NoError(t, err)
	require.Equal(t, "ready", empty.State)
}

func TestAlexaQueueBoundAndStopClearAllAnswers(t *testing.T) {
	s, id, _, published, _ := serviceFixture(t)
	s.Run = func(_ context.Context, w Work) (RunResult, error) {
		return RunResult{Text: "PRIVATE-QUEUED-ANSWER", Messages: append(w.Messages, chatAnswer("PRIVATE-QUEUED-ANSWER"))}, nil
	}
	for index := 0; index < MaxPendingReplies; index++ {
		v, err := s.Submit(t.Context(), id, fmt.Sprint(index), "session", "PRIVATE-INPUT")
		require.NoError(t, err)
		require.NoError(t, s.Execute(t.Context(), Job{Owner: id.Owner, Turn: v.Receipt}))
	}
	_, err := s.Submit(t.Context(), id, "too-many", "session", "must not run")
	require.ErrorIs(t, err, ErrReplyQueueFull)
	require.Equal(t, MaxPendingReplies, *published)
	_, err = s.Speech(t.Context(), id, "status")
	require.NoError(t, err)
	require.NoError(t, s.Reset(t.Context(), id, "fresh", true))
	entry, err := s.Repo.Read(t.Context(), id.Owner, sessionKey(id))
	require.NoError(t, err)
	require.NotContains(t, string(entry.Data), "PRIVATE-")
	view, err := s.Speech(t.Context(), id, "repeat")
	require.NoError(t, err)
	require.Equal(t, "ready", view.State)
}

type speechRaceRepository struct {
	Repository
	hook func(context.Context, []Change)
}

func (r *speechRaceRepository) Commit(ctx context.Context, changes []Change) error {
	if r.hook != nil {
		r.hook(ctx, changes)
	}
	return r.Repository.Commit(ctx, changes)
}

func TestAlexaSpeechAdvanceCannotLoseConcurrentWorkerResult(t *testing.T) {
	s, id, _, _, _ := serviceFixture(t)
	executions := 0
	s.Run = func(_ context.Context, w Work) (RunResult, error) {
		executions++
		text := fmt.Sprintf("Answer %d", executions)
		return RunResult{Text: text, Messages: append(w.Messages, chatAnswer(text))}, nil
	}
	first, err := s.Submit(t.Context(), id, "one", "session", "first")
	require.NoError(t, err)
	require.NoError(t, s.Execute(t.Context(), Job{Owner: id.Owner, Turn: first.Receipt}))
	second, err := s.Submit(t.Context(), id, "two", "session", "second")
	require.NoError(t, err)
	race := &speechRaceRepository{Repository: s.Repo}
	s.Repo = race
	interleaved := false
	race.hook = func(ctx context.Context, changes []Change) {
		if interleaved {
			return
		}
		for _, change := range changes {
			if change.After == nil || change.After.Key != turnKey(second.Receipt) {
				continue
			}
			var saved turn
			require.NoError(t, json.Unmarshal(change.After.Data, &saved))
			if saved.State != "complete" {
				continue
			}
			interleaved = true
			spoken, e := s.Speech(ctx, id, "status")
			require.NoError(t, e)
			require.Equal(t, "Answer 1", spoken.Text)
		}
	}
	require.NoError(t, s.Execute(t.Context(), Job{Owner: id.Owner, Turn: second.Receipt}))
	require.True(t, interleaved)
	spoken, err := s.Speech(t.Context(), id, "more")
	require.NoError(t, err)
	require.Equal(t, "Answer 2", spoken.Text)
	require.Equal(t, 2, executions)
}

func TestAlexaNewRequestPreservesPartlySpokenAnswer(t *testing.T) {
	s, id, _, _, _ := serviceFixture(t)
	count := 0
	long := strings.Repeat("First answer sentence. ", 40)
	s.Run = func(_ context.Context, w Work) (RunResult, error) {
		count++
		text := long
		if count == 2 {
			text = "Second answer."
		}
		return RunResult{Text: text, Messages: append(w.Messages, chatAnswer(text))}, nil
	}
	first, err := s.Submit(t.Context(), id, "one", "session", "first")
	require.NoError(t, err)
	require.NoError(t, s.Execute(t.Context(), Job{Owner: id.Owner, Turn: first.Receipt}))
	part, err := s.Speech(t.Context(), id, "status")
	require.NoError(t, err)
	require.True(t, part.More)
	second, err := s.Submit(t.Context(), id, "two", "session", "second")
	require.NoError(t, err)
	require.NoError(t, s.Execute(t.Context(), Job{Owner: id.Owner, Turn: second.Receipt}))
	repeated, err := s.Speech(t.Context(), id, "repeat")
	require.NoError(t, err)
	require.Equal(t, part.Text, repeated.Text)
	rest, err := s.Speech(t.Context(), id, "more")
	require.NoError(t, err)
	require.Equal(t, first.Receipt, rest.Receipt)
	require.False(t, rest.More)
	require.Equal(t, safeSpeech(long), part.Text+" "+rest.Text)
	require.Equal(t, 1, rest.PendingReplies)
	require.LessOrEqual(t, len([]rune(viewResponse(rest).Response.OutputSpeech.Text)), 600)
	last, err := s.Speech(t.Context(), id, "more")
	require.NoError(t, err)
	require.Equal(t, "Second answer.", last.Text)
	require.Equal(t, 2, count)
}

func TestAlexaAnswerQueueBoundsStoredTextAndUpgradesLegacySession(t *testing.T) {
	t.Run("bounded text", func(t *testing.T) {
		s, id, _, _, _ := serviceFixture(t)
		s.Run = func(_ context.Context, w Work) (RunResult, error) {
			text := strings.Repeat("a", MaxContextBytes-500)
			return RunResult{Text: text, Messages: append(w.Messages, chatAnswer(text))}, nil
		}
		for index := 0; index < MaxPendingReplies; index++ {
			v, err := s.Submit(t.Context(), id, fmt.Sprint(index), "session", "question")
			require.NoError(t, err)
			require.NoError(t, s.Execute(t.Context(), Job{Owner: id.Owner, Turn: v.Receipt}))
		}
		var saved session
		_, err := s.read(t.Context(), id.Owner, sessionKey(id), &saved)
		require.NoError(t, err)
		encoded, err := json.Marshal(saved.Replies)
		require.NoError(t, err)
		require.LessOrEqual(t, len(encoded), maxReplyQueueBytes)
		require.Len(t, saved.Replies, MaxPendingReplies)
		require.Equal(t, "complete", saved.Replies[0].State)
		require.Equal(t, "failed", saved.Replies[len(saved.Replies)-1].State)
		require.Contains(t, saved.Replies[len(saved.Replies)-1].Text, "too large")
	})
	t.Run("legacy context", func(t *testing.T) {
		s, id, _, _, ran := serviceFixture(t)
		v, err := s.Submit(t.Context(), id, "legacy", "session", "question")
		require.NoError(t, err)
		require.NoError(t, s.Execute(t.Context(), Job{Owner: id.Owner, Turn: v.Receipt}))
		var saved session
		row, err := s.read(t.Context(), id.Owner, sessionKey(id), &saved)
		require.NoError(t, err)
		saved.Replies = nil // Stored by the previously deployed implementation.
		require.NoError(t, s.Repo.Commit(t.Context(), []Change{changed(id.Owner, sessionKey(id), saved, row.Expires, row)}))
		spoken, err := s.Speech(t.Context(), id, "status")
		require.NoError(t, err)
		require.Equal(t, "Richmond", spoken.Text)
		require.Equal(t, 1, *ran)
	})
}
