package webapp

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/stretchr/testify/require"
)

func TestJobsHistoryHTTPNoteReplayIsolationAndUnsupportedCommands(t *testing.T) {
	app, svc := jobsTestApp(t, true)
	j, e := svc.Create(context.Background(), "alice", jobs.Input{Title: "Review", Kind: "review"}, "history-http-create")
	require.NoError(t, e)
	body := fmt.Sprintf(`{"kind":"note","text":"User context, no execution","expectedVersion":%d,"requestId":"note-http-request"}`, j.Version)
	status, first := jobsRequest(t, app, "POST", "/api/v1/jobs/"+j.ID+"/commands", body, "alice")
	require.Equal(t, 200, status, first)
	require.Equal(t, false, first["executionChanged"])
	status, again := jobsRequest(t, app, "POST", "/api/v1/jobs/"+j.ID+"/commands", body, "alice")
	require.Equal(t, 200, status, again)
	require.Equal(t, first["command"], again["command"])
	status, history := jobsRequest(t, app, "GET", "/api/v1/jobs/"+j.ID+"/history?limit=1", "", "alice")
	require.Equal(t, 200, status, history)
	require.Len(t, history["entries"], 1)
	require.NotEmpty(t, history["olderCursor"])
	require.NotEmpty(t, history["newerCursor"])
	status, _ = jobsRequest(t, app, "GET", "/api/v1/jobs/"+j.ID+"/history", "", "bob")
	require.Equal(t, 404, status)
	status, _ = jobsRequest(t, app, "GET", "/api/v1/jobs/"+j.ID+"/conversation", "", "alice")
	require.Equal(t, 501, status)
	status, _ = jobsRequest(t, app, "POST", "/api/v1/jobs/"+j.ID+"/commands", `{"kind":"steer","text":"Execute","expectedVersion":2,"requestId":"steer-http-request"}`, "alice")
	require.Equal(t, 501, status)
	status, _ = jobsRequest(t, app, "POST", "/api/v1/jobs/"+j.ID+"/commands", `{"kind":"note","text":"Forged","actor":"admin","expectedVersion":2,"requestId":"forged-http-request"}`, "alice")
	require.Equal(t, 400, status)
	for _, scope := range []string{"nova", "provider"} {
		req := httptest.NewRequest("POST", "/api/v1/jobs/"+j.ID+"/commands", strings.NewReader(body))
		req.Header.Set("Test-User", "alice")
		req.Header.Set("Test-Scope", scope)
		response, e := app.Test(req)
		require.NoError(t, e)
		require.Equal(t, 403, response.StatusCode)
		response.Body.Close()
	}
	req := httptest.NewRequest("POST", "/api/v1/jobs/"+j.ID+"/commands", strings.NewReader(body))
	req.Header.Set("Test-User", "alice")
	req.Header.Set("Cookie", RefreshCookieName+"=session")
	response, e := app.Test(req)
	require.NoError(t, e)
	require.Equal(t, 403, response.StatusCode)
	response.Body.Close()
}
