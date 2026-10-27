package webapp

// Route-level tests for the tool-capable fallback turn
// (handleFallbackTurn): the web-side loop that invokes the broker's
// "fallback-turn" mode, executes returned tool_calls through the SAME
// internal/tools registry pipeline as POST /api/v1/tools/invoke
// (re-authorization included), feeds the results back, and caps at
// maxFallbackToolIterations before degrading to a plain answer.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/alexa"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

// fakeBrokerLambda plays the realtime-broker Lambda: it records every
// decoded brokerRequest and answers from a script of response payloads
// (the last entry repeats once the script runs out).
type fakeBrokerLambda struct {
	responses   []map[string]any
	requests    []brokerRequest
	payloads    []map[string]any // decoded req.Payload per call
	lastOptions lambda.Options
}

func (f *fakeBrokerLambda) Invoke(_ context.Context, in *lambda.InvokeInput, options ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	for _, option := range options {
		option(&f.lastOptions)
	}
	var req brokerRequest
	if err := json.Unmarshal(in.Payload, &req); err != nil {
		return nil, err
	}
	f.requests = append(f.requests, req)
	var p map[string]any
	if len(req.Payload) > 0 {
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return nil, err
		}
	}
	f.payloads = append(f.payloads, p)

	idx := len(f.requests) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	out, err := json.Marshal(f.responses[idx])
	if err != nil {
		return nil, err
	}
	return &lambda.InvokeOutput{Payload: out}, nil
}

// newFallbackTurnApp mounts only the fallback-turn route over a
// FakeDynamo-backed store and a real tools.Registry, with auth locals
// pre-populated the way ExtractAuthContext would.
func newFallbackTurnApp(t *testing.T, fake *fakeBrokerLambda, reauth tools.ReauthorizeFunc) *fiber.App {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dyn := testutil.NewFakeDynamo()
	st := store.NewWithClient(dyn, "live-ninja-test")
	deps := &Deps{Store: st, Log: log, BrokerFn: "realtime-broker", Lambda: fake}

	registry, err := tools.NewRegistry(&tools.Deps{
		Store:       st,
		DDB:         dyn,
		TableName:   "live-ninja-test",
		Log:         log,
		Reauthorize: reauth,
		Now:         time.Now,
	})
	if err != nil {
		t.Fatalf("build tools registry: %v", err)
	}

	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		c.Locals(localSessionID, "sess-1")
		c.Locals(localSurface, "web")
		return c.Next()
	})
	app.Post("/api/v1/fallback/turn", handleFallbackTurn(deps, registry))
	return app
}

func allowAll(context.Context, string) error { return nil }

func TestAlexaRunnerUsesSharedLoopAndPrivateToolContext(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	db := testutil.NewFakeDynamo()
	st := store.NewWithClient(db, "test")
	fake := &fakeBrokerLambda{responses: []map[string]any{
		{"toolCalls": []map[string]any{{"id": "c1", "name": "remember_note", "arguments": `{"text":"I prefer tea"}`}}},
		{"text": "I saved your note."},
	}}
	deps := &Deps{Store: st, Log: log, Lambda: fake, BrokerFn: "broker"}
	registry, err := tools.NewRegistry(&tools.Deps{Store: st, DDB: db, TableName: "test", Log: log, Reauthorize: allowAll, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	w := alexa.Work{Identity: alexa.Identity{Owner: "u1", Device: "echo"}, Receipt: "receipt", SessionID: "session", Messages: []realtime.ChatMessage{{Role: "user", Content: "Remember my tea preference"}}, Check: func(context.Context) error { return nil }}
	r, err := runAlexaTurn(t.Context(), deps, registry, w)
	if err != nil || r.Text != "I saved your note." || len(r.Messages) != 4 {
		t.Fatalf("result=%+v err=%v", r, err)
	}
	if len(fake.requests) != 2 || fake.requests[1].AlexaRound != 1 || fake.requests[0].AlexaTurn != "receipt" || fake.requests[0].DeviceID != "" || fake.requests[0].Surface != "alexa" {
		t.Fatalf("bad authority: %+v", fake.requests)
	}
	if fake.lastOptions.RetryMaxAttempts != 1 {
		t.Fatal("ambiguous model invocation may retry")
	}
	rows, err := st.QuerySessionLog(t.Context(), "u1", "session")
	if err != nil || len(rows) != 0 {
		t.Fatal("privacy-off wrote transcript", err)
	}
	notes, err := st.QueryNotes(t.Context(), "u1", "tea")
	if err != nil || len(notes) != 1 {
		t.Fatal("explicit note was not saved", err)
	}
	toolLines := func() string {
		var out []string
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "Alexa tool") {
				out = append(out, line)
			}
		}
		return strings.Join(out, "\n")
	}
	if got := toolLines(); !strings.Contains(got, "Alexa tool completed") || !strings.Contains(got, "tool=remember_note") || !strings.Contains(got, "ok=true") || strings.Contains(got, "tea") {
		t.Fatalf("tool outcome log missing or leaked arguments: %q", got)
	}
	t.Setenv("ALEXA_TRIAL_MODE", "true")
	w.Receipt = "trial-receipt"
	fake.requests, fake.payloads = nil, nil
	fake.responses = []map[string]any{
		{"toolCalls": []map[string]any{{"id": "trial-call", "name": "remember_note", "arguments": `{"text":"tea trial note must not be saved"}`}}},
		{"text": "Tool actions are disabled during the setup trial."},
	}
	_, err = runAlexaTurn(t.Context(), deps, registry, w)
	if err != nil {
		t.Fatal(err)
	}
	notes, err = st.QueryNotes(t.Context(), "u1", "tea")
	if err != nil || len(notes) != 1 {
		t.Fatal("acceptance trial executed a tool action", err)
	}
	if got := toolLines(); !strings.Contains(got, "Alexa tool failed") || !strings.Contains(got, "errorCode=forbidden") || strings.Contains(got, "must not be saved") {
		t.Fatalf("rejected tool call was not logged by code only: %q", got)
	}
	w.Check = func(context.Context) error { return alexa.ErrCancelled }
	_, err = runAlexaTurn(t.Context(), deps, registry, w)
	if err == nil || len(fake.requests) != 2 {
		t.Fatal("cancelled turn called model")
	}
}

// A 402 monthly-limit rejection must reach the Alexa service as a typed
// quota error with its kind and reset time, not a generic broker failure,
// and must say whether a tool already ran earlier in the same turn.
func TestAlexaRunnerCarriesQuotaRejection(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testutil.NewFakeDynamo()
	st := store.NewWithClient(db, "test")
	quota := map[string]any{"error": "quota_exceeded", "code": 402, "kind": "monthly_tokens",
		"message": "Monthly usage limit reached. Resets at 2026-11-01T00:00:00Z.", "used": 1200000, "limit": 1000000, "resetAt": "2026-11-01T00:00:00Z"}
	registry, err := tools.NewRegistry(&tools.Deps{Store: st, DDB: db, TableName: "test", Log: log, Reauthorize: allowAll, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	w := alexa.Work{Identity: alexa.Identity{Owner: "u1", Device: "echo"}, Receipt: "receipt", SessionID: "session", Messages: []realtime.ChatMessage{{Role: "user", Content: "Remember my tea preference"}}, Check: func(context.Context) error { return nil }}
	for _, test := range []struct {
		name      string
		responses []map[string]any
		toolRan   bool
	}{
		{"first round", []map[string]any{quota}, false},
		{"after a tool", []map[string]any{{"toolCalls": []map[string]any{{"id": "c1", "name": "remember_note", "arguments": `{"text":"I prefer tea"}`}}}, quota}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeBrokerLambda{responses: test.responses}
			deps := &Deps{Store: st, Log: log, Lambda: fake, BrokerFn: "broker"}
			_, err := runAlexaTurn(t.Context(), deps, registry, w)
			var qe *alexa.QuotaError
			if !errors.As(err, &qe) {
				t.Fatalf("err = %v, want *alexa.QuotaError", err)
			}
			if qe.Kind != "monthly_tokens" || !qe.ResetAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || qe.ToolRan != test.toolRan {
				t.Fatalf("quota error = %+v", qe)
			}
			if len(fake.requests) != len(test.responses) {
				t.Fatalf("model invocations = %d, want %d (no retry)", len(fake.requests), len(test.responses))
			}
		})
	}
	// Any other broker refusal stays a generic failure; it is never reported
	// as a usage limit.
	fake := &fakeBrokerLambda{responses: []map[string]any{{"error": "upstream_unavailable", "code": 502}}}
	_, err = runAlexaTurn(t.Context(), &Deps{Store: st, Log: log, Lambda: fake, BrokerFn: "broker"}, registry, w)
	var qe *alexa.QuotaError
	if err == nil || errors.As(err, &qe) {
		t.Fatalf("non-quota broker failure err = %v, want a generic error", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("model invocations = %d, want 1 (no retry)", len(fake.requests))
	}
}

func TestWebResearchAdapterUsesVerifiedContextAndOneAttempt(t *testing.T) {
	fake := &fakeBrokerLambda{responses: []map[string]any{{"search": map[string]any{"summary": "Cited evidence", "sources": []map[string]string{{"title": "News", "url": "https://news.example/report"}}}}}}
	search := webResearchThroughBroker(&Deps{Lambda: fake, BrokerFn: "realtime-broker"})
	result, terr := search(t.Context(), tools.Invocation{UserID: "owner", Role: store.RoleOwner, Surface: "phone", TxID: "request-ref"}, "bank robbery November 2025", 0)
	if terr != nil {
		t.Fatal(terr)
	}
	if result == nil || len(result.Sources) != 1 {
		t.Fatal("missing citation")
	}
	if len(fake.requests) != 1 || fake.requests[0].Mode != "web-search" || fake.requests[0].UserID != "owner" || fake.requests[0].Surface != "phone" || fake.requests[0].Role != store.RoleOwner || fake.requests[0].TxID != "request-ref" {
		t.Fatal("search lost verified context")
	}
	if fake.payloads[0]["query"] != "bank robbery November 2025" || fake.payloads[0]["days"] != float64(0) {
		t.Fatal("query or dates changed")
	}
	if fake.lastOptions.Retryer == nil || fake.lastOptions.Retryer.MaxAttempts() != 1 {
		t.Fatal("paid search invocation can retry")
	}
	_, terr = search(t.Context(), tools.Invocation{UserID: "owner", Role: store.RoleOwner, Surface: store.SurfaceAlexa, TxID: "alexa-receipt"}, "same public question", 0)
	if terr != nil || fake.requests[1].AlexaTurn != "alexa-receipt" {
		t.Fatal("Alexa search lost its admission receipt", terr)
	}
}

func respToolCalls(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["toolCalls"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, _ := it.(map[string]any)
		out = append(out, m)
	}
	return out
}

func TestFallbackTurnPlainAnswer(t *testing.T) {
	fake := &fakeBrokerLambda{responses: []map[string]any{{"text": "Hello!"}}}
	app := newFallbackTurnApp(t, fake, allowAll)

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/fallback/turn", map[string]any{"text": "hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%v)", resp.StatusCode, body)
	}
	if body["text"] != "Hello!" {
		t.Errorf("text = %v, want Hello!", body["text"])
	}
	if calls := respToolCalls(t, body); len(calls) != 0 {
		t.Errorf("toolCalls = %v, want empty", calls)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("broker invokes = %d, want 1", len(fake.requests))
	}
	if fake.requests[0].Mode != "fallback-turn" || fake.requests[0].UserID != "u1" {
		t.Errorf("broker request = %+v", fake.requests[0])
	}
	// The web fn always speaks messages mode (tool-capable) to the broker.
	msgs, _ := fake.payloads[0]["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("payload messages = %v, want 1 user message", fake.payloads[0])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "hi" {
		t.Errorf("payload message[0] = %v", first)
	}
}

func TestFallbackTurnExecutesToolAndReinvokes(t *testing.T) {
	fake := &fakeBrokerLambda{responses: []map[string]any{
		{"toolCalls": []map[string]any{{
			"id": "call_1", "name": "remember_note", "arguments": `{"text":"buy milk"}`,
		}}},
		{"text": "Saved your note."},
	}}
	app := newFallbackTurnApp(t, fake, allowAll)

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/fallback/turn",
		map[string]any{"text": "remember to buy milk"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%v)", resp.StatusCode, body)
	}
	if body["text"] != "Saved your note." {
		t.Errorf("text = %v", body["text"])
	}

	// The executed call is surfaced for the UI's tool cards.
	calls := respToolCalls(t, body)
	if len(calls) != 1 {
		t.Fatalf("toolCalls = %v, want 1", calls)
	}
	if calls[0]["tool"] != "remember_note" || calls[0]["ok"] != true {
		t.Errorf("toolCalls[0] = %v", calls[0])
	}
	output, _ := calls[0]["output"].(map[string]any)
	if output["status"] != "saved" || output["noteId"] == "" {
		t.Errorf("toolCalls[0].output = %v", output)
	}

	// Loop mechanics: two broker invokes; the second carries the full
	// conversation — user, assistant tool request, tool result (ok:true).
	if len(fake.requests) != 2 {
		t.Fatalf("broker invokes = %d, want 2", len(fake.requests))
	}
	msgs, _ := fake.payloads[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("second payload messages = %d, want 3 (%v)", len(msgs), fake.payloads[1])
	}
	asst, _ := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Errorf("messages[1] = %v, want assistant", asst)
	}
	tcs, _ := asst["toolCalls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant toolCalls = %v", asst)
	}
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["toolCallId"] != "call_1" {
		t.Errorf("messages[2] = %v", toolMsg)
	}
	var fedResult map[string]any
	if err := json.Unmarshal([]byte(toolMsg["content"].(string)), &fedResult); err != nil {
		t.Fatalf("tool message content is not JSON: %v", err)
	}
	if fedResult["ok"] != true || fedResult["tool"] != "remember_note" {
		t.Errorf("fed-back result = %v", fedResult)
	}
}

func TestFallbackTurnCapsIterations(t *testing.T) {
	// The model keeps asking for tools forever; the loop must stop at the
	// cap and degrade to the tool-limit answer.
	fake := &fakeBrokerLambda{responses: []map[string]any{
		{"toolCalls": []map[string]any{{
			"id": "call_loop", "name": "recall_note", "arguments": `{}`,
		}}},
	}}
	app := newFallbackTurnApp(t, fake, allowAll)

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/fallback/turn", map[string]any{"text": "loop"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%v)", resp.StatusCode, body)
	}
	if len(fake.requests) != maxFallbackToolIterations {
		t.Errorf("broker invokes = %d, want %d", len(fake.requests), maxFallbackToolIterations)
	}
	if body["toolLimitReached"] != true {
		t.Errorf("toolLimitReached = %v, want true", body["toolLimitReached"])
	}
	if body["text"] != fallbackToolLimitText {
		t.Errorf("text = %v", body["text"])
	}
	if calls := respToolCalls(t, body); len(calls) != maxFallbackToolIterations {
		t.Errorf("executed toolCalls = %d, want %d", len(calls), maxFallbackToolIterations)
	}
}

func TestFallbackTurnRegistryAuthzStillEnforced(t *testing.T) {
	// Re-authorization denies → the registry refuses the call (forbidden),
	// the failure is fed back to the model, and the loop still completes.
	fake := &fakeBrokerLambda{responses: []map[string]any{
		{"toolCalls": []map[string]any{{
			"id": "call_1", "name": "remember_note", "arguments": `{"text":"secret"}`,
		}}},
		{"text": "I couldn't save that."},
	}}
	deny := func(context.Context, string) error { return errors.New("revoked") }
	app := newFallbackTurnApp(t, fake, deny)

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/fallback/turn", map[string]any{"text": "remember this"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%v)", resp.StatusCode, body)
	}
	calls := respToolCalls(t, body)
	if len(calls) != 1 {
		t.Fatalf("toolCalls = %v, want 1", calls)
	}
	if calls[0]["ok"] == true {
		t.Fatalf("tool call succeeded despite denied re-authorization: %v", calls[0])
	}
	errObj, _ := calls[0]["error"].(map[string]any)
	if errObj["code"] != tools.CodeForbidden {
		t.Errorf("error code = %v, want %s", errObj["code"], tools.CodeForbidden)
	}
	// The model was told, too.
	msgs, _ := fake.payloads[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("second payload messages = %d, want 3", len(msgs))
	}
	toolMsg, _ := msgs[2].(map[string]any)
	var fedResult map[string]any
	if err := json.Unmarshal([]byte(toolMsg["content"].(string)), &fedResult); err != nil {
		t.Fatalf("tool message content is not JSON: %v", err)
	}
	fedErr, _ := fedResult["error"].(map[string]any)
	if fedErr["code"] != tools.CodeForbidden {
		t.Errorf("fed-back error = %v", fedResult)
	}
}

func TestFallbackTurnBrokerErrorMidLoop(t *testing.T) {
	// A broker-side quota rejection surfaces as the standard 402 envelope
	// even when it happens on a re-invoke inside the tool loop.
	fake := &fakeBrokerLambda{responses: []map[string]any{
		{"toolCalls": []map[string]any{{
			"id": "call_1", "name": "recall_note", "arguments": `{}`,
		}}},
		{"error": "quota_exceeded", "code": 402, "kind": "monthly_tokens",
			"message": "Monthly usage limit reached."},
	}}
	app := newFallbackTurnApp(t, fake, allowAll)

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/fallback/turn", map[string]any{"text": "hi"})
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d (%v), want 402", resp.StatusCode, body)
	}
	env, _ := body["error"].(map[string]any)
	if env["code"] != "quota_exceeded" {
		t.Errorf("error envelope = %v", body)
	}
	if body["kind"] != "monthly_tokens" {
		t.Errorf("kind = %v", body["kind"])
	}
}

func TestFallbackTurnRequiresText(t *testing.T) {
	fake := &fakeBrokerLambda{responses: []map[string]any{{"text": "unused"}}}
	app := newFallbackTurnApp(t, fake, allowAll)

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/fallback/turn", map[string]any{"text": "   "})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d (%v), want 400", resp.StatusCode, body)
	}
	if len(fake.requests) != 0 {
		t.Errorf("broker invoked %d times on invalid input", len(fake.requests))
	}
}

func TestSharedTextTurnPreservesFollowupAndChecksCancellationBeforeTools(t *testing.T) {
	history := []brokerChatMessage{{Role: "user", Content: "Remember the city is Richmond."}, {Role: "assistant", Content: "Richmond."}, {Role: "user", Content: "What is its weather?"}}
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	r, err := runTextTurn(ctx, history, func(_ context.Context, m []brokerChatMessage) (*brokerResponse, error) {
		if len(m) != 3 || m[0].Content != "Remember the city is Richmond." {
			t.Fatal("lost conversation")
		}
		cancel()
		return &brokerResponse{ToolCalls: []brokerChatToolCall{{ID: "weather", Name: "weather", Arguments: `{}`}}}, nil
	}, func(context.Context, brokerChatToolCall, int, int) *tools.Result {
		calls++
		return &tools.Result{OK: true}
	})
	if !errors.Is(err, context.Canceled) || calls != 0 || r == nil {
		t.Fatalf("cancellation: calls=%d err=%v", calls, err)
	}
}
