package webapp

import (
	"context"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

type suburbanOAuthStub struct {
	calls int
	owner string
}

func (s *suburbanOAuthStub) Begin(_ context.Context, owner string) (string, string, error) {
	s.calls++
	s.owner = owner
	return "https://suburban.jeremy.ninja/oauth/authorize", "synthetic-cookie", nil
}
func (s *suburbanOAuthStub) Complete(_ context.Context, owner, state, code, cookie, issuer string) error {
	s.calls++
	s.owner = owner
	return nil
}
func TestSuburbanConnectRequiresCurrentOwner(t *testing.T) {
	app := fiber.New()
	stub := &suburbanOAuthStub{}
	owner := ""
	registerSuburbanRoutes(app, stub, func(*fiber.Ctx) string { return owner })
	r, e := app.Test(httptest.NewRequest("GET", "/auth/suburban/start", nil))
	require.NoError(t, e)
	require.Equal(t, 401, r.StatusCode)
	r.Body.Close()
	require.Zero(t, stub.calls)
	owner = "owner-1"
	r, e = app.Test(httptest.NewRequest("GET", "/auth/suburban/start", nil))
	require.NoError(t, e)
	require.Equal(t, 302, r.StatusCode)
	require.Equal(t, owner, stub.owner)
	for _, c := range r.Cookies() {
		if c.Name == suburbanCookie {
			require.True(t, c.Secure)
			require.True(t, c.HttpOnly)
		}
	}
	r.Body.Close()
	r, e = app.Test(httptest.NewRequest("GET", "/auth/suburban/callback?state=a&state=b&code=c&iss=d", nil))
	require.NoError(t, e)
	require.Equal(t, 400, r.StatusCode)
	r.Body.Close()
	require.Equal(t, 1, stub.calls)
}

func TestMCPSuburbanRequiresVehicleScope(t *testing.T) {
	f := &mcpFakeTools{}
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"suburban_remote","arguments":{"tool":"vehicle_lock","requestId":"suburban_request_1"}}}`
	status, _ := mcpRequest(t, mcpTestApp(f, []string{"memory:write"}), raw, nil)
	if status != 403 || len(f.calls) != 0 {
		t.Fatal("vehicle scope bypass")
	}
	status, out := mcpRequest(t, mcpTestApp(f, []string{"vehicle:control"}), raw, nil)
	if status != 200 || out["error"] != nil || len(f.calls) != 1 || f.calls[0].IdempotencyKey != "mcp:suburban_remote:suburban_request_1" || f.calls[0].UserID != "owner-1" {
		t.Fatal("vehicle scope routing")
	}
}
