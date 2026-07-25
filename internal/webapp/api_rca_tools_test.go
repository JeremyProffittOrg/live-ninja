package webapp

// Wiring tests for the M17 tool-failure RCA ingress. Same class of bug as the
// 2026-07-18 "Memory failed" incident pinned by api_memory_tools_test.go, and
// the same shape of guard: template.yaml can set RCA_QUEUE_URL, grant
// sqs:SendMessage and deploy the analyzer, and the whole pipeline still does
// nothing if buildAPIToolsRegistry never populates tools.Deps.RCA. The failure
// is invisible from the outside — tools keep working, the queue just stays
// empty forever — so it has to be pinned by a test rather than noticed.
//
// Three things are nailed down here:
//
//  1. buildAPIToolRCA returns a live enqueuer when the queue is configured
//     (the call that was missing entirely).
//  2. Unconfigured, it returns a TRUE nil interface, not an interface holding
//     a (*rca.SQSEnqueuer)(nil) — the typed-nil trap that would turn every
//     failed tool call into a nil-pointer panic on a diagnostics path.
//  3. End to end through POST /api/v1/tools/invoke: a failed invocation
//     actually lands a decodable tools.ToolFailure on the configured queue,
//     and a successful one lands nothing.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

// fakeSQS records SendMessage calls. Mutex-guarded because the enqueue runs on
// the request path and the test asserts on it after the response returns.
type fakeSQS struct {
	mu   sync.Mutex
	sent []sqs.SendMessageInput
	err  error
}

func (f *fakeSQS) SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.sent = append(f.sent, *params)
	return &sqs.SendMessageOutput{}, nil
}

func (f *fakeSQS) calls() []sqs.SendMessageInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sqs.SendMessageInput(nil), f.sent...)
}

// TestBuildAPIToolRCAWiresSeam is the direct regression: with an SQS client and
// a queue URL, buildAPIToolRCA must produce a non-nil enqueuer so
// buildAPIToolsRegistry sets tools.Deps.RCA. (Construction is offline — no AWS
// credentials involved — so this holds in CI.)
func TestBuildAPIToolRCAWiresSeam(t *testing.T) {
	deps := &Deps{SQS: &fakeSQS{}, SQSRcaURL: "https://sqs.us-east-1.amazonaws.com/1/live-ninja-rca", Log: testLogger()}

	if enq := buildAPIToolRCA(deps); enq == nil {
		t.Fatal("buildAPIToolRCA returned nil with a configured queue — no tool failure would ever be analyzed")
	}
}

// TestBuildAPIToolRCAUnconfiguredIsTrueNil is the typed-nil guard. rca.
// NewSQSEnqueuer returns (*rca.SQSEnqueuer)(nil) when unconfigured; if that
// value is ever assigned straight into the tools.FailureEnqueuer field, the
// interface is non-nil, the hook's nil check passes, and the first failed tool
// call panics. The comparison against nil here only holds if the seam returns
// early instead of returning the typed pointer.
func TestBuildAPIToolRCAUnconfiguredIsTrueNil(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps *Deps
	}{
		{"no queue url", &Deps{SQS: &fakeSQS{}, SQSRcaURL: "", Log: testLogger()}},
		{"no sqs client", &Deps{SQS: nil, SQSRcaURL: "https://example/q", Log: testLogger()}},
		{"neither", &Deps{Log: testLogger()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enq := buildAPIToolRCA(tc.deps)
			if enq != nil {
				t.Fatalf("buildAPIToolRCA returned a non-nil interface (%T) when unconfigured — "+
					"tools.Deps.RCA would be non-nil and the failure hook would nil-panic", enq)
			}
		})
	}
}

// TestBuildAPIToolsRegistryWiresRCA is the test that actually covers the
// missing line. The seam tests above prove buildAPIToolRCA builds an enqueuer;
// they would all still pass if buildAPIToolsRegistry never assigned it to
// tools.Deps.RCA — which was exactly the defect. There is no getter for
// Deps.RCA, so this asserts it behaviourally: build the registry the way
// cold start does, drive a failing invocation through it, and require that a
// message reached the queue.
func TestBuildAPIToolsRegistryWiresRCA(t *testing.T) {
	// LoadDefaultConfig inside buildAPIToolsRegistry needs a region but no
	// credentials — it only constructs SDK clients here.
	t.Setenv("AWS_REGION", "us-east-1")

	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja-test")
	queue := &fakeSQS{}
	const queueURL = "https://sqs.us-east-1.amazonaws.com/1/live-ninja-rca"

	deps := &Deps{
		Store:     st,
		Log:       testLogger(),
		SQS:       queue,
		SQSRcaURL: queueURL,
		Cfg:       config.App{TableName: "live-ninja-test"},
	}

	registry := buildAPIToolsRegistry(deps)
	if registry == nil {
		t.Fatal("buildAPIToolsRegistry returned nil")
	}

	res := registry.Invoke(context.Background(), tools.Invocation{
		Tool:      "definitely_not_a_tool",
		UserID:    "u1",
		SessionID: "sess-1",
		Surface:   "web",
		CallID:    "call-1",
	})
	if res.OK {
		t.Fatalf("expected the unknown tool to fail, got %+v", res)
	}
	if n := len(queue.calls()); n != 1 {
		t.Fatalf("buildAPIToolsRegistry produced %d RCA enqueues, want 1 — "+
			"tools.Deps.RCA is not being wired, so the entire M17 pipeline is dead code in production", n)
	}
}

// TestToolsInvokeEnqueuesRCAEndToEnd drives POST /api/v1/tools/invoke with an
// RCA enqueuer wired exactly as buildAPIToolsRegistry wires it, and asserts a
// failed invocation puts a decodable tools.ToolFailure on the configured queue.
// Before the wiring landed this queue stayed empty no matter what failed.
func TestToolsInvokeEnqueuesRCAEndToEnd(t *testing.T) {
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja-test")
	queue := &fakeSQS{}
	const queueURL = "https://sqs.us-east-1.amazonaws.com/1/live-ninja-rca"

	deps := &Deps{Store: st, Log: testLogger(), SQS: queue, SQSRcaURL: queueURL}
	registry, err := tools.NewRegistry(&tools.Deps{
		Store:       st,
		TableName:   "live-ninja-test",
		Log:         testLogger(),
		Reauthorize: func(ctx context.Context, userID string) error { return nil },
		RCA:         buildAPIToolRCA(deps),
	})
	if err != nil {
		t.Fatalf("tools.NewRegistry: %v", err)
	}

	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		c.Locals(localSessionID, "sess-1")
		c.Locals(localSurface, "web")
		return c.Next()
	})
	app.Post("/api/v1/tools/invoke", handleToolsInvoke(deps, registry))

	// A tool the model invented: unknown_tool is on the RCA allowlist and is
	// rejected before any handler runs, so it needs no tool-specific deps.
	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/tools/invoke", map[string]any{
		"tool":   "definitely_not_a_tool",
		"callId": "call-1",
	})
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected an error status for an unknown tool, got 200 (body %v)", body)
	}

	sent := queue.calls()
	if len(sent) != 1 {
		t.Fatalf("got %d RCA enqueues, want exactly 1 — the failure hook is not wired", len(sent))
	}
	if got := *sent[0].QueueUrl; got != queueURL {
		t.Errorf("enqueued to %q, want %q", got, queueURL)
	}

	var f tools.ToolFailure
	if err := json.Unmarshal([]byte(*sent[0].MessageBody), &f); err != nil {
		t.Fatalf("cmd/rca-analyzer decodes this body with the same type; unmarshal failed: %v", err)
	}
	if f.ErrorCode != tools.CodeUnknownTool {
		t.Errorf("ToolFailure.ErrorCode = %q, want %q", f.ErrorCode, tools.CodeUnknownTool)
	}
	if f.UserID != "u1" || f.SessionID != "sess-1" || f.Surface != "web" {
		t.Errorf("ToolFailure lost request identity: userId=%q sessionId=%q surface=%q", f.UserID, f.SessionID, f.Surface)
	}
	// The raw client-supplied name must not key a DynamoDB partition; the
	// sentinel carries the shape and RequestedTool carries the evidence.
	if f.Tool != "unknown_tool" || f.RequestedTool != "definitely_not_a_tool" {
		t.Errorf("unknown-tool sentinel not applied: tool=%q requestedTool=%q", f.Tool, f.RequestedTool)
	}
	if f.TxID == "" {
		t.Error("ToolFailure.TxID empty — the RCA could not be joined back to the invocation")
	}
}

// TestToolsInvokeDoesNotEnqueueOnSuccess is the other half: the hook must be
// silent for a successful call, or a working system mails the owner an RCA per
// tool call and burns the daily Opus budget on nothing.
func TestToolsInvokeDoesNotEnqueueOnSuccess(t *testing.T) {
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja-test")
	queue := &fakeSQS{}

	deps := &Deps{Store: st, Log: testLogger(), SQS: queue, SQSRcaURL: "https://example/q"}
	registry, err := tools.NewRegistry(&tools.Deps{
		Store:       st,
		DDB:         fake,
		TableName:   "live-ninja-test",
		Log:         testLogger(),
		Reauthorize: func(ctx context.Context, userID string) error { return nil },
		RCA:         buildAPIToolRCA(deps),
	})
	if err != nil {
		t.Fatalf("tools.NewRegistry: %v", err)
	}

	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		c.Locals(localSessionID, "sess-1")
		c.Locals(localSurface, "web")
		return c.Next()
	})
	app.Post("/api/v1/tools/invoke", handleToolsInvoke(deps, registry))

	// recall_note is read-only and needs nothing but the store, so it succeeds
	// against the fake table (returning zero notes).
	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/tools/invoke", map[string]any{
		"tool":   "recall_note",
		"callId": "call-ok",
		"args":   map[string]any{"limit": 5},
	})
	if resp.StatusCode != http.StatusOK || body["ok"] != true {
		t.Fatalf("recall_note failed (%d): %v", resp.StatusCode, body)
	}
	if n := len(queue.calls()); n != 0 {
		t.Fatalf("got %d RCA enqueues for a successful invocation, want 0", n)
	}
}
