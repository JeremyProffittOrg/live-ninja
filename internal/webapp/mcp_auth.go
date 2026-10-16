package webapp

import (
	"bytes"
	"context"
	"errors"
	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/mcpauth"
	"github.com/gofiber/fiber/v2"
	"html/template"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const mcpCookie = "__Host-ln_mcp"

type MCPAuth struct {
	deps    *Deps
	service *mcpauth.Service
}

func NewMCPAuth(deps *Deps) (*MCPAuth, error) {
	if deps == nil || deps.Store == nil {
		return nil, errors.New("MCP OAuth requires app store")
	}
	repo, e := mcpauth.NewDynamo(context.Background(), deps.Cfg.TableName)
	if e != nil {
		return nil, e
	}
	a := &MCPAuth{deps: deps, service: &mcpauth.Service{Repo: repo}}
	a.service.OwnerActive = repo.OwnerActive
	return a, nil
}
func (a *MCPAuth) ownerActive(ctx context.Context, uid string, issued int64) bool {
	return a.service.OwnerActive != nil && a.service.OwnerActive(ctx, uid, issued)
}
func (a *MCPAuth) browserOwner(c *fiber.Ctx) string {
	uid := resolveWebSessionUser(c, a.deps)
	if uid == "" {
		return ""
	}
	sid, secret, ok := splitWireRefresh(c.Cookies(RefreshCookieName))
	if !ok {
		return ""
	}
	sess, e := a.deps.Store.GetSessionForUser(c.Context(), uid, sid)
	if e != nil || sess == nil || sess.Surface != "web" || sess.ExpiresAt <= time.Now().Unix() || !subtleEquals(auth.HashRefreshToken(secret), sess.RefreshHash) || !a.ownerActive(c.Context(), uid, sess.CreatedAt) {
		return ""
	}
	return uid
}
func (a *MCPAuth) Register(app *fiber.App) {
	app.Get("/.well-known/oauth-protected-resource/mcp", a.resourceMetadata)
	app.Get("/.well-known/oauth-protected-resource", a.resourceMetadata)
	app.Get("/.well-known/oauth-authorization-server", a.serverMetadata)
	app.Post("/oauth/register", a.register)
	app.Get("/oauth/authorize", a.authorize)
	app.Post("/oauth/authorize", a.consent)
	app.Post("/oauth/token", a.token)
	app.Post("/oauth/revoke", a.revoke)
}
func GrantedScopes(c *fiber.Ctx) []string {
	v, _ := c.Locals("mcpScopes").([]string)
	return append([]string(nil), v...)
}
func (a *MCPAuth) Middleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		c.Set("Cache-Control", "no-store")
		if c.Path() != "/mcp" {
			return c.SendStatus(403)
		}
		if origin := c.Get("Origin"); origin != "" && origin != mcpauth.Issuer {
			return c.SendStatus(403)
		}
		raw := c.Get("Authorization")
		if !strings.HasPrefix(raw, "Bearer ") || c.Query("access_token") != "" {
			return a.challenge(c)
		}
		id, e := a.service.Verify(c.Context(), strings.TrimPrefix(raw, "Bearer "))
		if e != nil {
			return a.challenge(c)
		}
		c.Locals(localUserID, id.UserID)
		c.Locals(localSessionID, id.GrantID)
		c.Locals(localRole, "owner")
		c.Locals(localSurface, "mcp")
		c.Locals(localScope, "mcp")
		c.Locals("mcpScopes", strings.Fields(id.Scope))
		c.Locals("mcpClientID", id.ClientID)
		return c.Next()
	}
}
func (a *MCPAuth) challenge(c *fiber.Ctx) error {
	c.Set("WWW-Authenticate", `Bearer resource_metadata="`+mcpauth.Issuer+`/.well-known/oauth-protected-resource/mcp"`)
	return c.Status(401).JSON(fiber.Map{"error": "invalid_token"})
}
func (a *MCPAuth) resourceMetadata(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"resource": mcpauth.Resource, "authorization_servers": []string{mcpauth.Issuer}, "scopes_supported": mcpauth.Scopes, "bearer_methods_supported": []string{"header"}})
}
func (a *MCPAuth) serverMetadata(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"issuer": mcpauth.Issuer, "authorization_endpoint": mcpauth.Issuer + "/oauth/authorize", "token_endpoint": mcpauth.Issuer + "/oauth/token", "registration_endpoint": mcpauth.Issuer + "/oauth/register", "revocation_endpoint": mcpauth.Issuer + "/oauth/revoke",
		"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none"}, "revocation_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": mcpauth.Scopes, "authorization_response_iss_parameter_supported": true})
}
func oauthError(c *fiber.Ctx, e error) error {
	code := "server_error"
	status := 500
	for _, known := range []error{mcpauth.ErrInvalidGrant, mcpauth.ErrInvalidClient, mcpauth.ErrInvalidRequest, mcpauth.ErrInvalidScope, mcpauth.ErrInvalidTarget} {
		if errors.Is(e, known) {
			code = known.Error()
			status = 400
			break
		}
	}
	c.Set("Cache-Control", "no-store")
	c.Set("Pragma", "no-cache")
	return c.Status(status).JSON(fiber.Map{"error": code})
}
func (a *MCPAuth) register(c *fiber.Ctx) error {
	c.Set("Cache-Control", "no-store")
	if len(c.Body()) > 8192 || !strings.HasPrefix(c.Get("Content-Type"), "application/json") {
		return oauthError(c, mcpauth.ErrInvalidRequest)
	}
	var b struct {
		ClientName    string   `json:"client_name"`
		RedirectURIs  []string `json:"redirect_uris"`
		Method        string   `json:"token_endpoint_auth_method"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
		Scope         string   `json:"scope"`
	}
	if c.BodyParser(&b) != nil || b.Method != "" && b.Method != "none" {
		return oauthError(c, mcpauth.ErrInvalidRequest)
	}
	for _, v := range b.GrantTypes {
		if v != "authorization_code" && v != "refresh_token" {
			return oauthError(c, mcpauth.ErrInvalidRequest)
		}
	}
	for _, v := range b.ResponseTypes {
		if v != "code" {
			return oauthError(c, mcpauth.ErrInvalidRequest)
		}
	}
	if !a.service.RegistrationLimit(c.Context(), c.IP()) {
		return c.Status(429).JSON(fiber.Map{"error": "rate_limit_exceeded"})
	}
	client, e := a.service.Register(c.Context(), b.ClientName, b.RedirectURIs, b.Scope)
	if e != nil {
		return oauthError(c, e)
	}
	return c.Status(201).JSON(fiber.Map{"client_id": client.ID, "client_id_issued_at": time.Now().Unix(), "client_name": client.Name, "redirect_uris": client.RedirectURIs, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "scope": client.Scope})
}
func singleValues(raw string) (url.Values, error) {
	v, e := url.ParseQuery(raw)
	if e != nil {
		return nil, mcpauth.ErrInvalidRequest
	}
	for _, values := range v {
		if len(values) != 1 {
			return nil, mcpauth.ErrInvalidRequest
		}
	}
	return v, nil
}
func mcpReturnPending(c *fiber.Ctx) bool {
	return regexp.MustCompile("^[A-Za-z0-9_-]{43}$").MatchString(c.Cookies(mcpCookie))
}
func (a *MCPAuth) authorize(c *fiber.Ctx) error {
	c.Set("Cache-Control", "no-store")
	c.Set("Referrer-Policy", "no-referrer")
	id := c.Cookies(mcpCookie)
	if len(c.Context().QueryArgs().QueryString()) > 0 {
		q, e := singleValues(string(c.Context().QueryArgs().QueryString()))
		if e != nil {
			return oauthError(c, e)
		}
		if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
			return oauthError(c, mcpauth.ErrInvalidRequest)
		}
		req := mcpauth.Authorization{ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), Resource: q.Get("resource"), Scope: q.Get("scope"), State: q.Get("state"), Challenge: q.Get("code_challenge")}
		id, _, e = a.service.Begin(c.Context(), req)
		if e != nil {
			return oauthError(c, e)
		}
		c.Cookie(&fiber.Cookie{Name: mcpCookie, Value: id, Path: "/", Secure: true, HTTPOnly: true, SameSite: "Lax", MaxAge: 600})
	}
	if id == "" {
		return oauthError(c, mcpauth.ErrInvalidRequest)
	}
	if _, e := a.service.Pending(c.Context(), id); e != nil {
		return oauthError(c, e)
	}
	uid := a.browserOwner(c)
	if uid == "" {
		if resolveWebSessionUser(c, a.deps) != "" {
			return c.Status(403).SendString("Only the Live Ninja owner can approve this connection.")
		}
		return c.Redirect("/auth/lwa/login", 302)
	}
	pending, csrf, e := a.service.BindConsent(c.Context(), id, uid)
	if e != nil {
		return oauthError(c, e)
	}
	client, e := a.service.Client(c.Context(), pending.Request.ClientID)
	if e != nil {
		return oauthError(c, e)
	}
	c.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	c.Set("X-Frame-Options", "DENY")
	c.Type("html")
	var out bytes.Buffer
	e = consentPage.Execute(&out, map[string]any{"Name": client.Name, "ClientID": client.ID, "Redirect": pending.Request.RedirectURI, "Scopes": mcpScopeDescriptions(pending.Request.Scope), "CSRF": csrf})
	if e != nil {
		return oauthError(c, e)
	}
	return c.Send(out.Bytes())
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Connect Live Ninja</title><body><main><h1>Connect this client to Live Ninja?</h1><p>Client-supplied name: {{.Name}}</p><p>Client identifier: {{.ClientID}}</p><p>Return address: {{.Redirect}}</p><p>This client will act as you with these permissions:</p><ul>{{range .Scopes}}<li>{{.}}</li>{{end}}</ul><p>Approve only if you started this connection and recognize the return address.</p><form method="post" action="/oauth/authorize"><input type="hidden" name="csrf" value="{{.CSRF}}"><button name="decision" value="approve">Allow</button><button name="decision" value="deny">Cancel</button></form></main></body></html>`))

func (a *MCPAuth) consent(c *fiber.Ctx) error {
	if c.Get("Origin") != mcpauth.Issuer {
		return c.SendStatus(403)
	}
	if len(c.Body()) > 4096 || !strings.HasPrefix(c.Get("Content-Type"), "application/x-www-form-urlencoded") {
		return oauthError(c, mcpauth.ErrInvalidRequest)
	}
	form, e := singleValues(string(c.Body()))
	if e != nil {
		return oauthError(c, e)
	}
	decision := form.Get("decision")
	if decision != "approve" && decision != "deny" {
		return oauthError(c, mcpauth.ErrInvalidRequest)
	}
	uid := a.browserOwner(c)
	if uid == "" {
		return c.SendStatus(401)
	}
	req, code, e := a.service.Consent(c.Context(), c.Cookies(mcpCookie), form.Get("csrf"), uid, decision == "approve")
	if e != nil {
		return oauthError(c, e)
	}
	c.Cookie(&fiber.Cookie{Name: mcpCookie, Value: "", Path: "/", Secure: true, HTTPOnly: true, SameSite: "Lax", MaxAge: -1})
	u, _ := url.Parse(req.RedirectURI)
	q := u.Query()
	q.Set("state", req.State)
	q.Set("iss", mcpauth.Issuer)
	if code == "" {
		q.Set("error", "access_denied")
	} else {
		q.Set("code", code)
	}
	u.RawQuery = q.Encode()
	c.Set("Cache-Control", "no-store")
	c.Set("Referrer-Policy", "no-referrer")
	return c.Redirect(u.String(), 302)
}
func tokenForm(c *fiber.Ctx) (url.Values, error) {
	if len(c.Body()) > 8192 || !strings.HasPrefix(c.Get("Content-Type"), "application/x-www-form-urlencoded") || c.Get("Authorization") != "" {
		return nil, mcpauth.ErrInvalidRequest
	}
	form, e := singleValues(string(c.Body()))
	if e != nil {
		return nil, e
	}
	if form.Get("client_id") == "" || form.Get("client_secret") != "" {
		return nil, mcpauth.ErrInvalidClient
	}
	return form, nil
}
func (a *MCPAuth) token(c *fiber.Ctx) error {
	c.Set("Cache-Control", "no-store")
	c.Set("Pragma", "no-cache")
	f, e := tokenForm(c)
	if e != nil {
		return oauthError(c, e)
	}
	var t *mcpauth.Tokens
	switch f.Get("grant_type") {
	case "authorization_code":
		t, e = a.service.Exchange(c.Context(), f.Get("code"), f.Get("client_id"), f.Get("redirect_uri"), f.Get("code_verifier"), f.Get("resource"))
	case "refresh_token":
		t, e = a.service.Refresh(c.Context(), f.Get("refresh_token"), f.Get("client_id"), f.Get("resource"), f.Get("scope"))
	default:
		return c.Status(400).JSON(fiber.Map{"error": "unsupported_grant_type"})
	}
	if e != nil {
		return oauthError(c, e)
	}
	return c.JSON(t)
}
func (a *MCPAuth) revoke(c *fiber.Ctx) error {
	c.Set("Cache-Control", "no-store")
	f, e := tokenForm(c)
	if e != nil {
		return oauthError(c, e)
	}
	if e = a.service.Revoke(c.Context(), f.Get("token"), f.Get("client_id")); e != nil {
		return oauthError(c, e)
	}
	return c.SendStatus(200)
}

func mcpScopeDescriptions(scope string) []string {
	labels := map[string]string{"phone:send": "Send messages and start calls.", "phone:read": "Read phone history and recordings.", "jobs:read": "Read jobs and their results.", "jobs:manage": "Create and manage jobs.", "memory:read": "Read your saved memory.", "memory:write": "Add, change, or remove saved memory."}
	out := []string{}
	for _, v := range strings.Fields(scope) {
		out = append(out, labels[v])
	}
	return out
}
