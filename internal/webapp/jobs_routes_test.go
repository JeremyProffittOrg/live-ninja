package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
)

func jobsTestApp(t *testing.T, scheduling bool) (*fiber.App, *jobs.Service) {
	t.Helper()
	st, e := jobs.NewFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, e)
	t.Cleanup(func() { _ = st.Close() })
	svc := jobs.NewService(st)
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		if c.Get("Test-User") != "" {
			c.Locals("userId", c.Get("Test-User"))
			c.Locals("surface", "web")
		}
		c.Locals("scope", c.Get("Test-Scope"))
		return c.Next()
	})
	app.Use(CSRFProtect())
	RegisterJobsAPI(app, svc, scheduling, func(_ context.Context, u string) error {
		if u == "disabled" {
			return jobs.ErrForbidden
		}
		return nil
	})
	return app, svc
}
func jobsRequest(t *testing.T, app *fiber.App, method, path, body, user string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Test-User", user)
	res, e := app.Test(req)
	require.NoError(t, e)
	defer res.Body.Close()
	var got map[string]any
	require.NoError(t, json.NewDecoder(res.Body).Decode(&got))
	return res.StatusCode, got
}
func TestJobsHTTPCreateReplayAndTenantIsolation(t *testing.T) {
	app, _ := jobsTestApp(t, true)
	body := `{"title":"Review a quote","instructions":"Check the total","kind":"review","schedule":{"kind":"once"},"requestId":"create-job-001"}`
	status, got := jobsRequest(t, app, "POST", "/api/v1/jobs", body, "alice")
	require.Equal(t, 201, status, got)
	j := got["job"].(map[string]any)
	id := j["id"].(string)
	status, again := jobsRequest(t, app, "POST", "/api/v1/jobs", body, "alice")
	require.Equal(t, 201, status)
	require.Equal(t, id, again["job"].(map[string]any)["id"])
	status, _ = jobsRequest(t, app, "GET", "/api/v1/jobs/"+id, "", "bob")
	require.Equal(t, 404, status)
	status, list := jobsRequest(t, app, "GET", "/api/v1/jobs", "", "alice")
	require.Equal(t, 200, status)
	require.Len(t, list["jobs"], 1)
	version := int64(j["version"].(float64))
	status, run := jobsRequest(t, app, "POST", "/api/v1/jobs/"+id+"/run", fmt.Sprintf(`{"expectedVersion":%d,"requestId":"run-job-001"}`, version), "alice")
	require.Equal(t, 200, status, run)
	require.Equal(t, "waiting_approval", run["run"].(map[string]any)["status"])
	status, _ = jobsRequest(t, app, "POST", "/api/v1/jobs/"+id+"/cancel", fmt.Sprintf(`{"expectedVersion":%d,"requestId":"cancel-stale-001"}`, version), "alice")
	require.Equal(t, 409, status)
	current := int64(run["job"].(map[string]any)["version"].(float64))
	rid := run["run"].(map[string]any)["id"].(string)
	status, approved := jobsRequest(t, app, "POST", "/api/v1/jobs/"+id+"/runs/"+rid+"/approve", fmt.Sprintf(`{"expectedVersion":%d,"requestId":"approve-001"}`, current), "alice")
	require.Equal(t, 200, status, approved)
	require.Equal(t, "succeeded", approved["run"].(map[string]any)["status"])
}
func TestJobsHTTPFailClosed(t *testing.T) {
	app, _ := jobsTestApp(t, false)
	status, _ := jobsRequest(t, app, "GET", "/api/v1/jobs", "", "")
	require.Equal(t, 401, status)
	status, _ = jobsRequest(t, app, "GET", "/api/v1/jobs", "", "disabled")
	require.Equal(t, 403, status)
	req := httptest.NewRequest("POST", "/api/v1/jobs", strings.NewReader(`{"requestId":"attempt"}`))
	req.Header.Set("Test-User", "alice")
	req.Header.Set("Test-Scope", "nova")
	res, e := app.Test(req)
	require.NoError(t, e)
	require.Equal(t, 403, res.StatusCode)
	res.Body.Close()
	req = httptest.NewRequest("POST", "/api/v1/jobs", strings.NewReader(`{"requestId":"attempt"}`))
	req.Header.Set("Test-User", "alice")
	req.Header.Set("Cookie", RefreshCookieName+"=test")
	res, e = app.Test(req)
	require.NoError(t, e)
	require.Equal(t, 403, res.StatusCode)
	res.Body.Close()
	status, _ = jobsRequest(t, app, "POST", "/api/v1/jobs", `{"title":"Daily","kind":"reminder","schedule":{"kind":"daily","timezone":"Etc/UTC","time":"08:00"},"requestId":"schedule-001"}`, "alice")
	require.Equal(t, 409, status)
	status, _ = jobsRequest(t, app, "POST", "/api/v1/jobs", `{"title":"Bad","kind":"coding","requestId":"bad-kind-001"}`, "alice")
	require.Equal(t, 400, status)
	status, _ = jobsRequest(t, app, "POST", "/api/v1/jobs", `{"title":"Bad","userId":"bob","requestId":"bad-user-001"}`, "alice")
	require.Equal(t, 400, status)
}
