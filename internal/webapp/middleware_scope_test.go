package webapp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

const scopedAuthRequestContext = `{"authorizer":{"lambda":{"userId":"u1","sessionId":"sess-1","surface":"web","deviceId":"","role":"owner"}}}`

func newScopedAuthTestApp(t *testing.T) (*fiber.App, *auth.Signer, *testutil.FakeDynamo) {
	t.Helper()

	fakeDDB := testutil.NewFakeDynamo()
	st := store.NewWithClient(fakeDDB, "live-ninja-test")
	require.NoError(t, st.CreateUser(context.Background(), &store.User{
		UserID:       "u1",
		AmazonUserID: "amazon-u1",
		Email:        "u1@example.com",
		Name:         "Test User",
		Role:         store.RoleOwner,
		Status:       store.UserStatusActive,
	}))
	fakeDDB.SeedItem(map[string]ddbtypes.AttributeValue{
		"pk":         &ddbtypes.AttributeValueMemberS{Value: "USER#u1"},
		"sk":         &ddbtypes.AttributeValueMemberS{Value: "BUCKET#sess#sess-1"},
		"exp":        &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10)},
		"redeemedAt": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Unix(), 10)},
	})
	fakeKMS, err := testutil.NewFakeKMS()
	require.NoError(t, err)
	signer := auth.NewSignerWithClient(fakeKMS, "arn:aws:kms:us-east-1:000000000000:key/test-key")
	deps := &Deps{
		Store:  st,
		Signer: signer,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	app := fiber.New()
	app.Use(ExtractAuthContext(deps))
	app.Use(RequireAuth())
	app.All("/*", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})
	return app, signer, fakeDDB
}

func scopedAuthToken(t *testing.T, signer *auth.Signer, scope, surface, deviceID string) string {
	t.Helper()
	now := time.Now().Unix()
	token, err := signer.SignAccessToken(context.Background(), auth.Claims{
		Sub:     "u1",
		Sid:     "sess-1",
		Surface: surface,
		Did:     deviceID,
		Scope:   scope,
		Iat:     now,
		Exp:     now + 300,
	})
	require.NoError(t, err)
	return token
}

func scopedAuthRequest(t *testing.T, app *fiber.App, method, target, token, requestContext string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
	if requestContext != "" {
		req.Header.Set("x-amzn-request-context", requestContext)
	}
	resp, err := app.Test(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}

func TestNovaScopedBearerIsLimitedToExactBridgeSinks(t *testing.T) {
	app, signer, _ := newScopedAuthTestApp(t)
	token := scopedAuthToken(t, signer, realtime.NovaScope, store.SurfaceWeb, "")

	allowed := []string{
		"/api/v1/transcript",
		"/api/v1/transcript?retry=1",
		"/api/v1/tools/invoke",
	}
	for _, target := range allowed {
		t.Run("allow POST "+target, func(t *testing.T) {
			resp, body := scopedAuthRequest(t, app, http.MethodPost, target, token, "")
			require.Equal(t, fiber.StatusNoContent, resp.StatusCode, body)
		})
	}

	blocked := []struct {
		method string
		target string
	}{
		{http.MethodGet, "/api/v1/transcript"},
		{http.MethodOptions, "/api/v1/transcript"},
		{http.MethodPut, "/api/v1/transcript"},
		{http.MethodPost, "/api/v1/transcript/"},
		{http.MethodPost, "/v1/transcript"},
		{http.MethodGet, "/api/v1/tools/invoke"},
		{http.MethodPost, "/api/v1/tools/invoke/child"},
		{http.MethodGet, "/api/v1/me"},
		{http.MethodPost, "/api/v1/fallback/turn"},
		{http.MethodPost, "/auth/logout"},
	}
	for _, tc := range blocked {
		t.Run("reject "+tc.method+" "+tc.target, func(t *testing.T) {
			resp, body := scopedAuthRequest(t, app, tc.method, tc.target, token, "")
			require.Equal(t, fiber.StatusForbidden, resp.StatusCode, body)
			require.Contains(t, body, `"code":"insufficient_scope"`)
		})
	}
}

func TestNovaScopedBearerRequiresLiveRedeemedSession(t *testing.T) {
	app, signer, fakeDDB := newScopedAuthTestApp(t)
	token := scopedAuthToken(t, signer, realtime.NovaScope, store.SurfaceWeb, "")

	for name, item := range map[string]map[string]ddbtypes.AttributeValue{
		"expired": {
			"pk":         &ddbtypes.AttributeValueMemberS{Value: "USER#u1"},
			"sk":         &ddbtypes.AttributeValueMemberS{Value: "BUCKET#sess#sess-1"},
			"exp":        &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)},
			"redeemedAt": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Unix(), 10)},
		},
		"not redeemed": {
			"pk":  &ddbtypes.AttributeValueMemberS{Value: "USER#u1"},
			"sk":  &ddbtypes.AttributeValueMemberS{Value: "BUCKET#sess#sess-1"},
			"exp": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			fakeDDB.SeedItem(item)
			resp, body := scopedAuthRequest(t, app, http.MethodPost, "/api/v1/transcript", token, "")
			require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode, body)
			require.Contains(t, body, `"code":"session_inactive"`)
		})
	}
}

func TestNovaScopeGuardAppliesToAuthorizerContextPath(t *testing.T) {
	app, signer, _ := newScopedAuthTestApp(t)
	token := scopedAuthToken(t, signer, realtime.NovaScope, store.SurfaceWeb, "")

	resp, body := scopedAuthRequest(t, app, http.MethodGet, "/api/v1/me", token, scopedAuthRequestContext)
	require.Equal(t, fiber.StatusForbidden, resp.StatusCode, body)
	require.Contains(t, body, `"code":"insufficient_scope"`)

	resp, body = scopedAuthRequest(t, app, http.MethodPost, "/api/v1/transcript", token, scopedAuthRequestContext)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode, body)
}

func TestNovaScopeGuardFailsClosedOnInvalidOrMismatchedScopedToken(t *testing.T) {
	app, signer, _ := newScopedAuthTestApp(t)
	token := scopedAuthToken(t, signer, realtime.NovaScope, store.SurfaceWeb, "")
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	if parts[2][0] == 'A' {
		parts[2] = "B" + parts[2][1:]
	} else {
		parts[2] = "A" + parts[2][1:]
	}
	tampered := strings.Join(parts, ".")

	resp, body := scopedAuthRequest(t, app, http.MethodPost, "/api/v1/transcript", tampered, scopedAuthRequestContext)
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode, body)
	require.Contains(t, body, `"code":"invalid_token"`)

	mismatchedContext := strings.Replace(scopedAuthRequestContext, `"sess-1"`, `"different-session"`, 1)
	resp, body = scopedAuthRequest(t, app, http.MethodPost, "/api/v1/transcript", token, mismatchedContext)
	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode, body)
	require.Contains(t, body, `"code":"invalid_token"`)
}

func TestNormalSessionAndDeviceBearersKeepGeneralAccess(t *testing.T) {
	app, signer, _ := newScopedAuthTestApp(t)
	webToken := scopedAuthToken(t, signer, store.RoleOwner, store.SurfaceWeb, "")
	tokens := map[string]string{
		"web session": webToken,
		"device":      scopedAuthToken(t, signer, "", store.SurfaceDevice, "thing-1"),
	}

	for name, token := range tokens {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				method string
				target string
			}{
				{http.MethodGet, "/api/v1/me"},
				{http.MethodPost, "/api/v1/fallback/turn"},
				{http.MethodPost, "/api/v1/transcript"},
			} {
				resp, body := scopedAuthRequest(t, app, tc.method, tc.target, token, "")
				require.Equal(t, fiber.StatusNoContent, resp.StatusCode, body)
			}
		})
	}

	// The API Gateway context fast path is unchanged for an ordinary access
	// token as well; only a candidate scope=nova credential takes the extra
	// local verification and endpoint-capability checks.
	resp, body := scopedAuthRequest(t, app, http.MethodGet, "/api/v1/me", webToken, scopedAuthRequestContext)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode, body)
}
