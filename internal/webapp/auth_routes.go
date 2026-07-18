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
}

type authRoutes struct {
	deps *Deps
}

// ---- JWKS ----

func (r *authRoutes) jwks(c *fiber.Ctx) error {
	doc, err := r.deps.Signer.JWKS(c.Context())
	if err != nil {
		r.deps.Log.Error("jwks: fetch failed", "error", err.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "jwks_unavailable"})
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
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "code_exchange_failed"})
	}
	profile, err := r.deps.LWA.Validate(ctx, tokens.AccessToken)
	if err != nil {
		r.deps.Log.Warn("exchange: lwa validate failed", "error", err.Error())
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "token_validation_failed"})
	}

	user, err := auth.Authorize(ctx, r.deps.Store, profile)
	if err != nil {
		if errors.Is(err, auth.ErrNotAllowed) {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not_allowed"})
		}
		r.deps.Log.Error("exchange: authorize failed", "error", err.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
	}

	sess, wireRefresh, accessToken, accessExp, err := r.issueSession(c, user, store.SurfaceAndroid, "")
	if err != nil {
		r.deps.Log.Error("exchange: issue session failed", "error", err.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
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
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing_refresh_token"})
	}

	sessionID, secret, ok := splitWireRefresh(wire)
	if !ok {
		r.failRefresh(c, fromCookie)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_refresh_token"})
	}

	sess, err := r.deps.Store.GetSessionByID(ctx, sessionID)
	if err != nil {
		r.deps.Log.Error("refresh: session lookup failed", "error", err.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
	}
	now := time.Now().UTC()
	if sess == nil || (sess.ExpiresAt > 0 && sess.ExpiresAt < now.Unix()) {
		r.failRefresh(c, fromCookie)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_refresh_token"})
	}

	user, err := r.deps.Store.GetUser(ctx, sess.UserID)
	if err != nil {
		r.deps.Log.Error("refresh: user lookup failed", "error", err.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
	}
	if user == nil || user.Status != store.UserStatusActive {
		r.failRefresh(c, fromCookie)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "account_unavailable"})
	}
	// tokensValidAfter kill-switch also invalidates refresh lineages
	// created before the cutoff (logout-all deletes the rows too — this is
	// defense in depth against a partially-failed revoke).
	if user.TokensValidAfter > 0 && sess.CreatedAt > 0 && sess.CreatedAt < user.TokensValidAfter {
		_ = r.deps.Store.RevokeSession(ctx, sess.UserID, sess.SessionID)
		r.failRefresh(c, fromCookie)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "session_revoked"})
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
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "refresh_reused"})
		case errors.Is(err, store.ErrInvalidRefresh):
			r.failRefresh(c, fromCookie)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_refresh_token"})
		default:
			r.deps.Log.Error("refresh: rotate failed", "error", err.Error())
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
		}
	}

	accessToken, accessExp, err := r.mintAccess(c, user.UserID, rotated.SessionID, rotated.DeviceID, rotated.Surface, user.Role)
	if err != nil {
		r.deps.Log.Error("refresh: mint access token failed", "error", err.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
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
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
	}
	if err := r.deps.Store.RevokeAllForUser(ctx, userID); err != nil {
		r.deps.Log.Error("logout-all: revoke sessions failed", "error", err.Error(), "userId", userID)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
	}

	r.clearAuthCookies(c)
	return c.JSON(fiber.Map{"ok": true})
}

// ---- device pairing (M5Stack 10-year lineage) ----

// deviceRegister is the device's first, credential-less call: it registers
// a single-use pairing nonce bound to the S256 code_challenge the device
// generated on-chip, and learns the claim URL a human must open.
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

	nonce, expiresAt, err := auth.RegisterPairing(c.Context(), r.deps.Store, body.CodeChallenge)
	if err != nil {
		r.deps.Log.Error("device register: create pairing failed", "error", err.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"nonce":               nonce,
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
	if pair.Status != store.PairStatusPending {
		return htmlMessage(c, fiber.StatusConflict, "Already paired",
			"This pairing code was already used. If that wasn't you, revoke the device from your account settings.")
	}

	return c.Redirect("/auth/lwa/login?device_nonce="+url.QueryEscape(nonce), fiber.StatusFound)
}

// completeDeviceBind finishes the browser leg after the LWA round-trip:
// the same Authorize gate as every sign-in runs inside BindPairing; on
// success the device's DEVICE# record, IoT provisioning hook, and 10-year
// refresh family all exist and the device's next poll can claim them.
func (r *authRoutes) completeDeviceBind(c *fiber.Ctx, nonce string, profile *auth.LWAProfile) error {
	err := auth.BindPairing(c.Context(), r.deps.Store, r.deps.Log, nonce, "", profile)
	switch {
	case err == nil:
		// fall through to success below
	case errors.Is(err, auth.ErrNotAllowed):
		return htmlMessage(c, fiber.StatusForbidden, "Access restricted",
			"This Live Ninja instance is private. Your Amazon account is not on the access list.")
	case errors.Is(err, auth.ErrPairNotFound):
		return htmlMessage(c, fiber.StatusNotFound, "Pairing expired",
			"This pairing code has expired. Start pairing again on your device.")
	case errors.Is(err, auth.ErrPairAlreadyClaimed):
		return htmlMessage(c, fiber.StatusConflict, "Already paired",
			"This pairing code was already used. If that wasn't you, revoke the device from your account settings.")
	default:
		r.deps.Log.Error("device bind failed", "error", err.Error(), "nonce", nonce)
		return htmlMessage(c, fiber.StatusInternalServerError, "Pairing failed",
			"Something went wrong pairing your device. Please try again.")
	}

	r.notifySignIn(c, profile.Email, profile.Name, store.SurfaceDevice, "")
	return htmlMessage(c, fiber.StatusOK, "Device connected",
		"Your device is now paired to your account. You can close this page — the device will finish setting itself up within a few seconds.")
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
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
	}

	switch pair.Status {
	case store.PairStatusPending:
		return c.JSON(fiber.Map{"status": "pending", "pollIntervalSeconds": pollIntervalSeconds})
	case store.PairStatusClaimed:
		return c.Status(fiber.StatusGone).JSON(fiber.Map{"error": "already_claimed"})
	case store.PairStatusBound:
		// proceed to claim below
	default:
		r.deps.Log.Error("device poll: unexpected pair status", "status", pair.Status)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
	}

	if body.CodeVerifier == "" {
		return badRequest(c, "codeVerifier is required to claim a bound pairing.")
	}

	claim, err := auth.ClaimPairing(ctx, r.deps.Store, body.Nonce, body.CodeVerifier)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrPKCEMismatch):
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "verifier_mismatch"})
		case errors.Is(err, auth.ErrPairAlreadyClaimed):
			return c.Status(fiber.StatusGone).JSON(fiber.Map{"error": "already_claimed"})
		case errors.Is(err, auth.ErrPairNotBound):
			return c.JSON(fiber.Map{"status": "pending", "pollIntervalSeconds": pollIntervalSeconds})
		default:
			r.deps.Log.Error("device poll: claim failed", "error", err.Error())
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
		}
	}

	accessToken, accessExp, err := r.mintAccess(c, claim.UserID, claim.SessionID, claim.DeviceID, store.SurfaceDevice, "")
	if err != nil {
		r.deps.Log.Error("device poll: mint access token failed", "error", err.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal"})
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
	return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request", "message": msg})
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
