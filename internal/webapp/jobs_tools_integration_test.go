package webapp

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
)

// Exercise the production registry builder, HTTP envelope and fresh account
// authorization together. SDK clients are constructed but never called.
func TestJobsToolsProductionRegistryHTTPWiringAndRevocation(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	ctx := context.Background()
	st := store.NewWithClient(testutil.NewFakeDynamo(), "test-table")
	require.NoError(t, st.CreateUser(ctx, &store.User{UserID: "alice", AmazonUserID: "test-amazon-alice", Role: store.RoleOwner, Status: store.UserStatusActive}))
	fs, err := jobs.NewFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = fs.Close() })
	svc := jobs.NewService(fs)
	j, err := svc.Create(ctx, "alice", jobs.Input{Title: "Exact checkpoint", Instructions: "Review this text", Kind: "review"}, "wiring-create-job")
	require.NoError(t, err)
	deps := &Deps{Store: st, Log: testLogger(), Cfg: config.App{TableName: "test-table"}, Jobs: svc}
	registry := buildAPIToolsRegistry(deps)
	require.NotNil(t, registry)
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Request().Header.Set("X-LN-Capabilities", "jobs-review-v1")
		c.Locals(localUserID, "alice")
		c.Locals(localSessionID, "verified-session")
		c.Locals(localSurface, "web")
		return c.Next()
	})
	app.Post("/api/v1/tools/invoke", handleToolsInvoke(deps, registry))
	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/tools/invoke", map[string]any{"tool": "job_list", "args": map[string]any{}, "callId": "list-call"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, true, body["ok"])
	require.Contains(t, body, "output")
	data := body["output"].(map[string]any)
	require.Len(t, data["jobs"], 1)
	resp, body = doJSON(t, app, http.MethodPost, "/api/v1/tools/invoke", map[string]any{"tool": "job_start", "args": map[string]any{"jobId": j.ID}, "callId": "proposal-call"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	problem := body["error"].(map[string]any)
	require.Equal(t, "confirmation_required", problem["code"])
	details := problem["details"].(map[string]any)
	require.Equal(t, "job_start", details["operation"])
	require.Equal(t, j.ID, details["proposed"].(map[string]any)["jobId"])
	current, err := svc.Get(ctx, "alice", j.ID)
	require.NoError(t, err)
	require.Equal(t, j.Version, current.Version)
	runs, err := svc.ListRuns(ctx, "alice", j.ID, 10, "")
	require.NoError(t, err)
	require.Empty(t, runs.Runs)
	// A still-valid session cannot retain Jobs access after account revocation.
	require.NoError(t, st.SetUserStatus(ctx, "alice", "disabled"))
	resp, body = doJSON(t, app, http.MethodPost, "/api/v1/tools/invoke", map[string]any{"tool": "job_list", "callId": "revoked-call"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
}
