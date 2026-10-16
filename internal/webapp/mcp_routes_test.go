package webapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/memory"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
	"github.com/gofiber/fiber/v2"
)

type mcpFakeTools struct{ calls []tools.Invocation }

func (f *mcpFakeTools) Manifest() []map[string]any { return tools.CatalogManifest() }
func (f *mcpFakeTools) Invoke(_ context.Context, i tools.Invocation) *tools.Result {
	f.calls = append(f.calls, i)
	return &tools.Result{OK: true, Tool: i.Tool, Output: map[string]any{"accepted": true}}
}
func mcpTestApp(reg mcpToolInvoker, scopes []string) *fiber.App {
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "owner-1")
		c.Locals(localSessionID, "grant-1")
		c.Locals(localRole, "owner")
		c.Locals("mcpScopes", scopes)
		return c.Next()
	})
	app.All("/mcp", handleMCP(reg, "https://live.jeremy.ninja"))
	return app
}
func mcpRequest(t *testing.T, app *fiber.App, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-11-25")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	res, err := app.Test(r, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}
func TestMCPInitializeAndScopeFilteredTools(t *testing.T) {
	f := &mcpFakeTools{}
	app := mcpTestApp(f, []string{"memory:read"})
	status, out := mcpRequest(t, app, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, nil)
	if status != 200 || out["result"].(map[string]any)["protocolVersion"] != "2025-11-25" {
		t.Fatal(status, out)
	}
	_, out = mcpRequest(t, app, `{"jsonrpc":"2.0","id":"list","method":"tools/list"}`, nil)
	list := out["result"].(map[string]any)["tools"].([]any)
	if len(list) != 3 {
		t.Fatal(list)
	}
	for _, v := range list {
		tool := v.(map[string]any)
		if mcpPolicies[tool["name"].(string)].scope != "memory:read" {
			t.Fatal(tool)
		}
	}
	status, _ = mcpRequest(t, app, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	if status != 202 || len(f.calls) != 0 {
		t.Fatal(status, f.calls)
	}
}
func TestMCPCallUsesVerifiedIdentityAndStableRequest(t *testing.T) {
	f := &mcpFakeTools{}
	app := mcpTestApp(f, []string{"phone:send"})
	raw := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"call_message","arguments":{"destination":"primary","message":"Hello","requestId":"stable_request_1"}}}`
	for i := 0; i < 2; i++ {
		status, out := mcpRequest(t, app, raw, nil)
		if status != 200 || out["result"].(map[string]any)["isError"] != false {
			t.Fatal(status, out)
		}
	}
	if len(f.calls) != 2 || f.calls[0].IdempotencyKey != f.calls[1].IdempotencyKey || f.calls[0].IdempotencyKey == "" {
		t.Fatal(f.calls)
	}
	for _, c := range f.calls {
		if c.UserID != "owner-1" || c.Role != "owner" || c.Surface != "mcp" || c.Args["requestId"] != nil {
			t.Fatal(c)
		}
	}
}
func TestMCPInvalidTransportAndDeniedToolNeverInvokes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		headers    map[string]string
		status     int
	}{
		{"scope", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"call_message","arguments":{"message":"x","requestId":"stable_id"}}}`, nil, 403},
		{"unknown-tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_email"}}`, nil, 200},
		{"notification-write", `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"memory_write"}}`, nil, 400},
		{"origin", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": "https://evil.example"}, 403},
		{"version", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": "2099-01-01"}, 400},
		{"accept", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Accept": "text/html"}, 406},
		{"content", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Content-Type": "text/plain"}, 415},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &mcpFakeTools{}
			status, _ := mcpRequest(t, mcpTestApp(f, []string{"memory:read"}), tc.body, tc.headers)
			if status != tc.status || len(f.calls) != 0 {
				t.Fatal(status, f.calls)
			}
		})
	}
}
func TestMCPMemoryUsesExistingRegistryAndIdempotency(t *testing.T) {
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "test")
	mem, err := memory.NewService(st)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(&tools.Deps{Store: st, TableName: "test", Log: testLogger(), Reauthorize: func(context.Context, string) error { return nil }, Memory: tools.NewMemoryService(mem)})
	if err != nil {
		t.Fatal(err)
	}
	app := mcpTestApp(reg, []string{"memory:write"})
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_write","arguments":{"type":"info","name":"MCP test fact","requestId":"memory_request_1"}}}`
	_, out := mcpRequest(t, app, raw, nil)
	first := out["result"].(map[string]any)["structuredContent"].(map[string]any)
	if first["ok"] != true {
		t.Fatal(out)
	}
	result := first["output"].(map[string]any)
	id := result["entityId"].(string)
	if fake.RawItem("USER#owner-1", "ENT#info#"+id) == nil {
		t.Fatal("owner-scoped memory missing")
	}
	_, out = mcpRequest(t, app, raw, nil)
	repeated := out["result"].(map[string]any)["structuredContent"].(map[string]any)
	if repeated["duplicate"] != true {
		t.Fatal(out)
	}
}
