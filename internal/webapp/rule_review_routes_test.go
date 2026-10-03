package webapp

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
)

func ruleReviewTestApp(t *testing.T) (*fiber.App, *store.Store, *testutil.FakeDynamo) {
	t.Helper()
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "table")
	for _, uid := range []string{"alice", "bob", "disabled", "member"} {
		status, role := store.UserStatusActive, store.RoleOwner
		if uid == "disabled" {
			status = "deleting"
		}
		if uid == "member" {
			role = store.RoleMember
		}
		require.NoError(t, st.CreateUser(context.Background(), &store.User{UserID: uid, AmazonUserID: "amzn." + uid, Email: uid + "@example.com", Status: status, Role: role}))
	}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, c.Get("Test-User"))
		c.Locals(localSurface, c.Get("Test-Surface", "web"))
		c.Locals(localScope, c.Get("Test-Scope"))
		return c.Next()
	})
	RegisterRuleReviewRoutes(app, &Deps{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return app, st, fake
}

func reviewRequest(t *testing.T, app *fiber.App, body any, uid string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var raw string
	if literal, ok := body.(string); ok {
		raw = literal
	} else {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		raw = string(b)
	}
	req := httptest.NewRequest("POST", "/api/v1/rule-reviews", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Test-User", uid)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var data map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&data))
	return resp.StatusCode, data
}
func freshReviewBody() map[string]any {
	return map[string]any{"requestId": uuid.NewString(), "reviewedAt": time.Now().UTC().Format(time.RFC3339Nano), "operation": "save", "expectedVersion": 0, "proposed": map[string]any{
		"name": "trip-check", "description": "Load for trip planning.", "body": "Check weather.", "enabled": true,
	}}
}
func editedReviewBody(r map[string]any, op string) map[string]any {
	proposed := map[string]any{"name": r["name"]}
	if op == "save" {
		proposed["description"] = r["description"]
		proposed["body"] = "Check passport."
		proposed["enabled"] = false
	}
	return map[string]any{"requestId": uuid.NewString(), "reviewedAt": time.Now().UTC().Format(time.RFC3339Nano), "operation": op, "expectedVersion": r["version"], "expectedUpdatedAt": r["updatedAt"],
		"expectedRule": map[string]any{"description": r["description"], "body": r["body"], "enabled": r["enabled"]}, "proposed": proposed,
	}
}

func TestRuleReviewHTTPCreateEditDeleteReplayAndIsolation(t *testing.T) {
	app, _, fake := ruleReviewTestApp(t)
	status, got := reviewRequest(t, app, freshReviewBody(), "alice", nil)
	require.Equal(t, 200, status, got)
	require.Equal(t, "saved", got["status"])
	r := got["rule"].(map[string]any)
	require.Equal(t, "user", r["source"])
	require.Equal(t, float64(1), r["version"])
	require.NotNil(t, fake.RawItem("USER#alice", "RULEREVIEW#"+got["reviewId"].(string)))
	status, got = reviewRequest(t, app, freshReviewBody(), "alice", nil)
	require.Equal(t, 409, status, got)
	update := editedReviewBody(r, "save")
	status, got = reviewRequest(t, app, update, "bob", nil)
	require.Equal(t, 409, status, got)
	require.Nil(t, fake.RawItem("USER#bob", "RULE#trip-check"))
	status, got = reviewRequest(t, app, update, "alice", nil)
	require.Equal(t, 200, status, got)
	next := got["rule"].(map[string]any)
	require.Equal(t, float64(2), next["version"])
	require.Equal(t, false, next["enabled"])
	status, got = reviewRequest(t, app, update, "alice", nil)
	require.Equal(t, 409, status, got)
	deletion := editedReviewBody(next, "delete")
	status, got = reviewRequest(t, app, deletion, "alice", nil)
	require.Equal(t, 200, status, got)
	require.Equal(t, "deleted", got["status"])
	require.NotContains(t, got, "rule")
	require.Nil(t, fake.RawItem("USER#alice", "RULE#trip-check"))
	status, _ = reviewRequest(t, app, deletion, "alice", nil)
	require.Equal(t, 409, status)
}

func TestRuleReviewHTTPAuthScopeSurfaceAndCSRF(t *testing.T) {
	cases := []struct {
		name, user string
		headers    map[string]string
		status     int
	}{
		{"anonymous", "", nil, 401},
		{"inactive", "disabled", nil, 403},
		{"not_allowlisted", "member", nil, 403},
		{"scoped", "alice", map[string]string{"Test-Scope": "nova"}, 403},
		{"device", "alice", map[string]string{"Test-Surface": "device"}, 403},
		{"cookie_without_csrf", "alice", map[string]string{"Cookie": RefreshCookieName + "=session"}, 403},
		{"cookie_bad_csrf", "alice", map[string]string{"Cookie": RefreshCookieName + "=session; " + CSRFCookieName + "=a", CSRFHeaderName: "b"}, 403},
		{"web_cookie_match", "alice", map[string]string{"Cookie": RefreshCookieName + "=session; " + CSRFCookieName + "=matching", CSRFHeaderName: "matching"}, 200},
		{"android_bearer", "alice", map[string]string{"Test-Surface": "android"}, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _, fake := ruleReviewTestApp(t)
			status, got := reviewRequest(t, app, freshReviewBody(), tc.user, tc.headers)
			require.Equal(t, tc.status, status, got)
			if status != 200 {
				require.Nil(t, fake.RawItem("USER#alice", "RULE#trip-check"))
			}
		})
	}
}

func TestRuleReviewHTTPRejectsIncompleteOrInjectedApproval(t *testing.T) {
	app, _, fake := ruleReviewTestApp(t)
	cases := []string{
		`{"operation":"save","proposed":{"name":"trip-check","description":"Load for trips.","body":"Hello","enabled":true}}`,
		`{"operation":"save","expectedVersion":null,"proposed":{"name":"trip-check","description":"Load for trips.","body":"Hello","enabled":true}}`,
		`{"operation":"save","expectedVersion":0,"confirm":true,"proposed":{"name":"trip-check","description":"Load for trips.","body":"Hello","enabled":true}}`,
		`{"operation":"save","expectedVersion":0,"userId":"bob","proposed":{"name":"trip-check","description":"Load for trips.","body":"Hello","enabled":true}}`,
		`{"operation":"save","expectedVersion":0,"proposed":{"name":"trip-check","description":"Load for trips.","body":"Hello"}}`,
		`{"operation":"delete","expectedVersion":0,"proposed":{"name":"trip-check"}}`,
		`{"operation":"delete","expectedVersion":1,"expectedUpdatedAt":"now","proposed":{"name":"trip-check"}}`,
		`{"operation":"save","expectedVersion":1,"expectedUpdatedAt":"now","expectedRule":{"description":"before","body":"body"},"proposed":{"name":"trip-check","description":"Load for trips.","body":"Hello","enabled":true}}`,
		`{"operation":"save","expectedVersion":0,"proposed":{"name":"trip-check","description":"Load for trips.","body":"Hello","enabled":true}} {}`,
	}
	for _, raw := range cases {
		status, got := reviewRequest(t, app, raw, "alice", nil)
		require.Equal(t, 400, status, got)
	}
	require.Nil(t, fake.RawItem("USER#alice", "RULE#trip-check"))
}

func TestRuleReviewHTTPRejectsStaleTextEvenWithMatchingVersionAndTimestamp(t *testing.T) {
	app, _, _ := ruleReviewTestApp(t)
	status, got := reviewRequest(t, app, freshReviewBody(), "alice", nil)
	require.Equal(t, 200, status)
	r := got["rule"].(map[string]any)
	update := editedReviewBody(r, "save")
	update["expectedRule"].(map[string]any)["body"] = "A different snapshot."
	status, got = reviewRequest(t, app, update, "alice", nil)
	require.Equal(t, 409, status, got)
	require.Equal(t, "review_conflict", got["error"].(map[string]any)["code"])
}

func TestRuleReviewHTTPConsumedCreateCannotResurrect(t *testing.T) {
	app, _, fake := ruleReviewTestApp(t)
	creation := freshReviewBody()
	status, got := reviewRequest(t, app, creation, "alice", nil)
	require.Equal(t, 200, status, got)
	status, got = reviewRequest(t, app, editedReviewBody(got["rule"].(map[string]any), "delete"), "alice", nil)
	require.Equal(t, 200, status, got)
	status, got = reviewRequest(t, app, creation, "alice", nil)
	require.Equal(t, 409, status, got)
	require.Equal(t, "already_reviewed", got["error"].(map[string]any)["code"])
	require.Nil(t, fake.RawItem("USER#alice", "RULE#trip-check"))
	creation["reviewedAt"] = time.Now().Add(-16 * time.Minute).Format(time.RFC3339Nano)
	status, got = reviewRequest(t, app, creation, "alice", nil)
	require.Equal(t, 409, status, got)
	require.Equal(t, "review_expired", got["error"].(map[string]any)["code"])
	require.Nil(t, fake.RawItem("USER#alice", "RULE#trip-check"))
}

func TestRuleReviewHTTPMalformedValidApprovalNeverMutates(t *testing.T) {
	for _, mode := range []string{"unknown-field", "nested-unknown", "trailing-object", "missing-request", "missing-time"} {
		t.Run(mode, func(t *testing.T) {
			app, _, fake := ruleReviewTestApp(t)
			body := freshReviewBody()
			switch mode {
			case "unknown-field":
				body["confirm"] = true
			case "nested-unknown":
				body["proposed"].(map[string]any)["userId"] = "bob"
			case "missing-request":
				delete(body, "requestId")
			case "missing-time":
				delete(body, "reviewedAt")
			}
			var request any = body
			if mode == "trailing-object" {
				b, err := json.Marshal(body)
				require.NoError(t, err)
				request = string(b) + " {}"
			}
			before := fake.Len()
			status, got := reviewRequest(t, app, request, "alice", nil)
			require.Equal(t, 400, status, got)
			require.Equal(t, before, fake.Len(), "reject must not write a rule or consume an approval receipt")
		})
	}
}
