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

type httpPhoneProvider struct {
	catalog []jobs.PhoneTarget
	calls   int
	state   string
	last    jobs.PhoneResult
}

func (p *httpPhoneProvider) Catalog(context.Context, string) (jobs.PhoneCatalog, error) {
	return jobs.PhoneCatalog{Targets: p.catalog}, nil
}
func (p *httpPhoneProvider) Advance(_ context.Context, c jobs.PhoneCommand) (jobs.PhoneResult, error) {
	p.calls++
	state := p.state
	if c.Command == "pause" {
		state = "paused"
	}
	if c.Command == "cancel" {
		state = "cancelled"
	}
	r := jobs.PhoneResult{Status: state, Version: int64(p.calls), CommandVersion: c.CommandVersion, Reason: state}
	for _, id := range c.TargetIDs {
		r.Properties = append(r.Properties, jobs.PhoneProperty{TargetID: id, Status: "not_started"})
	}
	p.last = r
	return r, nil
}
func (p *httpPhoneProvider) Status(context.Context, string, string, string) (jobs.PhoneResult, error) {
	return p.last, nil
}

func TestPhoneJobsRequireWebFiveTargetReviewBeforeRunOrResume(t *testing.T) {
	_, base := jobsTestApp(t, false)
	p := &httpPhoneProvider{state: "blocked"}
	ids := []string{}
	for n := 1; n <= 5; n++ {
		id := fmt.Sprintf("hotel-%02d", n)
		ids = append(ids, id)
		p.catalog = append(p.catalog, jobs.PhoneTarget{ID: id, Name: id, City: "California", Phone: fmt.Sprintf("+12025550%03d", n), SourceURL: "https://hotel.example/" + id})
	}
	svc := base.WithPhoneResearch(p)
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "alice")
		c.Locals(localSurface, c.Get("Test-Surface"))
		return c.Next()
	})
	app.Use(CSRFProtect())
	RegisterJobsAPI(app, svc, false, func(context.Context, string) error { return nil })
	request := func(surface, method, path string, value any) (int, map[string]any) {
		raw, e := json.Marshal(value)
		require.NoError(t, e)
		req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Test-Surface", surface)
		resp, e := app.Test(req)
		require.NoError(t, e)
		defer resp.Body.Close()
		var body map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		return resp.StatusCode, body
	}
	input := map[string]any{"kind": jobs.PhoneResearchKind, "title": "Hotels", "instructions": jobs.PhoneResearchPurpose, "phoneTargetIds": ids, "schedule": map[string]string{"kind": "once"}, "requestId": "phone-create-1"}
	status, _ := request("android", "POST", "/api/v1/jobs", input)
	require.Equal(t, 403, status)
	require.Zero(t, p.calls)
	status, body := request("web", "POST", "/api/v1/jobs", input)
	require.Equal(t, 201, status, body)
	j := body["job"].(map[string]any)
	id := j["id"].(string)
	v := j["version"]
	status, _ = request("android", "POST", "/api/v1/jobs/"+id+"/run", map[string]any{"requestId": "phone-run-1", "expectedVersion": v})
	require.Equal(t, 403, status)
	require.Zero(t, p.calls)
	status, body = request("web", "POST", "/api/v1/jobs/"+id+"/run", map[string]any{"requestId": "phone-run-1", "expectedVersion": v})
	require.Equal(t, 200, status, body)
	require.Equal(t, 1, p.calls)
	j = body["job"].(map[string]any)
	v = j["version"]
	require.Equal(t, "paused", j["status"])
	status, _ = request("android", "POST", "/api/v1/jobs/"+id+"/resume", map[string]any{"requestId": "phone-resume-1", "expectedVersion": v})
	require.Equal(t, 403, status)
	require.Equal(t, 1, p.calls)
	p.state = "running"
	status, body = request("web", "POST", "/api/v1/jobs/"+id+"/resume", map[string]any{"requestId": "phone-resume-1", "expectedVersion": v})
	require.Equal(t, 200, status, body)
	require.Equal(t, 2, p.calls)
	j = body["job"].(map[string]any)
	v = j["version"]
	status, body = request("android", "POST", "/api/v1/jobs/"+id+"/pause", map[string]any{"requestId": "phone-pause-1", "expectedVersion": v})
	require.Equal(t, 200, status, body)
	require.Equal(t, 3, p.calls)
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
