package webapp

// handleTranscript's final-flush cost seam (Task #7): the client's
// per-session cost estimate rides the topics-extract event after
// sanitization, and garbage figures are dropped rather than persisted.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

type captureLambda struct {
	inputs []*lambda.InvokeInput
}

func (c *captureLambda) Invoke(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	c.inputs = append(c.inputs, params)
	return &lambda.InvokeOutput{}, nil
}

func newTranscriptApp(t *testing.T) (*fiber.App, *captureLambda) {
	t.Helper()
	t.Setenv("TOPICS_EXTRACT_FUNCTION_NAME", "topics-extract-test")
	st := store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja")
	capture := &captureLambda{}
	deps := &Deps{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Lambda: capture}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		c.Locals(localSurface, "web")
		return c.Next()
	})
	app.Post("/api/v1/transcript", handleTranscript(deps))
	return app, capture
}

func extractEventPayload(t *testing.T, capture *captureLambda, i int) map[string]any {
	t.Helper()
	if len(capture.inputs) <= i {
		t.Fatalf("expected at least %d extractor invokes, got %d", i+1, len(capture.inputs))
	}
	var payload map[string]any
	if err := json.Unmarshal(capture.inputs[i].Payload, &payload); err != nil {
		t.Fatalf("decode extractor payload: %v", err)
	}
	return payload
}

func TestTranscriptFinalForwardsCost(t *testing.T) {
	app, capture := newTranscriptApp(t)

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/transcript", map[string]any{
		"sessionId": "sessC",
		"final":     true,
		"cost":      map[string]any{"usd": 0.123, "textTokens": 500, "audioTokens": 1500},
		"turns": []map[string]any{
			{"seq": 0, "role": "user", "text": "hi", "engine": "openai-realtime"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcript status = %d (%v)", resp.StatusCode, body)
	}

	payload := extractEventPayload(t, capture, 0)
	if payload["sessionId"] != "sessC" || payload["userId"] != "u1" {
		t.Errorf("extractor identity = %v", payload)
	}
	if usd, _ := payload["costUsd"].(float64); usd != 0.123 {
		t.Errorf("costUsd = %v, want 0.123", payload["costUsd"])
	}
	if tok, _ := payload["costTextTokens"].(float64); tok != 500 {
		t.Errorf("costTextTokens = %v, want 500", payload["costTextTokens"])
	}
	if tok, _ := payload["costAudioTokens"].(float64); tok != 1500 {
		t.Errorf("costAudioTokens = %v, want 1500", payload["costAudioTokens"])
	}

	// A garbage estimate (negative) is dropped — the event carries no cost.
	resp, _ = doJSON(t, app, http.MethodPost, "/api/v1/transcript", map[string]any{
		"sessionId": "sessD",
		"final":     true,
		"cost":      map[string]any{"usd": -5, "textTokens": 1, "audioTokens": 1},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("garbage-cost transcript status = %d", resp.StatusCode)
	}
	payload = extractEventPayload(t, capture, 1)
	if _, present := payload["costUsd"]; present {
		t.Errorf("negative cost must not reach the extractor, got %v", payload["costUsd"])
	}
}

func TestSanitizeSessionCost(t *testing.T) {
	cases := []struct {
		name        string
		usd         float64
		text, audio int
		wantZero    bool
	}{
		{"valid", 0.5, 100, 200, false},
		{"zero is fine", 0, 0, 0, false},
		{"negative usd", -0.1, 0, 0, true},
		{"absurd usd", 5000, 0, 0, true},
		{"nan", math.NaN(), 0, 0, true},
		{"inf", math.Inf(1), 0, 0, true},
		{"negative tokens", 0.1, -1, 0, true},
		{"absurd tokens", 0.1, 0, 2e9, true},
	}
	for _, tc := range cases {
		got := sanitizeSessionCost(tc.usd, tc.text, tc.audio)
		if tc.wantZero && got != (sessionCost{}) {
			t.Errorf("%s: expected rejection, got %+v", tc.name, got)
		}
		if !tc.wantZero && (got.USD != tc.usd || got.TextTokens != tc.text || got.AudioTokens != tc.audio) {
			t.Errorf("%s: mangled cost %+v", tc.name, got)
		}
	}
}
