package alexa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

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
