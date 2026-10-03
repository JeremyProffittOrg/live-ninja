package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/codeapproval"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
)

func codeApprovalApp(t *testing.T) (*fiber.App, *bool) {
	t.Helper()
	st, e := codeapproval.NewFileStore(filepath.Join(t.TempDir(), "reviews.json"))
	require.NoError(t, e)
	t.Cleanup(func() { _ = st.Close() })
	svc := codeapproval.NewPreviewService(st)
	active := true
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("userId", c.Get("Test-User"))
		surface := c.Get("Test-Surface")
		if surface == "" {
			surface = "web"
		}
		c.Locals("surface", surface)
		c.Locals("role", "owner")
		c.Locals("scope", c.Get("Test-Scope"))
		return c.Next()
	})
	RegisterCodeApprovalAPI(app, svc, func(_ context.Context, uid string) error {
		if !active || uid == "member" {
			return codeapproval.ErrForbidden
		}
		return nil
	})
	return app, &active
}

const approvalPrepareBody = `{"repo":"preview/example","node":"PREVIEW_ONLY","instructions":"Review this exact change","agent":"codex","preprocess":false,"deploy":false,"requestId":"prepare-http-001"}`

func TestCodeApprovalHTTPPrepareReviewReceiptAndReplay(t *testing.T) {
	app, active := codeApprovalApp(t)
	status, got := jobsRequest(t, app, "POST", "/api/v1/code-approvals/prepare", approvalPrepareBody, "owner")
	require.Equal(t, 201, status, got)
	intent := got["intent"].(map[string]any)
	id := intent["id"].(string)
	version := int64(intent["version"].(float64))
	hash := intent["actionHash"].(string)
	path := "/api/v1/code-approvals/" + id + "/approve"
	status, again := jobsRequest(t, app, "POST", "/api/v1/code-approvals/prepare", approvalPrepareBody, "owner")
	require.Equal(t, 201, status)
	require.Equal(t, id, again["intent"].(map[string]any)["id"])
	// A stale JWT role cannot override the fresh owner lookup.
	*active = false
	status, _ = jobsRequest(t, app, "POST", path, fmt.Sprintf(`{"expectedVersion":%d,"expectedActionHash":%q,"requestId":"approve-http-001"}`, version, hash), "owner")
	require.Equal(t, 403, status)
	*active = true
	status, _ = jobsRequest(t, app, "POST", path, fmt.Sprintf(`{"expectedVersion":%d,"expectedActionHash":%q,"requestId":"approve-http-001","instructions":"replacement"}`, version, hash), "owner")
	require.Equal(t, 400, status)
	body := fmt.Sprintf(`{"expectedVersion":%d,"expectedActionHash":%q,"requestId":"approve-http-001"}`, version, hash)
	status, approved := jobsRequest(t, app, "POST", path, body, "owner")
	require.Equal(t, 200, status, approved)
	saved := approved["intent"].(map[string]any)
	require.Equal(t, "approved_blocked", saved["status"])
	require.Equal(t, "not_started", saved["receipt"].(map[string]any)["executionState"])
	require.Equal(t, false, approved["capabilities"].(map[string]any)["execute"])
	status, replay := jobsRequest(t, app, "POST", path, body, "owner")
	require.Equal(t, 200, status)
	require.Equal(t, saved, replay["intent"])
	status, _ = jobsRequest(t, app, "POST", path, strings.ReplaceAll(body, "approve-http-001", "approve-http-002"), "owner")
	require.Equal(t, 409, status)
	status, _ = jobsRequest(t, app, "GET", "/api/v1/code-approvals/"+id, "", "other")
	require.Equal(t, 404, status)
	status, list := jobsRequest(t, app, "GET", "/api/v1/code-approvals", "", "owner")
	require.Equal(t, 200, status)
	require.Len(t, list["intents"], 1)
}
func TestCodeApprovalHTTPFailsClosedForScopeSurfaceCSRFAndUnknownFields(t *testing.T) {
	app, _ := codeApprovalApp(t)
	for _, uid := range []string{"", "member"} {
		status, _ := jobsRequest(t, app, "GET", "/api/v1/code-approvals/options", "", uid)
		if uid == "" {
			require.Equal(t, 401, status)
		} else {
			require.Equal(t, 403, status)
		}
	}
	for name, headers := range map[string]map[string]string{"scoped": {"Test-Scope": "nova"}, "device": {"Test-Surface": "device"}, "cookie missing CSRF": {"Cookie": RefreshCookieName + "=test"}, "cookie mismatched CSRF": {"Cookie": RefreshCookieName + "=test; " + CSRFCookieName + "=abc", CSRFHeaderName: "different"}} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/code-approvals/prepare", strings.NewReader(approvalPrepareBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Test-User", "owner")
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			response, e := app.Test(req)
			require.NoError(t, e)
			defer response.Body.Close()
			require.Equal(t, 403, response.StatusCode)
		})
	}
	for _, body := range []string{strings.Replace(approvalPrepareBody, `"deploy":false`, `"deploy":true`, 1), strings.Replace(approvalPrepareBody, `"repo":`, `"userId":"other","repo":`, 1), approvalPrepareBody + ` {}`} {
		status, _ := jobsRequest(t, app, "POST", "/api/v1/code-approvals/prepare", body, "owner")
		require.Equal(t, 400, status)
	}
	// Native bearer sessions carry no refresh cookie and need no browser CSRF echo.
	req := httptest.NewRequest("POST", "/api/v1/code-approvals/prepare", strings.NewReader(approvalPrepareBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Test-User", "owner")
	req.Header.Set("Test-Surface", "android")
	response, e := app.Test(req)
	require.NoError(t, e)
	defer response.Body.Close()
	require.Equal(t, 201, response.StatusCode)
	var got map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&got))
	require.Equal(t, false, got["intent"].(map[string]any)["verification"].(map[string]any)["repoVerified"])
}
