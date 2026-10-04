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

const ghostTestSession = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

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

// ghostWorkFixture deliberately uses Fiber's default ReadBufferSize (4096),
// which cmd/web/main.go also leaves unset, so header-buffer behavior matches.
func ghostWorkFixture(t *testing.T, before ...fiber.Handler) (*fiber.App, *ghostWorkInvoke, *store.Store) {
	t.Helper()
	f := &ghostWorkInvoke{status: 200, body: `{"version":1,"node_id":"OFFICEPC","sessions":[],"next_cursor":"","coverage":"retained_only"}`}
	st := store.NewWithClient(testutil.NewFakeDynamo(), "test")
	for _, u := range []store.User{{UserID: "owner", AmazonUserID: "test-owner", Role: store.RoleOwner, Status: store.UserStatusActive}, {UserID: "member", AmazonUserID: "test-member", Role: store.RoleMember, Status: store.UserStatusActive}} {
		require.NoError(t, st.CreateUser(context.Background(), &u))
	}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	for _, handler := range before {
		app.Use(handler)
	}
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, c.Get("Test-User"))
		c.Locals(localSurface, "web")
		c.Locals(localScope, c.Get("Test-Scope"))
		return c.Next()
	})
	RegisterGhostWorkRoutes(app, &Deps{Store: st, Ghost: ghost.New(ghost.Config{API: f, Function: "fake-command", Log: testLogger()})})
	return app, f, st
}

type ghostWorkResult struct {
	status int
	body   map[string]any
	cache  string
}

func ghostWorkSend(app *fiber.App, method, target, body string, headers map[string]string) (ghostWorkResult, error) {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, e := app.Test(req, -1)
	if e != nil {
		return ghostWorkResult{}, e
	}
	defer resp.Body.Close()
	out := ghostWorkResult{status: resp.StatusCode, cache: resp.Header.Get("Cache-Control")}
	_ = json.NewDecoder(resp.Body).Decode(&out.body)
	return out, nil
}

func ghostWorkPost(t *testing.T, app *fiber.App, path, body, user string, extra map[string]string) ghostWorkResult {
	t.Helper()
	headers := map[string]string{"Content-Type": "application/json"}
	if user != "" {
		headers["Test-User"] = user
	}
	for k, v := range extra {
		if v == "" {
			delete(headers, k)
		} else {
			headers[k] = v
		}
	}
	res, e := ghostWorkSend(app, "POST", path, body, headers)
	require.NoError(t, e)
	return res
}

func ghostEventsEndBody() string {
	return `{"version":1,"node_id":"OFFICEPC","session_id":"` + ghostTestSession + `","events":[],"resume_cursor":"resume","coverage":"retained_only","gaps":[]}`
}

func errorCode(res ghostWorkResult) string {
	if m, ok := res.body["error"].(map[string]any); ok {
		code, _ := m["code"].(string)
		return code
	}
	code, _ := res.body["error"].(string)
	return code
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
	const session = ghostTestSession
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

// POST archive-page reads keep the same owner boundary and carry a maximum
// 2048-byte cursor to the provider unchanged, outside the request line.
func TestGhostWorkPostPagesKeepOwnerBoundaryAndCursorOutOfRequestLine(t *testing.T) {
	app, f, _ := ghostWorkFixture(t)
	cursor := strings.Repeat("A", 2048)
	sessionsBody := `{"node_id":"OFFICEPC","cursor":"` + cursor + `"}`
	for _, tc := range []struct {
		user, scope string
		status      int
	}{{"", "", 401}, {"member", "", 403}, {"owner", "nova", 403}, {"owner", "provider", 403}} {
		res := ghostWorkPost(t, app, "/api/v1/ghost-work/sessions", sessionsBody, tc.user, map[string]string{"Test-Scope": tc.scope})
		require.Equal(t, tc.status, res.status)
		require.Equal(t, "no-store", res.cache)
	}
	require.Zero(t, f.calls)
	res := ghostWorkPost(t, app, "/api/v1/ghost-work/sessions", sessionsBody, "owner", nil)
	require.Equal(t, 200, res.status, res.body)
	require.Equal(t, "no-store", res.cache)
	require.Equal(t, "retained_only", res.body["coverage"])
	require.Equal(t, 1, f.calls)
	require.Equal(t, "GET", f.event["method"])
	require.Equal(t, "/history/sessions", f.event["resource"])
	require.Equal(t, cursor, f.event["query"].(map[string]any)["cursor"])

	f.body = ghostEventsEndBody()
	res = ghostWorkPost(t, app, "/api/v1/ghost-work/events", `{"node_id":"OFFICEPC","session_id":"`+ghostTestSession+`","cursor":"`+cursor+`"}`, "owner", nil)
	require.Equal(t, 200, res.status, res.body)
	require.Equal(t, "resume", res.body["resume_cursor"])
	require.Equal(t, 2, f.calls)
	query := f.event["query"].(map[string]any)
	require.Equal(t, "/history/events", f.event["resource"])
	require.Equal(t, ghostTestSession, query["session_id"])
	require.Equal(t, cursor, query["cursor"])

	// Upstream recovery status mapping is identical for POST reads.
	f.status, f.body = 409, `{"error":"private upstream body"}`
	res = ghostWorkPost(t, app, "/api/v1/ghost-work/events", `{"node_id":"OFFICEPC","session_id":"`+ghostTestSession+`"}`, "owner", nil)
	require.Equal(t, 409, res.status)
	require.Equal(t, "history_rescan_required", errorCode(res))
}

func TestGhostWorkPostPagesRejectMalformedOrUnboundedBodies(t *testing.T) {
	app, f, _ := ghostWorkFixture(t)
	f.body = ghostEventsEndBody()
	session := ghostTestSession
	events, sessions := "/api/v1/ghost-work/events", "/api/v1/ghost-work/sessions"
	for name, tc := range map[string]struct {
		path, body string
		headers    map[string]string
		status     int
		code       string
	}{
		"oversized body":             {events, `{"node_id":"OFFICEPC","session_id":"` + session + `","cursor":"` + strings.Repeat("A", 9000) + `"}`, nil, 413, "request_too_large"},
		"cursor over provider limit": {events, `{"node_id":"OFFICEPC","session_id":"` + session + `","cursor":"` + strings.Repeat("A", 2049) + `"}`, nil, 400, "invalid_request"},
		"unknown field":              {events, `{"node_id":"OFFICEPC","session_id":"` + session + `","principal":"x"}`, nil, 400, "invalid_request"},
		"trailing object":            {events, `{"node_id":"OFFICEPC","session_id":"` + session + `"}{}`, nil, 400, "invalid_request"},
		"trailing garbage":           {events, `{"node_id":"OFFICEPC","session_id":"` + session + `"} x`, nil, 400, "invalid_request"},
		"null body":                  {events, `null`, nil, 400, "invalid_request"},
		"array body":                 {events, `[]`, nil, 400, "invalid_request"},
		"empty body":                 {events, ``, nil, 400, "invalid_request"},
		"null field":                 {events, `{"node_id":"OFFICEPC","session_id":"` + session + `","cursor":null}`, nil, 400, "invalid_request"},
		"number field":               {events, `{"node_id":7,"session_id":"` + session + `"}`, nil, 400, "invalid_request"},
		"truncated JSON":             {events, `{"node_id":`, nil, 400, "invalid_request"},
		"text content type":          {events, `{"node_id":"OFFICEPC","session_id":"` + session + `"}`, map[string]string{"Content-Type": "text/plain"}, 400, "invalid_request"},
		"encoded body":               {events, `{"node_id":"OFFICEPC","session_id":"` + session + `"}`, map[string]string{"Content-Encoding": "gzip"}, 400, "invalid_request"},
		"missing session":            {events, `{"node_id":"OFFICEPC"}`, nil, 400, "invalid_request"},
		"latest alias":               {events, `{"node_id":"OFFICEPC","session_id":"latest"}`, nil, 400, "invalid_request"},
		"session on discovery":       {sessions, `{"node_id":"OFFICEPC","session_id":"` + session + `"}`, nil, 400, "invalid_request"},
		"invalid node":               {sessions, `{"node_id":"../secret"}`, nil, 400, "invalid_request"},
	} {
		t.Run(name, func(t *testing.T) {
			before := f.calls
			res := ghostWorkPost(t, app, tc.path, tc.body, "owner", tc.headers)
			require.Equal(t, tc.status, res.status, res.body)
			require.Equal(t, tc.code, errorCode(res))
			require.Equal(t, "no-store", res.cache)
			require.Equal(t, before, f.calls, "rejected request reached provider")
		})
	}
	// GET compatibility for existing clients is preserved.
	f.body = `{"version":1,"node_id":"OFFICEPC","sessions":[],"next_cursor":"","coverage":"retained_only"}`
	status, page := jobsRequest(t, app, "GET", "/api/v1/ghost-work/sessions?node_id=OFFICEPC", "", "owner")
	require.Equal(t, 200, status, page)
}

func TestGhostWorkPostPagesRequireCSRFWithCookieSession(t *testing.T) {
	app, f, _ := ghostWorkFixture(t, CSRFProtect())
	body := `{"node_id":"OFFICEPC"}`
	cookie := RefreshCookieName + "=fixture-refresh; " + CSRFCookieName + "=fixture-csrf"
	res := ghostWorkPost(t, app, "/api/v1/ghost-work/sessions", body, "owner", map[string]string{"Cookie": cookie})
	require.Equal(t, 403, res.status)
	require.Equal(t, "csrf_failed", errorCode(res))
	require.Zero(t, f.calls)
	res = ghostWorkPost(t, app, "/api/v1/ghost-work/sessions", body, "owner", map[string]string{"Cookie": cookie, CSRFHeaderName: "wrong"})
	require.Equal(t, 403, res.status)
	require.Zero(t, f.calls)
	res = ghostWorkPost(t, app, "/api/v1/ghost-work/sessions", body, "owner", map[string]string{"Cookie": cookie, CSRFHeaderName: "fixture-csrf"})
	require.Equal(t, 200, res.status, res.body)
	require.Equal(t, 1, f.calls)
}

// Transport-level reproducer for the HTTP 431 hypothesis, through fasthttp's
// real request parser (app.Test -> Server.ServeConn) with Fiber's default
// 4096-byte read buffer, which bounds request line + headers together.
//
// SYNTHETIC SIZES, NOT A PRODUCTION MEASUREMENT: the padding headers stand in
// for deployed-path headers (bearer token, cookies, the Lambda Web Adapter's
// forwarded request context) and carry no credential values. Only byte counts
// are logged. This demonstrates the mechanism; it does not prove the cause of
// any observed production 431.
func TestGhostWorkHTTP431ReproducerCursorInRequestLineVersusBody(t *testing.T) {
	app, f, _ := ghostWorkFixture(t)
	f.body = ghostEventsEndBody()
	cursor := strings.Repeat("A", 2048)
	padding := map[string]string{
		"X-Synthetic-Context": strings.Repeat("r", 1300),
		"X-Synthetic-Bearer":  strings.Repeat("j", 900),
		"X-Synthetic-Cookie":  strings.Repeat("c", 200),
		"User-Agent":          "synthetic-browser/" + strings.Repeat("u", 150),
	}
	size := func(method, target string, headers map[string]string) int {
		n := len(method) + 1 + len(target) + len(" HTTP/1.1\r\n")
		for k, v := range headers {
			n += len(k) + len(": \r\n") + len(v)
		}
		return n
	}
	with := func(extra map[string]string) map[string]string {
		h := map[string]string{}
		for k, v := range padding {
			h[k] = v
		}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}
	base := "/api/v1/ghost-work/events?node_id=OFFICEPC&session_id=" + ghostTestSession

	getHeaders := with(map[string]string{"Test-User": "owner"})
	getWithCursor := base + "&cursor=" + cursor
	getSize := size("GET", getWithCursor, getHeaders)
	res, e := ghostWorkSend(app, "GET", getWithCursor, "", getHeaders)
	if e != nil {
		require.Contains(t, e.Error(), "small read buffer")
	} else {
		require.Equal(t, fiber.StatusRequestHeaderFieldsTooLarge, res.status)
	}
	require.Zero(t, f.calls, "oversized request line must not reach the handler")

	noCursorSize := size("GET", base, getHeaders)
	res, e = ghostWorkSend(app, "GET", base, "", getHeaders)
	require.NoError(t, e)
	require.Equal(t, 200, res.status, res.body)
	require.Equal(t, 1, f.calls)

	postBody := `{"node_id":"OFFICEPC","session_id":"` + ghostTestSession + `","cursor":"` + cursor + `"}`
	postHeaders := with(map[string]string{"Test-User": "owner", "Content-Type": "application/json"})
	postSize := size("POST", "/api/v1/ghost-work/events", postHeaders)
	res, e = ghostWorkSend(app, "POST", "/api/v1/ghost-work/events", postBody, postHeaders)
	require.NoError(t, e)
	require.Equal(t, 200, res.status, res.body)
	require.Equal(t, 2, f.calls)
	require.Equal(t, cursor, f.event["query"].(map[string]any)["cursor"])

	// The same oversized-cursor POST still enforces authentication.
	res, e = ghostWorkSend(app, "POST", "/api/v1/ghost-work/events", postBody, with(map[string]string{"Content-Type": "application/json"}))
	require.NoError(t, e)
	require.Equal(t, 401, res.status)
	require.Equal(t, 2, f.calls)

	require.Greater(t, getSize, 4096)
	require.Less(t, noCursorSize, 4096)
	require.Less(t, postSize, 4096)
	t.Logf("synthetic request-line+header lower-bound bytes: GET+cursor=%d GET=%d POST=%d (default read buffer 4096)", getSize, noCursorSize, postSize)
}
