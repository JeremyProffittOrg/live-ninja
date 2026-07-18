package webapp

// Regression tests for the 2026-07-18 "Memory failed" prod incident: the
// tool registry built by buildAPIToolsRegistry never wired
// tools.Deps.Memory, so every voice-session memory tool (memory_write,
// memory_search, entity_get, plan_upsert, forget) answered
// not_configured — while IAM, Bedrock model access, and the REST
// /api/v1/memory* surface were all healthy. These tests pin the wiring:
//
//   1. buildAPIToolMemory must return a non-nil MemoryService seam
//      whenever a store is present (the exact call that was missing).
//   2. The full /api/v1/tools/invoke route must execute memory_write
//      successfully (ENT# + EMB# persisted) when Memory is wired the
//      way buildAPIToolsRegistry now wires it.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/memory"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBuildAPIToolMemoryWiresSeam is the direct regression: with a live
// store, buildAPIToolMemory must produce a non-nil tool seam so
// buildAPIToolsRegistry sets tools.Deps.Memory. (Embedder construction
// is offline — it only builds an SDK client — so this holds in CI with
// no AWS credentials.)
func TestBuildAPIToolMemoryWiresSeam(t *testing.T) {
	st := store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja-test")
	deps := &Deps{Store: st, Log: testLogger()}

	if mem := buildAPIToolMemory(context.Background(), deps); mem == nil {
		t.Fatal("buildAPIToolMemory returned nil with a live store — memory tools would degrade to not_configured (the 'Memory failed' incident)")
	}
}

// TestBuildAPIToolMemoryNilStoreDegrades pins the graceful path: no
// store → nil seam (memory tools report not_configured), never a panic.
func TestBuildAPIToolMemoryNilStoreDegrades(t *testing.T) {
	deps := &Deps{Store: nil, Log: testLogger()}
	if mem := buildAPIToolMemory(context.Background(), deps); mem != nil {
		t.Fatal("buildAPIToolMemory should return nil without a store")
	}
}

// TestToolsInvokeMemoryWriteEndToEnd drives POST /api/v1/tools/invoke
// through handleToolsInvoke with a registry whose Memory seam is wired
// exactly as buildAPIToolsRegistry wires it (tools.NewMemoryService over
// memory.NewService), with only the Bedrock embedder swapped for the
// deterministic fake. Before the fix this invocation answered 503
// not_configured; now it must persist both the ENT# and EMB# items.
func TestToolsInvokeMemoryWriteEndToEnd(t *testing.T) {
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja-test")

	msvc, err := memory.NewService(st, &fakeEmbedder{})
	if err != nil {
		t.Fatalf("memory.NewService: %v", err)
	}
	registry, err := tools.NewRegistry(&tools.Deps{
		Store:       st,
		TableName:   "live-ninja-test",
		Log:         testLogger(),
		Reauthorize: func(ctx context.Context, userID string) error { return nil },
		Memory:      tools.NewMemoryService(msvc),
	})
	if err != nil {
		t.Fatalf("tools.NewRegistry: %v", err)
	}

	deps := &Deps{Store: st, Log: testLogger()}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		c.Locals(localSessionID, "sess-1")
		c.Locals(localSurface, "web")
		return c.Next()
	})
	app.Post("/api/v1/tools/invoke", handleToolsInvoke(deps, registry))

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/tools/invoke", map[string]any{
		"tool":           "memory_write",
		"callId":         "call-1",
		"idempotencyKey": "idem-1",
		"args": map[string]any{
			"type":  "person",
			"name":  "Sam the coffee friend",
			"attrs": []string{"role=friend"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("memory_write via /tools/invoke = %d, want 200 (body %v)", resp.StatusCode, body)
	}
	if body["ok"] != true {
		t.Fatalf("memory_write result not ok: %v", body)
	}
	output, _ := body["output"].(map[string]any)
	if output == nil || output["status"] != "saved" {
		t.Fatalf("memory_write output = %v, want status=saved", body["output"])
	}
	entityID, _ := output["entityId"].(string)
	if entityID == "" {
		t.Fatalf("memory_write output missing entityId: %v", output)
	}
	if fake.RawItem("USER#u1", "ENT#person#"+entityID) == nil {
		t.Errorf("ENT item missing after tool write")
	}
	if fake.RawItem("USER#u1", "EMB#"+entityID) == nil {
		t.Errorf("EMB item missing after tool write — entity saved but not semantically indexed")
	}
}
