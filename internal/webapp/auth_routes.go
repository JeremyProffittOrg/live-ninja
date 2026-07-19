package webapp

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// Refresh-token wire format: "<sessionId>.<secret>". The opaque secret
// alone can't locate its SESSION row (lookup is by sessionId via GSI1), so
// the wire form carries both — the cookie for web, the refreshToken JSON
// field for Android/device. Only the secret is credential material; the
// sessionId half is public-ish (it's also in the JWT sid claim).

// pollIntervalSeconds is the device-pairing poll cadence hint returned to
// devices on register/poll responses.
const pollIntervalSeconds = 5

// PairConfirmCookieName binds the device-pairing user-code confirm form to
// the browser that completed the LWA leg: the device callback sets it to
// the same one-shot token embedded in the form, and the confirm POST
// requires an exact match — so a leaked/forwarded form URL or token alone
// cannot be replayed from another browser. HttpOnly; __Host- prefix rules
// (Secure, Path=/, no Domain) as with every other auth cookie.
const PairConfirmCookieName = "__Host-ln_pair"

// pairFailedReason is the machine-readable reason the device poll reports
// with the terminal "failed" status after too many wrong user codes.
const pairFailedReason = "user_code_attempts_exceeded"

// appReturnScheme is the Android app's custom-scheme return target. The
// broker callback 302s here with a one-shot handoff code; the app's
// intent-filter (ninja.jeremy.liveninja://lwa) catches it. LWA itself never
// sees this scheme — it only ever redirects to the whitelisted
// /auth/lwa/callback, so no custom-scheme return URL has to be whitelisted
// in the Amazon Developer Portal (which only accepts http/https anyway).
const appReturnScheme = "ninja.jeremy.liveninja://lwa"

// RegisterAuthRoutes mounts the M1 auth surface on app. Route names follow
// the shared spec's M1+M2 route list; the contracts/api.md canonical
// /auth/* spellings are mounted as aliases onto the same handlers so both
// inventories resolve (all /auth/* and /api/v1/auth/* paths here are on
// the authorizer's public list — the handlers do their own credential
// validation: OAuth state, refresh token, pairing nonce + PKCE).
func RegisterAuthRoutes(app *fiber.App, deps *Deps) {
	r := &authRoutes{deps: deps}

	app.Get("/.well-known/jwks.json", r.jwks)

	app.Get("/auth/lwa/login", r.login)
	app.Get("/auth/lwa/callback", r.callback)

	// Android broker sign-in: no LWA-portal redirect URI needed — the app
	// rides the whitelisted /auth/lwa/callback and gets a one-shot handoff
	// code back via its custom scheme, claimed here for a real session.
	app.Get("/auth/lwa/app-login", r.appLogin)
	app.Post("/auth/lwa/app-claim", r.appClaim)

	app.Post("/api/v1/auth/lwa/exchange", r.exchange)
	app.Post("/auth/lwa/exchange", r.exchange) // contracts/api.md canonical alias

	app.Post("/api/v1/auth/refresh", r.refresh)
	app.Post("/auth/refresh", r.refresh)

	app.Post("/api/v1/auth/logout", r.logout)
	app.Post("/auth/logout", r.logout)

	app.Post("/api/v1/auth/logout-all", RequireAuth(), r.logoutAll)

	app.Post("/api/v1/auth/device/register", r.deviceRegister)
	app.Post("/auth/device/pair/start", r.deviceRegister)

	app.Post("/api/v1/auth/device/poll", r.devicePoll)
	app.Post("/auth/device/pair/poll", r.devicePoll)

	app.Get("/auth/device/claim", r.deviceClaim)
	app.Post("/auth/device/pair/confirm", r.deviceConfirm)
}

type authRoutes struct {
	deps *Deps
}

// ---- JWKS ----

func (r *authRoutes) jwks(c *fiber.Ctx) error {
	doc, err := r.deps.Signer.JWKS(c.Context())
	if err != nil {
		r.deps.Log.Error("jwks: fetch failed", "error", err.Error())
		return errorJSON(c, fiber.StatusInternalServerError, "jwks_unavailable", "Could not load the signing keys.")
	}
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	c.Set(fiber.HeaderCacheControl, "public, max-age=3600")
	return c.Send(doc)
}

// ---- LWA web flow ----

// login starts the LWA Authorization Code + PKCE flow for the web surface.
// An optional ?device_nonce= carries a device-pairing nonce through the
// round-trip (the browser leg of the M5Stack pairing flow) — the callback
// then binds the device instead of opening a web session.
func (r *authRoutes) login(c *fiber.Ctx) error {
	state, err := randomURLToken(32)
	if err != nil {
		return fmt.Errorf("generate oauth state: %w", err)
	}
	verifier, err := randomURLToken(32)
	if err != nil {
		return fmt.Errorf("generate pkce verifier: %w", err)
	}

	redirectURI := r.baseURL(c) + "/auth/lwa/callback"
	st := &store.OAuthState{
		State:        state,
		CodeVerifier: verifier,
		Surface:      store.SurfaceWeb,
		RedirectURI:  redirectURI,
		DeviceNonce:  c.Query("device_nonce"),
	}
	if err := r.deps.Store.PutOAuthState(c.Context(), st); err != nil {
		r.deps.Log.Error("login: put oauth state failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusInternalServerError, "Sign-in unavailable",
			"Could not start sign-in. Please try again in a moment.")
	}

	// Bind this transaction to the initiating browser (see OAuthStateCookieName).
	c.Cookie(&fiber.Cookie{
		Name:     OAuthStateCookieName,
		Value:    state,
		Path:     "/",
		Secure:   true,
		HTTPOnly: true,
		SameSite: "Lax",
		MaxAge:   600,
	})

	return c.Redirect(r.deps.LWA.BuildAuthorizeURL(state, s256Challenge(verifier), redirectURI),
		fiber.StatusFound)
}

// callback completes the LWA round-trip: one-shot state consumption,
// confidential code exchange, two-check validation, then either the web
// session cookie flow or the device-pairing bind (when the consumed state
// carries a DeviceNonce).
func (r *authRoutes) callback(c *fiber.Ctx) error {
	ctx := c.Context()

	if lwaErr := c.Query("error"); lwaErr != "" {
		return htmlMessage(c, fiber.StatusForbidden, "Sign-in cancelled",
			"Amazon reported: "+html.EscapeString(lwaErr)+". You can close this page and try again.")
	}
	code, state := c.Query("code"), c.Query("state")
	if code == "" || state == "" {
		return htmlMessage(c, fiber.StatusBadRequest, "Invalid sign-in response",
			"The sign-in response was missing required parameters.")
	}

	// The state must match the cookie set on THIS browser at login — proves the
	// browser completing the callback is the one that started the flow (blocks
	// login-CSRF / session fixation). Device-pairing callbacks arrive on the
	// device's own browser leg with the same cookie, so this applies uniformly.
	stateCookie := c.Cookies(OAuthStateCookieName)
	c.ClearCookie(OAuthStateCookieName)
	if stateCookie == "" || subtle.ConstantTimeCompare([]byte(stateCookie), []byte(state)) != 1 {
		return htmlMessage(c, fiber.StatusBadRequest, "Sign-in link expired",
			"This sign-in attempt could not be verified in this browser. Please start again.")
	}

	st, err := r.deps.Store.GetOAuthState(ctx, state)
	if err != nil {
		r.deps.Log.Error("callback: consume oauth state failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusInternalServerError, "Sign-in failed",
			"Something went wrong completing sign-in. Please try again.")
	}
	if st == nil {
		return htmlMessage(c, fiber.StatusBadRequest, "Sign-in link expired",
			"This sign-in attempt has expired or was already used. Please start again.")
	}

	tokens, err := r.deps.LWA.ExchangeCode(ctx, code, st.CodeVerifier, st.RedirectURI)
	if err != nil {
		r.deps.Log.Error("callback: lwa code exchange failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusBadGateway, "Sign-in failed",
			"Amazon did not accept the sign-in. Please try again.")
	}
	profile, err := r.deps.LWA.Validate(ctx, tokens.AccessToken)
	if err != nil {
		r.deps.Log.Error("callback: lwa validate failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusBadGateway, "Sign-in failed",
			"Your Amazon sign-in could not be verified. Please try again.")
	}

	// Device-pairing browser leg: bind the device instead of opening a
	// web session, then tell the human to return to their device.
	if st.DeviceNonce != "" {
		return r.completeDeviceBind(c, st.DeviceNonce, profile)
	}

	// Android broker leg: hand a one-shot, PKCE-bound handoff code back to
	// the app via its custom scheme instead of opening a web session.
	if st.AppChallenge != "" {
		return r.completeAppHandoff(c, st, profile)
	}

	user, err := auth.Authorize(ctx, r.deps.Store, profile)
	if err != nil {
		if errors.Is(err, auth.ErrNotAllowed) {
			return htmlMessage(c, fiber.StatusForbidden, "Access restricted",
				"This Live Ninja instance is private. Your Amazon account is not on the access list.")
		}
		r.deps.Log.Error("callback: authorize failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusInternalServerError, "Sign-in failed",
			"Something went wrong completing sign-in. Please try again.")
	}

	sess, wireRefresh, _, _, err := r.issueSession(c, user, store.SurfaceWeb, "")
	if err != nil {
		r.deps.Log.Error("callback: issue session failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusInternalServerError, "Sign-in failed",
			"Could not create your session. Please try again.")
	}
	if err := r.setAuthCookies(c, wireRefresh, auth.SlidingWindow); err != nil {
		r.deps.Log.Error("callback: set cookies failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusInternalServerError, "Sign-in failed",
			"Could not create your session. Please try again.")
	}

	r.notifySignIn(c, user.Email, user.Name, store.SurfaceWeb, sess.SessionID)
	return c.Redirect("/", fiber.StatusFound)
}

// ---- Android broker sign-in (no LWA-portal redirect URI) ----

// appLogin starts the LWA flow for the Android app in a Custom Tab. The app
// passes its own S256 code_challenge (app_challenge) and a state nonce
// (app_state). We run the ordinary web-callback flow against the
// already-whitelisted /auth/lwa/callback; the callback then hands a
// one-shot, PKCE-bound handoff code back to the app via its custom scheme
// (completeAppHandoff). This keeps LWA blind to the app's custom scheme, so
// nothing new has to be whitelisted in the Amazon Developer Portal.
func (r *authRoutes) appLogin(c *fiber.Ctx) error {
	appChallenge := c.Query("app_challenge")
	appState := c.Query("app_state")
	if !validCodeChallenge(appChallenge) {
		return htmlMessage(c, fiber.StatusBadRequest, "Sign-in unavailable",
			"The app sign-in request was malformed. Please update Live Ninja and try again.")
	}
	if appState == "" {
		return htmlMessage(c, fiber.StatusBadRequest, "Sign-in unavailable",
			"The app sign-in request was incomplete. Please try again.")
	}

	state, err := randomURLToken(32)
	if err != nil {
		return fmt.Errorf("generate oauth state: %w", err)
	}
	verifier, err := randomURLToken(32)
	if err != nil {
		return fmt.Errorf("generate pkce verifier: %w", err)
	}

	redirectURI := r.baseURL(c) + "/auth/lwa/callback"
	st := &store.OAuthState{
		State:        state,
		CodeVerifier: verifier,
		Surface:      store.SurfaceAndroid,
		RedirectURI:  redirectURI,
		AppChallenge: appChallenge,
		AppState:     appState,
	}
	if err := r.deps.Store.PutOAuthState(c.Context(), st); err != nil {
		r.deps.Log.Error("app login: put oauth state failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusInternalServerError, "Sign-in unavailable",
			"Could not start sign-in. Please try again in a moment.")
	}

	// Bind this transaction to the initiating Custom Tab (see the web login
	// leg — the cookie is carried through the Amazon → callback navigation
	// within the same Custom Tab browser session).
	c.Cookie(&fiber.Cookie{
		Name:     OAuthStateCookieName,
		Value:    state,
		Path:     "/",
		Secure:   true,
		HTTPOnly: true,
		SameSite: "Lax",
		MaxAge:   600,
	})

	return c.Redirect(r.deps.LWA.BuildAuthorizeURL(state, s256Challenge(verifier), redirectURI),
		fiber.StatusFound)
}

// completeAppHandoff runs after the LWA round-trip of an app broker leg: it
// runs the same Authorize access gate every surface uses, then stores a
// one-shot APPHANDOFF row (the authorized userId, PKCE-bound to the app's
// code_challenge) and 302s the code back to the app via its custom scheme.
// No session or tokens are minted here — that happens at appClaim, so no
// credential ever travels through the custom-scheme URL.
func (r *authRoutes) completeAppHandoff(c *fiber.Ctx, st *store.OAuthState, profile *auth.LWAProfile) error {
	ctx := c.Context()

	user, err := auth.Authorize(ctx, r.deps.Store, profile)
	if err != nil {
		if errors.Is(err, auth.ErrNotAllowed) {
			return htmlMessage(c, fiber.StatusForbidden, "Access restricted",
				"This Live Ninja instance is private. Your Amazon account is not on the access list.")
		}
		r.deps.Log.Error("app handoff: authorize failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusInternalServerError, "Sign-in failed",
			"Something went wrong completing sign-in. Please try again.")
	}

	code, err := randomURLToken(32)
	if err != nil {
		return fmt.Errorf("generate app handoff code: %w", err)
	}
	if err := r.deps.Store.PutAppHandoff(ctx, &store.AppHandoff{
		Code:         code,
		UserID:       user.UserID,
		AppChallenge: st.AppChallenge,
	}); err != nil {
		r.deps.Log.Error("app handoff: put handoff failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusInternalServerError, "Sign-in failed",
			"Something went wrong completing sign-in. Please try again.")
	}

	loc := appReturnScheme + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(st.AppState)
	return c.Redirect(loc, fiber.StatusFound)
}

// appClaim completes the Android broker flow: the app presents the one-shot
// handoff code from the custom-scheme redirect plus its PKCE code_verifier.
// We verify S256(verifier) == the stored challenge (proving this is the same
// app instance that started the flow — an app that merely intercepted the
// custom-scheme redirect cannot produce the verifier), then mint + return
// the Android session, identical in shape to /auth/lwa/exchange.
func (r *authRoutes) appClaim(c *fiber.Ctx) error {
	ctx := c.Context()

	var body struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"codeVerifier"`
	}
	if err := c.BodyParser(&body); err != nil {
		return badRequest(c, "Body must be JSON with code and codeVerifier.")
	}
	if body.Code == "" || body.CodeVerifier == "" {
		return badRequest(c, "code and codeVerifier are required.")
	}

	handoff, err := r.deps.Store.GetAppHandoff(ctx, body.Code)
	if err != nil {
		r.deps.Log.Error("app claim: get handoff failed", "error", err.Error())
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}
	if handoff == nil {
		return errorJSON(c, fiber.StatusUnauthorized, "invalid_code",
			"This sign-in code has expired or was already used. Please try again.")
	}
	// Constant-time PKCE check: the presented verifier must hash to the
	// challenge captured at app-login. Both sides are fixed-length base64url
	// S256 digests.
	if subtle.ConstantTimeCompare([]byte(s256Challenge(body.CodeVerifier)), []byte(handoff.AppChallenge)) != 1 {
		return errorJSON(c, fiber.StatusForbidden, "verifier_mismatch", "The sign-in verifier did not match.")
	}

	user, err := r.deps.Store.GetUser(ctx, handoff.UserID)
	if err != nil {
		r.deps.Log.Error("app claim: user lookup failed", "error", err.Error())
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}
	if user == nil || user.Status != store.UserStatusActive {
		return errorJSON(c, fiber.StatusForbidden, "account_unavailable", "Your account is not available. Please sign in again.")
	}

	sess, wireRefresh, accessToken, accessExp, err := r.issueSession(c, user, store.SurfaceAndroid, "")
	if err != nil {
		r.deps.Log.Error("app claim: issue session failed", "error", err.Error())
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}

	r.notifySignIn(c, user.Email, user.Name, store.SurfaceAndroid, sess.SessionID)
	return c.JSON(fiber.Map{
		"accessToken":      accessToken,
		"expiresAt":        accessExp,
		"refreshToken":     wireRefresh,
		"refreshExpiresAt": sess.ExpiresAt,
		"sessionId":        sess.SessionID,
	})
}

// exchange is the Android Custom-Tabs + PKCE code exchange:
// {code, codeVerifier, redirectURI} → access JWT + 30-day rotating refresh.
func (r *authRoutes) exchange(c *fiber.Ctx) error {
	ctx := c.Context()

	var body struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"codeVerifier"`
		RedirectURI  string `json:"redirectURI"`
	}
	if err := c.BodyParser(&body); err != nil {
		return badRequest(c, "Body must be JSON with code, codeVerifier, redirectURI.")
	}
	if body.Code == "" || body.CodeVerifier == "" || body.RedirectURI == "" {
		return badRequest(c, "code, codeVerifier and redirectURI are required.")
	}

	tokens, err := r.deps.LWA.ExchangeCode(ctx, body.Code, body.CodeVerifier, body.RedirectURI)
	if err != nil {
		r.deps.Log.Warn("exchange: lwa code exchange failed", "error", err.Error())
		return errorJSON(c, fiber.StatusUnauthorized, "code_exchange_failed", "Amazon did not accept the sign-in code. Please try again.")
	}
	profile, err := r.deps.LWA.Validate(ctx, tokens.AccessToken)
	if err != nil {
		r.deps.Log.Warn("exchange: lwa validate failed", "error", err.Error())
		return errorJSON(c, fiber.StatusUnauthorized, "token_validation_failed", "Your Amazon sign-in could not be verified. Please try again.")
	}

	user, err := auth.Authorize(ctx, r.deps.Store, profile)
	if err != nil {
		if errors.Is(err, auth.ErrNotAllowed) {
			return errorJSON(c, fiber.StatusForbidden, "not_allowed", "This Live Ninja instance is private. Your Amazon account is not on the access list.")
		}
		r.deps.Log.Error("exchange: authorize failed", "error", err.Error())
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}

	sess, wireRefresh, accessToken, accessExp, err := r.issueSession(c, user, store.SurfaceAndroid, "")
	if err != nil {
		r.deps.Log.Error("exchange: issue session failed", "error", err.Error())
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}

	r.notifySignIn(c, user.Email, user.Name, store.SurfaceAndroid, sess.SessionID)
	return c.JSON(fiber.Map{
		"accessToken":      accessToken,
		"expiresAt":        accessExp,
		"refreshToken":     wireRefresh,
		"refreshExpiresAt": sess.ExpiresAt,
		"sessionId":        sess.SessionID,
	})
}

// ---- refresh / logout ----

// refresh rotates the presented refresh token (web __Host- cookie or JSON
// refreshToken for Android/device) and mints a fresh 15-minute access JWT.
// Reuse of an already-rotated token trips store.RotateRefresh's family
// revoke; this handler then fires the security-alert email.
func (r *authRoutes) refresh(c *fiber.Ctx) error {
	ctx := c.Context()

	wire := c.Cookies(RefreshCookieName)
	fromCookie := wire != ""
	if !fromCookie {
		var body struct {
			RefreshToken string `json:"refreshToken"`
		}
		// Body is optional-shaped: only reject when no cookie either.
		_ = c.BodyParser(&body)
		wire = body.RefreshToken
	}
	if wire == "" {
		return errorJSON(c, fiber.StatusUnauthorized, "missing_refresh_token", "A refresh token is required.")
	}

	sessionID, secret, ok := splitWireRefresh(wire)
	if !ok {
		r.failRefresh(c, fromCookie)
		return errorJSON(c, fiber.StatusUnauthorized, "invalid_refresh_token", "The refresh token is invalid.")
	}

	sess, err := r.deps.Store.GetSessionByID(ctx, sessionID)
	if err != nil {
		r.deps.Log.Error("refresh: session lookup failed", "error", err.Error())
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}
	now := time.Now().UTC()
	if sess == nil || (sess.ExpiresAt > 0 && sess.ExpiresAt < now.Unix()) {
		r.failRefresh(c, fromCookie)
		return errorJSON(c, fiber.StatusUnauthorized, "invalid_refresh_token", "The refresh token is invalid.")
	}

	user, err := r.deps.Store.GetUser(ctx, sess.UserID)
	if err != nil {
		r.deps.Log.Error("refresh: user lookup failed", "error", err.Error())
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}
	if user == nil || user.Status != store.UserStatusActive {
		r.failRefresh(c, fromCookie)
		return errorJSON(c, fiber.StatusUnauthorized, "account_unavailable", "Your account is not available. Please sign in again.")
	}
	// tokensValidAfter kill-switch also invalidates refresh lineages
	// created before the cutoff (logout-all deletes the rows too — this is
	// defense in depth against a partially-failed revoke).
	if user.TokensValidAfter > 0 && sess.CreatedAt > 0 && sess.CreatedAt < user.TokensValidAfter {
		_ = r.deps.Store.RevokeSession(ctx, sess.UserID, sess.SessionID)
		r.failRefresh(c, fromCookie)
		return errorJSON(c, fiber.StatusUnauthorized, "session_revoked", "This session was revoked. Please sign in again.")
	}

	newSecret, newHash, err := auth.GenerateRefreshToken()
	if err != nil {
		return fmt.Errorf("generate refresh token: %w", err)
	}
	window := auth.SlidingWindow
	if sess.Surface == store.SurfaceDevice {
		window = auth.DeviceWindow
	}

	rotated, err := r.deps.Store.RotateRefresh(ctx, sess,
		auth.HashRefreshToken(secret), newHash, now.Add(window).Unix())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrRefreshReuse):
			// The whole family is already revoked by the store; alert the
			// account owner — this is the canonical stolen-token signal.
			r.deps.Log.Warn("refresh: reuse detected, family revoked",
				"userId", sess.UserID, "sessionId", sess.SessionID, "familyId", sess.FamilyID)
			r.notifySecurityAlert(c, user.Email, sess.Surface)
			r.failRefresh(c, fromCookie)
			return errorJSON(c, fiber.StatusUnauthorized, "refresh_reused",
				"This refresh token was already used and every session in its lineage has been signed out. Please sign in again.")
		case errors.Is(err, store.ErrInvalidRefresh):
			r.failRefresh(c, fromCookie)
			return errorJSON(c, fiber.StatusUnauthorized, "invalid_refresh_token", "The refresh token is invalid.")
		default:
			r.deps.Log.Error("refresh: rotate failed", "error", err.Error())
			return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
		}
	}

	accessToken, accessExp, err := r.mintAccess(c, user.UserID, rotated.SessionID, rotated.DeviceID, rotated.Surface, user.Role)
	if err != nil {
		r.deps.Log.Error("refresh: mint access token failed", "error", err.Error())
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}

	newWire := rotated.SessionID + "." + newSecret
	if fromCookie {
		if err := r.setAuthCookies(c, newWire, window); err != nil {
			return fmt.Errorf("set refreshed cookies: %w", err)
		}
		// Web keeps the refresh credential in the HttpOnly cookie only —
		// never in a JS-readable body.
		return c.JSON(fiber.Map{"accessToken": accessToken, "expiresAt": accessExp})
	}
	return c.JSON(fiber.Map{
		"accessToken":      accessToken,
		"expiresAt":        accessExp,
		"refreshToken":     newWire,
		"refreshExpiresAt": rotated.ExpiresAt,
		"sessionId":        rotated.SessionID,
	})
}

// logout revokes the caller's session. It accepts either an authorizer-
// verified identity (Locals) or, for a browser whose access JWT already
// expired, the refresh cookie itself — whose secret must hash-match the
// session row before the revoke happens. Idempotent: already-gone sessions
// still return ok.
func (r *authRoutes) logout(c *fiber.Ctx) error {
	ctx := c.Context()
	userID, sessionID := UserID(c), SessionID(c)

	if sessionID == "" {
		if sid, secret, ok := splitWireRefresh(c.Cookies(RefreshCookieName)); ok {
			sess, err := r.deps.Store.GetSessionByID(ctx, sid)
			if err == nil && sess != nil {
				h := auth.HashRefreshToken(secret)
				if subtleEquals(h, sess.RefreshHash) || subtleEquals(h, sess.PrevHash) {
					userID, sessionID = sess.UserID, sess.SessionID
				}
			}
		}
	}

	if userID != "" && sessionID != "" {
		if err := r.deps.Store.RevokeSession(ctx, userID, sessionID); err != nil && !errors.Is(err, store.ErrNotFound) {
			r.deps.Log.Error("logout: revoke session failed", "error", err.Error(),
				"userId", userID, "sessionId", sessionID)
		}
	}

	r.clearAuthCookies(c)
	return c.JSON(fiber.Map{"ok": true})
}

// logoutAll is "log out everywhere": bump tokensValidAfter (kills every
// outstanding JWT within the authorizer's cache window) and delete every
// session row (kills every refresh lineage immediately).
func (r *authRoutes) logoutAll(c *fiber.Ctx) error {
	ctx := c.Context()
	userID := UserID(c)

	if err := r.deps.Store.SetTokensValidAfter(ctx, userID, time.Now().Unix()); err != nil {
		r.deps.Log.Error("logout-all: set tokensValidAfter failed", "error", err.Error(), "userId", userID)
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}
	if err := r.deps.Store.RevokeAllForUser(ctx, userID); err != nil {
		r.deps.Log.Error("logout-all: revoke sessions failed", "error", err.Error(), "userId", userID)
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}

	r.clearAuthCookies(c)
	return c.JSON(fiber.Map{"ok": true})
}

// ---- device pairing (M5Stack 10-year lineage) ----

// deviceRegister is the device's first, credential-less call: it registers
// a single-use pairing nonce bound to the S256 code_challenge the device
// generated on-chip, and learns the claim URL a human must open plus the
// RFC 8628 user code ("XXXX-XXXX") it must display on its screen — the
// human types that code into the browser confirm page before the bind can
// finalize.
func (r *authRoutes) deviceRegister(c *fiber.Ctx) error {
	var body struct {
		CodeChallenge string `json:"codeChallenge"`
	}
	if err := c.BodyParser(&body); err != nil {
		return badRequest(c, "Body must be JSON with codeChallenge.")
	}
	if !validCodeChallenge(body.CodeChallenge) {
		return badRequest(c, "codeChallenge must be a 43-128 char base64url S256 challenge.")
	}

	nonce, userCode, expiresAt, err := auth.RegisterPairing(c.Context(), r.deps.Store, body.CodeChallenge)
	if err != nil {
		r.deps.Log.Error("device register: create pairing failed", "error", err.Error())
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"nonce":               nonce,
		"userCode":            auth.FormatUserCode(userCode),
		"claimUrl":            r.baseURL(c) + "/auth/device/claim?nonce=" + url.QueryEscape(nonce),
		"expiresAt":           expiresAt.UTC().Format(time.RFC3339),
		"pollIntervalSeconds": pollIntervalSeconds,
	})
}

// deviceClaim is the browser leg's entry point (the URL the human opens
// from the device's pairing screen/QR): validate the nonce is still
// claimable, then hand off to the LWA login flow carrying the nonce — the
// callback completes the bind via auth.BindPairing.
func (r *authRoutes) deviceClaim(c *fiber.Ctx) error {
	nonce := c.Query("nonce")
	if nonce == "" {
		return htmlMessage(c, fiber.StatusBadRequest, "Invalid pairing link",
			"This pairing link is missing its code. Start pairing again on your device.")
	}

	pair, err := auth.PollPairing(c.Context(), r.deps.Store, nonce)
	if err != nil {
		if errors.Is(err, auth.ErrPairNotFound) {
			return htmlMessage(c, fiber.StatusNotFound, "Pairing expired",
				"This pairing code has expired or was never registered. Start pairing again on your device.")
		}
		r.deps.Log.Error("device claim: poll pairing failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusInternalServerError, "Pairing unavailable",
			"Something went wrong. Please try again.")
	}
	if pair.Status == store.PairStatusFailed {
		return htmlMessage(c, fiber.StatusGone, "Pairing cancelled",
			"Too many incorrect codes were entered for this pairing. Restart pairing on your device to get a fresh code.")
	}
	if pair.Status != store.PairStatusPending {
		return htmlMessage(c, fiber.StatusConflict, "Already paired",
			"This pairing code was already used. If that wasn't you, revoke the device from your account settings.")
	}

	return c.Redirect("/auth/lwa/login?device_nonce="+url.QueryEscape(nonce), fiber.StatusFound)
}

// completeDeviceBind runs after the LWA round-trip of the device-pairing
// browser leg. It does NOT bind yet: it verifies the pairing is still
// claimable, runs the same Authorize gate every sign-in surface uses (so
// an account that couldn't sign in never sees the confirm form), then
// serves the RFC 8628 user-code confirm page. The LWA-verified identity is
// carried to the confirm POST via a one-shot PAIRCONFIRM row whose random
// token lives in BOTH a hidden form field and an HttpOnly cookie — the
// actual bind happens in deviceConfirm once the human proves they can read
// the code on the device's screen.
func (r *authRoutes) completeDeviceBind(c *fiber.Ctx, nonce string, profile *auth.LWAProfile) error {
	ctx := c.Context()

	pair, err := auth.PollPairing(ctx, r.deps.Store, nonce)
	if err != nil {
		if errors.Is(err, auth.ErrPairNotFound) {
			return htmlMessage(c, fiber.StatusNotFound, "Pairing expired",
				"This pairing code has expired. Start pairing again on your device.")
		}
		r.deps.Log.Error("device bind: poll pairing failed", "error", err.Error(), "nonce", nonce)
		return htmlMessage(c, fiber.StatusInternalServerError, "Pairing failed",
			"Something went wrong pairing your device. Please try again.")
	}
	if pair.Status == store.PairStatusFailed {
		return htmlMessage(c, fiber.StatusGone, "Pairing cancelled",
			"Too many incorrect codes were entered for this pairing. Restart pairing on your device to get a fresh code.")
	}
	if pair.Status != store.PairStatusPending {
		return htmlMessage(c, fiber.StatusConflict, "Already paired",
			"This pairing code was already used. If that wasn't you, revoke the device from your account settings.")
	}

	// Same access gate BindPairing will re-run at confirm time — checked
	// here too so a not-allowed account is turned away before the code form.
	if _, err := auth.Authorize(ctx, r.deps.Store, profile); err != nil {
		if errors.Is(err, auth.ErrNotAllowed) {
			return htmlMessage(c, fiber.StatusForbidden, "Access restricted",
				"This Live Ninja instance is private. Your Amazon account is not on the access list.")
		}
		r.deps.Log.Error("device bind: authorize failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusInternalServerError, "Pairing failed",
			"Something went wrong pairing your device. Please try again.")
	}

	token, err := randomURLToken(32)
	if err != nil {
		return fmt.Errorf("generate pair confirm token: %w", err)
	}
	if err := r.deps.Store.PutPairConfirm(ctx, &store.PairConfirm{
		Token:        token,
		Nonce:        nonce,
		AmazonUserID: profile.UserID,
		Email:        profile.Email,
		Name:         profile.Name,
	}); err != nil {
		r.deps.Log.Error("device bind: put pair confirm failed", "error", err.Error(), "nonce", nonce)
		return htmlMessage(c, fiber.StatusInternalServerError, "Pairing failed",
			"Something went wrong pairing your device. Please try again.")
	}
	c.Cookie(&fiber.Cookie{
		Name: PairConfirmCookieName, Value: token,
		Path: "/", MaxAge: 600, Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
	})

	return r.renderConfirmPage(c, fiber.StatusOK, token, "", "")
}

// deviceConfirm is the POST target of the user-code confirm page: it
// re-establishes the confirm context (hidden-field token must equal the
// HttpOnly cookie token, constant-time, and resolve to a live PAIRCONFIRM
// row), then hands the typed code to auth.BindPairing — which does the
// constant-time code match, attempt accounting, and the bind itself.
func (r *authRoutes) deviceConfirm(c *fiber.Ctx) error {
	ctx := c.Context()

	token := c.FormValue("token")
	cookie := c.Cookies(PairConfirmCookieName)
	if token == "" || cookie == "" || subtle.ConstantTimeCompare([]byte(token), []byte(cookie)) != 1 {
		return htmlMessage(c, fiber.StatusBadRequest, "Pairing session expired",
			"This pairing confirmation could not be verified in this browser. Start again from the link or QR code on your device.")
	}

	confirm, err := r.deps.Store.GetPairConfirm(ctx, token)
	if err != nil {
		r.deps.Log.Error("device confirm: get pair confirm failed", "error", err.Error())
		return htmlMessage(c, fiber.StatusInternalServerError, "Pairing failed",
			"Something went wrong pairing your device. Please try again.")
	}
	if confirm == nil {
		r.clearPairConfirmCookie(c)
		return htmlMessage(c, fiber.StatusBadRequest, "Pairing session expired",
			"This pairing confirmation has expired or was already used. Start again from the link or QR code on your device.")
	}

	userCode := c.FormValue("user_code")
	if strings.TrimSpace(userCode) == "" {
		// Nothing typed: re-prompt without burning an attempt.
		return r.renderConfirmPage(c, fiber.StatusBadRequest, token, "",
			"Enter the code shown on your device's screen.")
	}

	profile := &auth.LWAProfile{UserID: confirm.AmazonUserID, Email: confirm.Email, Name: confirm.Name}
	err = auth.BindPairing(ctx, r.deps.Store, r.deps.Log, confirm.Nonce, "", userCode, profile)
	var mismatch *auth.UserCodeMismatchError
	switch {
	case err == nil:
		// fall through to success below
	case errors.As(err, &mismatch):
		plural := "s"
		if mismatch.AttemptsRemaining == 1 {
			plural = ""
		}
		return r.renderConfirmPage(c, fiber.StatusBadRequest, token, userCode,
			fmt.Sprintf("That code doesn't match the one on your device. %d attempt%s remaining.",
				mismatch.AttemptsRemaining, plural))
	case errors.Is(err, auth.ErrPairFailed):
		r.discardPairConfirm(c, token)
		return htmlMessage(c, fiber.StatusGone, "Pairing cancelled",
			"Too many incorrect codes were entered, so this pairing has been cancelled for safety. Restart pairing on your device to get a fresh code.")
	case errors.Is(err, auth.ErrNotAllowed):
		r.discardPairConfirm(c, token)
		return htmlMessage(c, fiber.StatusForbidden, "Access restricted",
			"This Live Ninja instance is private. Your Amazon account is not on the access list.")
	case errors.Is(err, auth.ErrPairNotFound):
		r.discardPairConfirm(c, token)
		return htmlMessage(c, fiber.StatusNotFound, "Pairing expired",
			"This pairing code has expired. Start pairing again on your device.")
	case errors.Is(err, auth.ErrPairAlreadyClaimed):
		r.discardPairConfirm(c, token)
		return htmlMessage(c, fiber.StatusConflict, "Already paired",
			"This pairing code was already used. If that wasn't you, revoke the device from your account settings.")
	default:
		r.deps.Log.Error("device bind failed", "error", err.Error(), "nonce", confirm.Nonce)
		return htmlMessage(c, fiber.StatusInternalServerError, "Pairing failed",
			"Something went wrong pairing your device. Please try again.")
	}

	r.discardPairConfirm(c, token)
	r.notifySignIn(c, profile.Email, profile.Name, store.SurfaceDevice, "")
	return htmlMessage(c, fiber.StatusOK, "Device connected",
		"Your device is now paired to your account. You can close this page — the device will finish setting itself up within a few seconds.")
}

// discardPairConfirm drops the confirm row and its browser cookie once the
// confirm leg reaches any terminal outcome (success or failure). Row
// deletion is best-effort — the 10-minute TTL reaps stragglers.
func (r *authRoutes) discardPairConfirm(c *fiber.Ctx, token string) {
	if err := r.deps.Store.DeletePairConfirm(c.Context(), token); err != nil {
		r.deps.Log.Warn("device confirm: delete pair confirm failed", "error", err.Error())
	}
	r.clearPairConfirmCookie(c)
}

func (r *authRoutes) clearPairConfirmCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name: PairConfirmCookieName, Value: "", Expires: time.Now().Add(-time.Hour),
		Path: "/", MaxAge: -1, Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
	})
}

// devicePoll is the device's polling loop: pending → keep waiting; bound →
// present the PKCE code_verifier and (exactly once) receive the 10-year
// refresh credential + an access JWT; claimed → this nonce is spent.
func (r *authRoutes) devicePoll(c *fiber.Ctx) error {
	ctx := c.Context()

	var body struct {
		Nonce        string `json:"nonce"`
		CodeVerifier string `json:"codeVerifier"`
	}
	if err := c.BodyParser(&body); err != nil {
		return badRequest(c, "Body must be JSON with nonce (and codeVerifier once claiming).")
	}
	if body.Nonce == "" {
		return badRequest(c, "nonce is required.")
	}

	pair, err := auth.PollPairing(ctx, r.deps.Store, body.Nonce)
	if err != nil {
		if errors.Is(err, auth.ErrPairNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"status": "expired"})
		}
		r.deps.Log.Error("device poll: poll pairing failed", "error", err.Error())
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}

	switch pair.Status {
	case store.PairStatusPending:
		return c.JSON(fiber.Map{"status": "pending", "pollIntervalSeconds": pollIntervalSeconds})
	case store.PairStatusClaimed:
		return errorJSON(c, fiber.StatusGone, "already_claimed", "This pairing code was already used.")
	case store.PairStatusFailed:
		return r.pairFailedJSON(c)
	case store.PairStatusBound:
		// proceed to claim below
	default:
		r.deps.Log.Error("device poll: unexpected pair status", "status", pair.Status)
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}

	if body.CodeVerifier == "" {
		return badRequest(c, "codeVerifier is required to claim a bound pairing.")
	}

	claim, err := auth.ClaimPairing(ctx, r.deps.Store, body.Nonce, body.CodeVerifier)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrPKCEMismatch):
			return errorJSON(c, fiber.StatusForbidden, "verifier_mismatch", "The pairing code_verifier did not match.")
		case errors.Is(err, auth.ErrPairAlreadyClaimed):
			return errorJSON(c, fiber.StatusGone, "already_claimed", "This pairing code was already used.")
		case errors.Is(err, auth.ErrPairFailed):
			return r.pairFailedJSON(c)
		case errors.Is(err, auth.ErrPairNotBound):
			return c.JSON(fiber.Map{"status": "pending", "pollIntervalSeconds": pollIntervalSeconds})
		default:
			r.deps.Log.Error("device poll: claim failed", "error", err.Error())
			return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
		}
	}

	accessToken, accessExp, err := r.mintAccess(c, claim.UserID, claim.SessionID, claim.DeviceID, store.SurfaceDevice, "")
	if err != nil {
		r.deps.Log.Error("device poll: mint access token failed", "error", err.Error())
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}

	r.deps.Log.Info("device pairing claimed",
		"deviceId", claim.DeviceID, "userId", claim.UserID, "sessionId", claim.SessionID)
	return c.JSON(fiber.Map{
		"status":           "bound",
		"deviceId":         claim.DeviceID,
		"sessionId":        claim.SessionID,
		"refreshToken":     claim.SessionID + "." + claim.RefreshToken,
		"refreshExpiresAt": claim.ExpiresAt,
		"accessToken":      accessToken,
		"expiresAt":        accessExp,
		"thingName":        claim.ThingName,
		"certArn":          claim.CertArn,
	})
}

// ---- session issuance helpers ----

// issueSession creates a fresh session family (web/android surfaces) and
// mints its first access JWT. Returns the stored session, the wire-format
// refresh credential, and the access token + its expiry (unix seconds).
func (r *authRoutes) issueSession(c *fiber.Ctx, user *store.User, surface, deviceID string) (*store.Session, string, string, int64, error) {
	secret, hash, err := auth.GenerateRefreshToken()
	if err != nil {
		return nil, "", "", 0, fmt.Errorf("generate refresh token: %w", err)
	}

	now := time.Now().UTC()
	sess := &store.Session{
		SessionID:   uuid.NewString(),
		UserID:      user.UserID,
		FamilyID:    uuid.NewString(),
		Surface:     surface,
		DeviceID:    deviceID,
		RefreshHash: hash,
		CreatedAt:   now.Unix(),
		LastUsedAt:  now.Unix(),
		ExpiresAt:   now.Add(auth.SlidingWindow).Unix(),
		TTL:         now.Add(auth.SlidingWindow).Unix(),
	}
	if err := r.deps.Store.CreateSession(c.Context(), sess); err != nil {
		return nil, "", "", 0, fmt.Errorf("create session: %w", err)
	}

	accessToken, accessExp, err := r.mintAccess(c, user.UserID, sess.SessionID, deviceID, surface, user.Role)
	if err != nil {
		return nil, "", "", 0, err
	}
	return sess, sess.SessionID + "." + secret, accessToken, accessExp, nil
}

// mintAccess signs a 15-minute ES256 access JWT for the given identity.
func (r *authRoutes) mintAccess(c *fiber.Ctx, userID, sessionID, deviceID, surface, scope string) (string, int64, error) {
	now := time.Now().Unix()
	exp := now + int64(auth.AccessTokenTTL/time.Second)
	token, err := r.deps.Signer.SignAccessToken(c.Context(), auth.Claims{
		Sub:     userID,
		Sid:     sessionID,
		Did:     deviceID,
		Surface: surface,
		Scope:   scope,
		Iat:     now,
		Exp:     exp,
	})
	if err != nil {
		return "", 0, fmt.Errorf("sign access token: %w", err)
	}
	return token, exp, nil
}

// ---- cookies ----

// setAuthCookies sets the __Host- refresh cookie (HttpOnly) and a fresh
// CSRF double-submit cookie (JS-readable). __Host- prefix rules: Secure,
// Path=/, no Domain.
func (r *authRoutes) setAuthCookies(c *fiber.Ctx, wireRefresh string, maxAge time.Duration) error {
	csrf, err := randomURLToken(32)
	if err != nil {
		return fmt.Errorf("generate csrf token: %w", err)
	}
	secs := int(maxAge / time.Second)
	c.Cookie(&fiber.Cookie{
		Name: RefreshCookieName, Value: wireRefresh,
		Path: "/", MaxAge: secs, Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
	})
	c.Cookie(&fiber.Cookie{
		Name: CSRFCookieName, Value: csrf,
		Path: "/", MaxAge: secs, Secure: true, HTTPOnly: false, SameSite: fiber.CookieSameSiteLaxMode,
	})
	return nil
}

func (r *authRoutes) clearAuthCookies(c *fiber.Ctx) {
	expired := time.Now().Add(-time.Hour)
	c.Cookie(&fiber.Cookie{
		Name: RefreshCookieName, Value: "", Expires: expired,
		Path: "/", MaxAge: -1, Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
	})
	c.Cookie(&fiber.Cookie{
		Name: CSRFCookieName, Value: "", Expires: expired,
		Path: "/", MaxAge: -1, Secure: true, HTTPOnly: false, SameSite: fiber.CookieSameSiteLaxMode,
	})
}

// failRefresh clears the auth cookies when the failing credential arrived
// via cookie, so a broken browser session doesn't retry a dead token
// forever.
func (r *authRoutes) failRefresh(c *fiber.Ctx, fromCookie bool) {
	if fromCookie {
		r.clearAuthCookies(c)
	}
}

// ---- security notifications (SQS → email-dispatch → SES) ----

// notifySignIn enqueues the new-sign-in alert. Best-effort: a queue
// failure is logged, never surfaced to the signing-in user.
func (r *authRoutes) notifySignIn(c *fiber.Ctx, email, name, surface, sessionID string) {
	if email == "" {
		r.deps.Log.Warn("sign-in alert skipped: user has no email", "surface", surface)
		return
	}
	when := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	text := fmt.Sprintf(
		"Hi %s,\n\nA new sign-in to Live Ninja just completed.\n\nSurface: %s\nTime: %s\n\nIf this was you, no action is needed. If not, open Live Ninja and use \"Log out everywhere\", then review your devices.\n",
		firstNonEmpty(name, "there"), surface, when)
	if err := r.deps.EnqueueEmail(c.Context(), "new-device-login", email,
		"Live Ninja: new sign-in on "+surface, text); err != nil {
		r.deps.Log.Warn("sign-in alert enqueue failed", "error", err.Error(), "sessionId", sessionID)
	}
}

// notifySecurityAlert enqueues the refresh-token-reuse alert fired when a
// rotated token is replayed (the session family is already revoked).
func (r *authRoutes) notifySecurityAlert(c *fiber.Ctx, email, surface string) {
	if email == "" {
		return
	}
	when := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	text := fmt.Sprintf(
		"A previously-used Live Ninja refresh token was presented again at %s (surface: %s).\n\nThis usually means a token was copied or stolen, so every session in that lineage has been signed out automatically. Sign in again to continue; if this keeps happening, use \"Log out everywhere\".\n",
		when, surface)
	if err := r.deps.EnqueueEmail(c.Context(), "security-alert", email,
		"Live Ninja security alert: session token reuse detected", text); err != nil {
		r.deps.Log.Warn("security alert enqueue failed", "error", err.Error())
	}
}

// ---- small helpers ----

// baseURL is the externally-visible origin used for redirect URIs and
// claim links: the configured domain in deployment, the request's own
// host/scheme in local dev.
func (r *authRoutes) baseURL(c *fiber.Ctx) string {
	if r.deps.Cfg.DomainName != "" {
		return "https://" + r.deps.Cfg.DomainName
	}
	return c.Protocol() + "://" + string(c.Request().URI().Host())
}

func badRequest(c *fiber.Ctx, msg string) error {
	return errorJSON(c, fiber.StatusBadRequest, "bad_request", msg)
}

// pairFailedJSON is the terminal poll/claim response for a pairing
// invalidated by too many wrong user codes: the device should stop
// polling this nonce, show the human why, and restart pairing (fresh
// nonce, fresh code).
func (r *authRoutes) pairFailedJSON(c *fiber.Ctx) error {
	return c.Status(fiber.StatusGone).JSON(fiber.Map{
		"status":  "failed",
		"reason":  pairFailedReason,
		"message": "Too many incorrect pairing codes were entered in the browser. Restart pairing to get a fresh code.",
	})
}

// renderConfirmPage serves the SSR user-code confirm form (the RFC 8628
// anti-phishing leg): same minimal single-purpose style as htmlMessage,
// with a labeled, autofocused code input. token is the server-generated
// confirm token echoed as a hidden field; typedCode re-fills the input
// after a wrong entry (preserve input, per form rules); errMsg, when
// non-empty, renders as the inline field error. The inline script is
// progressive enhancement only: when this browser also holds a Live Ninja
// web session, the global CSRF middleware requires the X-LN-CSRF header on
// POSTs, which a bare HTML form cannot send — the script re-submits via
// fetch with that header. Without JS (or without a web session, the
// common pairing case) the native form POST works as-is.
func (r *authRoutes) renderConfirmPage(c *fiber.Ctx, status int, token, typedCode, errMsg string) error {
	errBlock := ""
	describedBy := "user_code_hint"
	if errMsg != "" {
		errBlock = `<p class="err" id="user_code_error" role="alert">` + html.EscapeString(errMsg) + `</p>`
		describedBy = "user_code_error user_code_hint"
	}
	c.Type("html")
	return c.Status(status).SendString(fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Confirm your device — Live Ninja</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
         margin: 0; min-height: 100vh; display: flex; align-items: center;
         justify-content: center; background: #0b0b12; color: #f4f4f8; }
  main { text-align: center; padding: 2rem; max-width: 32rem; }
  h1 { font-size: 1.5rem; margin: 0 0 .75rem; }
  p { color: #9a9aab; margin: 0 0 1.25rem; line-height: 1.5; }
  label { display: block; font-weight: 600; margin: 0 0 .5rem; }
  input[type="text"] { font-family: ui-monospace, "Cascadia Mono", Consolas, monospace;
         font-size: 1.5rem; letter-spacing: .3em; text-transform: uppercase;
         text-align: center; width: 100%%; max-width: 16rem; padding: .6rem .5rem;
         border-radius: .5rem; border: 1px solid #3a3a4d; background: #15151f;
         color: #f4f4f8; }
  input[type="text"]:focus { outline: 2px solid #8ab4ff; outline-offset: 2px; }
  .err { color: #ff9d9d; margin: .75rem 0 0; }
  button { margin-top: 1.25rem; font-size: 1rem; font-weight: 600; padding: .7rem 2rem;
         border-radius: .5rem; border: none; background: #8ab4ff; color: #0b0b12;
         cursor: pointer; }
  button:focus-visible { outline: 2px solid #f4f4f8; outline-offset: 2px; }
</style>
</head>
<body>
<main>
  <h1>Confirm your device</h1>
  <p id="user_code_hint">Enter the code shown on your device&#39;s screen to finish pairing.
     This proves you&#39;re connecting the device in front of you.</p>
  <form id="pair-confirm-form" method="post" action="/auth/device/pair/confirm">
    <input type="hidden" name="token" value="%[1]s">
    <label for="user_code">Pairing code</label>
    <input type="text" id="user_code" name="user_code" value="%[2]s"
           autofocus autocomplete="off" autocapitalize="characters"
           spellcheck="false" inputmode="text" maxlength="9"
           placeholder="XXXX-XXXX" aria-describedby="%[4]s" required>
    %[3]s
    <button type="submit">Connect device</button>
  </form>
</main>
<script>
(function () {
  var f = document.getElementById('pair-confirm-form');
  f.addEventListener('submit', function (e) {
    var m = document.cookie.match(/(?:^|; )__Host-ln_csrf=([^;]*)/);
    if (!m) return; // no web session in this browser: native POST is fine
    e.preventDefault();
    fetch(f.action, {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'X-LN-CSRF': decodeURIComponent(m[1]),
        'Content-Type': 'application/x-www-form-urlencoded'
      },
      body: new URLSearchParams(new FormData(f)).toString()
    }).then(function (r) { return r.text(); }).then(function (htmlBody) {
      document.open(); document.write(htmlBody); document.close();
    }).catch(function () {
      // form.submit() bypasses submit handlers, so this cannot loop.
      f.submit();
    });
  });
})();
</script>
</body>
</html>
`, html.EscapeString(token), html.EscapeString(typedCode), errBlock, describedBy))
}

// htmlMessage renders the minimal browser-facing message page used by the
// login/callback/pairing legs (they're navigated to directly, so JSON
// would be user-hostile). body is pre-escaped by callers where dynamic.
func htmlMessage(c *fiber.Ctx, status int, title, body string) error {
	c.Type("html")
	return c.Status(status).SendString(fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%[1]s — Live Ninja</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
         margin: 0; min-height: 100vh; display: flex; align-items: center;
         justify-content: center; background: #0b0b12; color: #f4f4f8; }
  main { text-align: center; padding: 2rem; max-width: 32rem; }
  h1 { font-size: 1.5rem; margin: 0 0 .75rem; }
  p { color: #9a9aab; margin: 0 0 1.25rem; line-height: 1.5; }
  a { color: #8ab4ff; }
</style>
</head>
<body>
<main>
  <h1>%[1]s</h1>
  <p>%[2]s</p>
  <p><a href="/">Back to Live Ninja</a></p>
</main>
</body>
</html>
`, html.EscapeString(title), body))
}

// splitWireRefresh splits "<sessionId>.<secret>" at the first dot.
func splitWireRefresh(wire string) (sessionID, secret string, ok bool) {
	i := strings.IndexByte(wire, '.')
	if i <= 0 || i == len(wire)-1 {
		return "", "", false
	}
	return wire[:i], wire[i+1:], true
}

func subtleEquals(a, b string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// validCodeChallenge enforces the RFC 7636 shape of an S256 challenge:
// 43-128 base64url characters.
func validCodeChallenge(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomURLToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
