package webapp

import (
	"context"
	"encoding/json"
	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/mcpauth"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/gofiber/fiber/v2"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func mcpOAuthTestApp(t *testing.T) (*fiber.App, *MCPAuth, string) {
	t.Helper()
	ddb := testutil.NewFakeDynamo()
	st := store.NewWithClient(ddb, "test")
	now := time.Now()
	ctx := context.Background()
	for _, e := range []error{st.CreateUser(ctx, &store.User{UserID: "owner-id", AmazonUserID: "amazon-owner", Role: "owner", Status: store.UserStatusActive}), st.BindOwner(ctx, "amazon-owner", "owner-id"), st.CreateSession(ctx, &store.Session{SessionID: "browser", UserID: "owner-id", FamilyID: "browser-family", Surface: "web", RefreshHash: auth.HashRefreshToken("browser-secret"), CreatedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix(), TTL: now.Add(time.Hour).Unix()})} {
		if e != nil {
			t.Fatal(e)
		}
	}
	a := &MCPAuth{deps: &Deps{Store: st}, service: &mcpauth.Service{Repo: &mcpauth.Dynamo{Client: ddb, Table: "test"}}}
	a.service.OwnerActive = a.service.Repo.(*mcpauth.Dynamo).OwnerActive
	app := fiber.New()
	a.Register(app)
	app.All("/mcp", a.Middleware(), func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"user": UserID(c), "role": Role(c), "surface": Surface(c), "scopes": GrantedScopes(c)})
	})
	app.Get("/outside", a.Middleware(), func(c *fiber.Ctx) error { return c.SendStatus(200) })
	return app, a, RefreshCookieName + "=browser.browser-secret"
}
func mcpOAuthRequest(t *testing.T, app *fiber.App, method, path, body, cookie, origin, authorization string) (*http.Response, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	resp, e := app.Test(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}
func mcpAuthRequest(t *testing.T, a *MCPAuth) (mcpauth.Authorization, string) {
	t.Helper()
	cl, e := a.service.Register(context.Background(), "<script>Bad name</script>", []string{"https://client.example/callback"}, "phone:read jobs:read")
	if e != nil {
		t.Fatal(e)
	}
	verifier := strings.Repeat("x", 43)
	return mcpauth.Authorization{ClientID: cl.ID, RedirectURI: cl.RedirectURIs[0], Resource: mcpauth.Resource, Scope: "phone:read", State: "state-safe", Challenge: mcpauth.Challenge(verifier)}, verifier
}
func mcpQuery(a mcpauth.Authorization) string {
	return url.Values{"response_type": {"code"}, "client_id": {a.ClientID}, "redirect_uri": {a.RedirectURI}, "resource": {a.Resource}, "scope": {a.Scope}, "state": {a.State}, "code_challenge": {a.Challenge}, "code_challenge_method": {"S256"}}.Encode()
}
func mcpBeginConsent(t *testing.T, app *fiber.App, a mcpauth.Authorization, cookie string) (string, string) {
	t.Helper()
	resp, body := mcpOAuthRequest(t, app, "GET", "/oauth/authorize?"+mcpQuery(a), "", cookie, "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("authorize=%d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "<script>Bad name") {
		t.Fatal("unescaped client metadata")
	}
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)
	if len(csrf) != 2 {
		t.Fatal("missing csrf")
	}
	for _, c := range resp.Cookies() {
		if c.Name == mcpCookie {
			if !c.Secure || !c.HttpOnly || c.Path != "/" {
				t.Fatal("insecure request cookie")
			}
			return cookie + "; " + mcpCookie + "=" + c.Value, csrf[1]
		}
	}
	t.Fatal("missing cookie")
	return "", ""
}
func TestMCPOAuthBrowserConsentAndTokenFlow(t *testing.T) {
	app, a, cookie := mcpOAuthTestApp(t)
	req, verifier := mcpAuthRequest(t, a)
	cookie, csrf := mcpBeginConsent(t, app, req, cookie)
	resp, body := mcpOAuthRequest(t, app, "POST", "/oauth/authorize", url.Values{"csrf": {csrf}, "decision": {"approve"}}.Encode(), cookie, mcpauth.Issuer, "")
	u := mcpCallbackURL(t, resp, body)
	if u.Scheme != "https" || u.Host != "client.example" || u.Query().Get("state") != "state-safe" || u.Query().Get("iss") != mcpauth.Issuer {
		t.Fatal("callback not bound")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {req.ClientID}, "redirect_uri": {req.RedirectURI}, "resource": {mcpauth.Resource}, "code": {u.Query().Get("code")}, "code_verifier": {verifier}}
	resp, body = mcpOAuthRequest(t, app, "POST", "/oauth/token", form.Encode(), "", "", "")
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("token %d %s", resp.StatusCode, body)
	}
	var tokens mcpauth.Tokens
	if json.Unmarshal([]byte(body), &tokens) != nil {
		t.Fatal("invalid token response")
	}
	resp, body = mcpOAuthRequest(t, app, "POST", "/mcp", "", "", "", "Bearer "+tokens.AccessToken)
	if resp.StatusCode != 200 || !strings.Contains(body, "owner-id") || !strings.Contains(body, "phone:read") {
		t.Fatalf("MCP %d %s", resp.StatusCode, body)
	}
	resp, _ = mcpOAuthRequest(t, app, "GET", "/outside", "", "", "", "Bearer "+tokens.AccessToken)
	if resp.StatusCode != 403 {
		t.Fatal("token escaped MCP route")
	}
	resp, _ = mcpOAuthRequest(t, app, "POST", "/oauth/token", form.Encode(), "", "", "")
	if resp.StatusCode != 400 {
		t.Fatal("code replay")
	}
}
func TestMCPOAuthConsentRejectsForgery(t *testing.T) {
	for _, kind := range []string{"wrong_origin", "missing_origin", "missing_cookie", "wrong_csrf", "duplicate_csrf"} {
		t.Run(kind, func(t *testing.T) {
			app, a, cookie := mcpOAuthTestApp(t)
			req, _ := mcpAuthRequest(t, a)
			cookie, csrf := mcpBeginConsent(t, app, req, cookie)
			origin := mcpauth.Issuer
			switch kind {
			case "wrong_origin":
				origin = "https://evil.example"
			case "missing_origin":
				origin = ""
			case "missing_cookie":
				cookie = ""
			case "wrong_csrf":
				csrf = "wrong"
			}
			form := url.Values{"csrf": {csrf}, "decision": {"approve"}}
			if kind == "duplicate_csrf" {
				form.Add("csrf", "other")
			}
			resp, _ := mcpOAuthRequest(t, app, "POST", "/oauth/authorize", form.Encode(), cookie, origin, "")
			if resp.StatusCode < 400 {
				t.Fatal("forged consent accepted")
			}
		})
	}
}
func TestMCPOAuthLoginReturnIsFixedAndPending(t *testing.T) {
	app, a, _ := mcpOAuthTestApp(t)
	req, _ := mcpAuthRequest(t, a)
	resp, _ := mcpOAuthRequest(t, app, "GET", "/oauth/authorize?"+mcpQuery(req), "", "", "", "")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/auth/lwa/login" {
		t.Fatal("missing browser login redirect")
	}
	resp, _ = mcpOAuthRequest(t, app, "GET", "/oauth/authorize", "", "", "", "")
	if resp.StatusCode != 400 {
		t.Fatal("missing request accepted")
	}
}
func TestMCPOAuthMetadataAndDCR(t *testing.T) {
	app, _, _ := mcpOAuthTestApp(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server"} {
		resp, body := mcpOAuthRequest(t, app, "GET", path, "", "", "", "")
		if resp.StatusCode != 200 || !strings.Contains(body, mcpauth.Issuer) {
			t.Fatal(path)
		}
		// ghost-job-control: both ghost scopes are advertised wherever
		// scopes_supported is published, beside the original six.
		for _, scope := range []string{"phone:send", "ghost:read", "ghost:manage"} {
			if !strings.Contains(body, `"`+scope+`"`) {
				t.Fatal(path, "missing scope", scope)
			}
		}
	}
	r := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(`{"client_name":"test","redirect_uris":["https://client.example/callback"],"token_endpoint_auth_method":"none"}`))
	r.Header.Set("Content-Type", "application/json")
	resp, e := app.Test(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatal(resp.StatusCode)
	}
}
func TestMCPOAuthBearerChallengeAndNoCookieAuthentication(t *testing.T) {
	app, _, cookie := mcpOAuthTestApp(t)
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://evil.example")
	badOrigin, err := app.Test(req)
	if err != nil || badOrigin.StatusCode != 403 {
		t.Fatal("invalid origin must be rejected before bearer processing")
	}
	badOrigin.Body.Close()
	for _, x := range []struct{ path, auth, cookie string }{{"/mcp", "", ""}, {"/mcp", "Bearer not-valid", ""}, {"/mcp", "", cookie}, {"/mcp?access_token=query-token", "", ""}} {
		resp, _ := mcpOAuthRequest(t, app, "POST", x.path, "", x.cookie, "", x.auth)
		if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp") {
			t.Fatal("missing proper challenge")
		}
	}
}
func TestMCPOAuthRejectsPlainPKCEAndDuplicateParameters(t *testing.T) {
	app, a, cookie := mcpOAuthTestApp(t)
	req, _ := mcpAuthRequest(t, a)
	for _, q := range []string{strings.Replace(mcpQuery(req), "code_challenge_method=S256", "code_challenge_method=plain", 1), mcpQuery(req) + "&client_id=other"} {
		resp, _ := mcpOAuthRequest(t, app, "GET", "/oauth/authorize?"+q, "", cookie, "", "")
		if resp.StatusCode != 400 {
			t.Fatal("unsafe auth parameters")
		}
	}
}

func TestMCPOAuthConsentPolicyKeepsFormOnIssuer(t *testing.T) {
	// The consent POST returns a navigation page instead of a redirect, so the
	// form never needs the client's origin, and the client's own onward hops
	// (chatgpt.com to its app scheme) are not form-action checked.
	app, a, cookie := mcpOAuthTestApp(t)
	req, _ := mcpAuthRequest(t, a)
	resp, _ := mcpOAuthRequest(t, app, "GET", "/oauth/authorize?"+mcpQuery(req), "", cookie, "", "")
	if resp.StatusCode != 200 {
		t.Fatal("consent page unavailable")
	}
	sources := ""
	for _, directive := range strings.Split(resp.Header.Get("Content-Security-Policy"), ";") {
		if strings.HasPrefix(strings.TrimSpace(directive), "form-action ") {
			sources = strings.TrimSpace(directive)
		}
	}
	if sources != "form-action 'self'" {
		t.Fatal("consent form policy:", sources)
	}
}

// mcpCallbackURL reads the consent result page's navigation target and checks
// that the page carries no form-action restriction that could block the
// client's own onward redirect.
func mcpCallbackURL(t *testing.T, resp *http.Response, body string) *url.URL {
	t.Helper()
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" || strings.Contains(resp.Header.Get("Content-Security-Policy"), "form-action") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	m := regexp.MustCompile(`<a href="([^"]+)">Continue</a>`).FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, `http-equiv="refresh" content="0;url=`+m[1]+`"`) {
		t.Fatalf("missing callback navigation: %s", body)
	}
	u, e := url.Parse(html.UnescapeString(m[1]))
	if e != nil {
		t.Fatal(e)
	}
	return u
}
