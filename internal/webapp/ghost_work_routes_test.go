package webapp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
)

type ghostWorkInvoke struct {
	status        int
	body          string
	functionError bool
	calls         int
	event         map[string]any
}

func (f *ghostWorkInvoke) Invoke(_ context.Context, in *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	f.calls++
	_ = json.Unmarshal(in.Payload, &f.event)
	if f.functionError {
		return &lambda.InvokeOutput{FunctionError: aws.String("Unhandled"), Payload: []byte(`{"errorMessage":"route not allowlisted"}`)}, nil
	}
	b, _ := json.Marshal(map[string]any{"statusCode": f.status, "body": f.body})
	return &lambda.InvokeOutput{Payload: b}, nil
}
func ghostWorkFixture(t *testing.T) (*fiber.App, *ghostWorkInvoke, *store.Store) {
	t.Helper()
	f := &ghostWorkInvoke{status: 200, body: `{"version":1,"node_id":"OFFICEPC","sessions":[],"next_cursor":"","coverage":"retained_only"}`}
	st := store.NewWithClient(testutil.NewFakeDynamo(), "test")
	for _, u := range []store.User{{UserID: "owner", AmazonUserID: "test-owner", Role: store.RoleOwner, Status: store.UserStatusActive}, {UserID: "member", AmazonUserID: "test-member", Role: store.RoleMember, Status: store.UserStatusActive}} {
		require.NoError(t, st.CreateUser(context.Background(), &u))
	}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, c.Get("Test-User"))
		c.Locals(localSurface, "web")
		c.Locals(localScope, c.Get("Test-Scope"))
		return c.Next()
	})
	RegisterGhostWorkRoutes(app, &Deps{Store: st, Ghost: ghost.New(ghost.Config{API: f, Function: "fake-command", Log: testLogger()})})
	return app, f, st
}
func TestGhostWorkProductionOwnerBoundaryAndRevocation(t *testing.T) {
	app, f, st := ghostWorkFixture(t)
	path := "/api/v1/ghost-work/sessions?node_id=OFFICEPC"
	for _, tc := range []struct {
		user, scope string
		status      int
	}{{"", "", 401}, {"member", "", 403}, {"owner", "nova", 403}, {"owner", "provider", 403}} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Test-User", tc.user)
		req.Header.Set("Test-Scope", tc.scope)
		resp, e := app.Test(req)
		require.NoError(t, e)
		require.Equal(t, tc.status, resp.StatusCode)
		require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
		resp.Body.Close()
	}
	require.Zero(t, f.calls)
	status, page := jobsRequest(t, app, "GET", path, "", "owner")
	require.Equal(t, 200, status, page)
	require.Equal(t, "retained_only", page["coverage"])
	require.Equal(t, 1, f.calls)
	require.Equal(t, "GET", f.event["method"])
	require.Equal(t, "/history/sessions", f.event["resource"])
	require.NotContains(t, f.event, "principal")
	require.NotContains(t, f.event, "role")
	require.NotContains(t, f.event, "scope")
	require.NoError(t, st.SetUserStatus(context.Background(), "owner", store.UserStatusDeleting))
	status, _ = jobsRequest(t, app, "GET", path, "", "owner")
	require.Equal(t, 403, status)
	require.Equal(t, 1, f.calls)
}

func TestGhostWorkHTTPPreservesFidelityAndRecoveryStatus(t *testing.T) {
	const session = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	app, f, _ := ghostWorkFixture(t)
	long := strings.Repeat("complete visible text ", 2000)
	p := ghost.HistoryEventsPage{Version: 1, NodeID: "OFFICEPC", SessionID: session, Coverage: "retained_only", ResumeCursor: "resume", NextCursor: "next", Gaps: []string{"missing older upload"}, Events: []ghost.HistoryEvent{{ID: "batch/0001", Sequence: "batch/0001", Kind: "tool_result", Text: long, Part: 0, More: true}, {ID: "batch/0002", Sequence: "batch/0002", Kind: "tool_result", Text: long, Part: 1}}}
	b, _ := json.Marshal(p)
	f.body = string(b)
	path := "/api/v1/ghost-work/events?node_id=OFFICEPC&session_id=" + session + "&cursor=opaque"
	status, page := jobsRequest(t, app, "GET", path, "", "owner")
	require.Equal(t, 200, status, page)
	events := page["events"].([]any)
	require.Equal(t, long, events[0].(map[string]any)["text"])
	require.Equal(t, long, events[1].(map[string]any)["text"])
	require.NotContains(t, events[0].(map[string]any), "timestamp")
	require.Equal(t, "batch/0001", events[0].(map[string]any)["sequence"])
	require.Equal(t, "next", page["next_cursor"])
	require.Equal(t, "resume", page["resume_cursor"])
	query := f.event["query"].(map[string]any)
	require.Equal(t, session, query["session_id"])
	require.Equal(t, "opaque", query["cursor"])
	// A malformed fragment is a failed page, never a successful empty/final
	// response that lets clients advance past missing visible text.
	f.body = `{"version":1,"node_id":"OFFICEPC","session_id":"` + session + `","events":[{"id":"one","sequence":"one","kind":"assistant","part":0,"more":false}],"resume_cursor":"resume","coverage":"retained_only","gaps":[]}`
	status, page = jobsRequest(t, app, "GET", path, "", "owner")
	require.Equal(t, 503, status)
	require.Equal(t, "provider_unavailable", page["error"].(map[string]any)["code"])
	require.NotContains(t, page, "events")
	require.NotContains(t, page, "resume_cursor")
	for upstream, code := range map[int]string{400: "invalid_request", 401: "provider_access_denied", 403: "provider_access_denied", 409: "history_rescan_required", 410: "history_expired", 413: "history_object_too_large", 422: "history_malformed", 429: "provider_rate_limited", 503: "provider_unavailable"} {
		f.status = upstream
		f.body = `{"error":"private upstream body"}`
		status, page = jobsRequest(t, app, "GET", path, "", "owner")
		require.Equal(t, upstream, status, page)
		require.Equal(t, code, page["error"].(map[string]any)["code"])
		require.NotContains(t, page["error"].(map[string]any)["message"], "private upstream body")
	}
	f.functionError = true
	status, page = jobsRequest(t, app, "GET", path, "", "owner")
	require.Equal(t, 503, status)
	require.Equal(t, "provider_transport_unavailable", page["error"].(map[string]any)["code"])
	before := f.calls
	status, _ = jobsRequest(t, app, "GET", "/api/v1/ghost-work/events?node_id=OFFICEPC&session_id=latest", "", "owner")
	require.Equal(t, 400, status)
	require.Equal(t, before, f.calls)
}
