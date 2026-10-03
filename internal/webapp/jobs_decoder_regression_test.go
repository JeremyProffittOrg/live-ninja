package webapp

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestJobsRejectedJSONNeverMutates(t *testing.T) {
	for _, suffix := range []string{`,"unknown":true}`, `} {"extra":true}`} {
		t.Run(suffix, func(t *testing.T) {
			app, svc := jobsTestApp(t, true)
			body := `{"title":"Must not exist","kind":"reminder","schedule":{"kind":"once"},"requestId":"invalid-json-create"` + suffix
			status, _ := jobsRequest(t, app, "POST", "/api/v1/jobs", body, "alice")
			require.Equal(t, 400, status)
			page, err := svc.List(context.Background(), "alice", 50, "")
			require.NoError(t, err)
			require.Empty(t, page.Jobs)
		})
	}
	app, svc := jobsTestApp(t, true)
	status, created := jobsRequest(t, app, "POST", "/api/v1/jobs", `{"title":"Valid","kind":"reminder","schedule":{"kind":"once"},"requestId":"valid-json-create"}`, "alice")
	require.Equal(t, 201, status)
	job := created["job"].(map[string]any)
	id := job["id"].(string)
	status, _ = jobsRequest(t, app, "POST", "/api/v1/jobs/"+id+"/run", fmt.Sprintf(`{"expectedVersion":%.0f,"requestId":"invalid-json-run","unknown":true}`, job["version"]), "alice")
	require.Equal(t, 400, status)
	runs, err := svc.ListRuns(context.Background(), "alice", id, 50, "")
	require.NoError(t, err)
	require.Empty(t, runs.Runs)
}
