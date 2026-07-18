package webapp

// Tests for the device-pairing HTTP surface (auth_routes.go): the RFC
// 8628 user code returned by register, the browser confirm leg (token +
// cookie binding, wrong-code inline errors with preserved input, attempt
// exhaustion -> terminal invalidation), and the device poll's terminal
// "failed" status.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// newPairingApp mounts the full auth route surface over a FakeDynamo store
// and a FakeKMS-backed signer (the claim leg mints a real ES256 JWT).
func newPairingApp(t *testing.T) (*fiber.App, *store.Store) {
	t.Helper()
	st := store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja")
	fakeKMS, err := testutil.NewFakeKMS()
	if err != nil {
		t.Fatalf("fake kms: %v", err)
	}
	deps := &Deps{
		Store:  st,
		Signer: auth.NewSignerWithClient(fakeKMS, "arn:aws:kms:us-east-1:000000000000:key/test-key"),
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	app := fiber.New()
	RegisterAuthRoutes(app, deps)
	return app, st
}

// doForm POSTs an application/x-www-form-urlencoded body with optional
// cookies and returns the response plus its full body text.
func doForm(t *testing.T, app *fiber.App, path string, cookies map[string]string, form url.Values) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for name, val := range cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: val})
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp, string(body)
}

// registerPairing drives POST /auth/device/pair/start and returns the
// nonce plus the raw (undashed) user code read back from the PAIR row.
func registerPairing(t *testing.T, app *fiber.App, st *store.Store, codeVerifier string) (nonce, userCode string) {
	t.Helper()
	challenge := base64RawURLSHA256(codeVerifier)
	resp, body := doJSON(t, app, http.MethodPost, "/auth/device/pair/start",
		map[string]any{"codeChallenge": challenge})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status = %d (%v)", resp.StatusCode, body)
	}
	nonce, _ = body["nonce"].(string)
	if nonce == "" {
		t.Fatalf("register: no nonce in %v", body)
	}
	pair, err := st.GetPair(context.Background(), nonce)
	if err != nil || pair == nil {
		t.Fatalf("register: pair row missing (err=%v)", err)
	}
	return nonce, pair.UserCode
}

// seedConfirm plants the PAIRCONFIRM row + returns the cookie map the
// confirm POST needs, standing in for the LWA callback leg.
func seedConfirm(t *testing.T, st *store.Store, token, nonce string) map[string]string {
	t.Helper()
	if err := st.PutPairConfirm(context.Background(), &store.PairConfirm{
		Token:        token,
		Nonce:        nonce,
		AmazonUserID: "amzn1.account.owner",
		Email:        "owner@example.com",
		Name:         "Owner",
	}); err != nil {
		t.Fatalf("seed pair confirm: %v", err)
	}
	return map[string]string{PairConfirmCookieName: token}
}

func base64RawURLSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestDeviceRegisterReturnsFormattedUserCode(t *testing.T) {
	app, st := newPairingApp(t)

	resp, body := doJSON(t, app, http.MethodPost, "/auth/device/pair/start",
		map[string]any{"codeChallenge": base64RawURLSHA256("verifier-verifier-verifier-verifier")})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%v)", resp.StatusCode, body)
	}

	userCode, _ := body["userCode"].(string)
	if !regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$`).MatchString(userCode) {
		t.Fatalf("userCode = %q, want XXXX-XXXX from the RFC 8628 alphabet", userCode)
	}
	nonce, _ := body["nonce"].(string)
	if claimURL, _ := body["claimUrl"].(string); !strings.Contains(claimURL, url.QueryEscape(nonce)) {
		t.Errorf("claimUrl %q does not carry the nonce", claimURL)
	}

	// The stored (undashed) code is what the response's dashed form renders.
	pair, err := st.GetPair(context.Background(), nonce)
	if err != nil || pair == nil {
		t.Fatalf("pair row missing (err=%v)", err)
	}
	if auth.FormatUserCode(pair.UserCode) != userCode {
		t.Errorf("stored code %q != displayed %q", pair.UserCode, userCode)
	}
}

func TestDeviceConfirmHappyPath(t *testing.T) {
	app, st := newPairingApp(t)
	verifier := "device-on-chip-code-verifier-0123456789abcdef"
	nonce, userCode := registerPairing(t, app, st, verifier)
	cookies := seedConfirm(t, st, "tok-happy", nonce)

	// Human types the code lowercased with the dash — accepted.
	typed := strings.ToLower(auth.FormatUserCode(userCode))
	resp, page := doForm(t, app, "/auth/device/pair/confirm", cookies,
		url.Values{"token": {"tok-happy"}, "user_code": {typed}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: status = %d, want 200\n%s", resp.StatusCode, page)
	}
	if !strings.Contains(page, "Device connected") {
		t.Errorf("confirm page missing success message:\n%s", page)
	}

	// Confirm context is one-shot: row deleted, cookie cleared.
	if pc, _ := st.GetPairConfirm(context.Background(), "tok-happy"); pc != nil {
		t.Errorf("pair confirm row survived a successful bind")
	}

	// Device poll now claims the 10-year credential.
	resp, body := doJSON(t, app, http.MethodPost, "/auth/device/pair/poll",
		map[string]any{"nonce": nonce, "codeVerifier": verifier})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("poll: status = %d (%v)", resp.StatusCode, body)
	}
	if body["status"] != "bound" {
		t.Fatalf("poll status = %v, want bound", body["status"])
	}
	for _, key := range []string{"refreshToken", "accessToken", "deviceId"} {
		if s, _ := body[key].(string); s == "" {
			t.Errorf("claim response missing %s", key)
		}
	}
}

func TestDeviceConfirmWrongCodeInlineErrorPreservesInput(t *testing.T) {
	app, st := newPairingApp(t)
	nonce, _ := registerPairing(t, app, st, "verifier-verifier-verifier-verifier")
	cookies := seedConfirm(t, st, "tok-wrong", nonce)

	resp, page := doForm(t, app, "/auth/device/pair/confirm", cookies,
		url.Values{"token": {"tok-wrong"}, "user_code": {"BBBB-BBBB"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(page, "doesn&#39;t match") && !strings.Contains(page, "doesn't match") {
		t.Errorf("missing specific inline error:\n%s", page)
	}
	if !strings.Contains(page, "4 attempts remaining") {
		t.Errorf("missing remaining-attempts count:\n%s", page)
	}
	// Preserved input + still a functioning form (token intact).
	if !strings.Contains(page, `value="BBBB-BBBB"`) {
		t.Errorf("typed code not preserved in the re-rendered form:\n%s", page)
	}
	if !strings.Contains(page, `value="tok-wrong"`) {
		t.Errorf("confirm token missing from the re-rendered form")
	}
	if !strings.Contains(page, `role="alert"`) {
		t.Errorf("inline error not announced (role=alert missing)")
	}

	pair, err := st.GetPair(context.Background(), nonce)
	if err != nil || pair == nil {
		t.Fatalf("pair row missing (err=%v)", err)
	}
	if pair.CodeAttempts != 1 || pair.Status != store.PairStatusPending {
		t.Errorf("pair = attempts %d status %s, want 1/pending", pair.CodeAttempts, pair.Status)
	}
}

func TestDeviceConfirmAttemptExhaustionInvalidatesPairing(t *testing.T) {
	app, st := newPairingApp(t)
	verifier := "device-on-chip-code-verifier-0123456789abcdef"
	nonce, userCode := registerPairing(t, app, st, verifier)
	cookies := seedConfirm(t, st, "tok-exhaust", nonce)

	form := url.Values{"token": {"tok-exhaust"}, "user_code": {"BBBB-BBBB"}}
	for i := 1; i < auth.MaxUserCodeAttempts; i++ {
		resp, _ := doForm(t, app, "/auth/device/pair/confirm", cookies, form)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("attempt %d: status = %d, want 400", i, resp.StatusCode)
		}
	}

	// Final wrong attempt: terminal.
	resp, page := doForm(t, app, "/auth/device/pair/confirm", cookies, form)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("final attempt: status = %d, want 410\n%s", resp.StatusCode, page)
	}
	if !strings.Contains(page, "Pairing cancelled") {
		t.Errorf("missing terminal failure page:\n%s", page)
	}
	if pc, _ := st.GetPairConfirm(context.Background(), "tok-exhaust"); pc != nil {
		t.Errorf("confirm row survived pairing invalidation")
	}

	// Device poll sees the terminal failed status with a machine reason.
	resp, body := doJSON(t, app, http.MethodPost, "/auth/device/pair/poll",
		map[string]any{"nonce": nonce, "codeVerifier": verifier})
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("poll: status = %d, want 410 (%v)", resp.StatusCode, body)
	}
	if body["status"] != "failed" || body["reason"] != pairFailedReason {
		t.Errorf("poll body = %v, want status=failed reason=%s", body, pairFailedReason)
	}

	// Even the correct code can never bind the invalidated pairing.
	cookies2 := seedConfirm(t, st, "tok-late", nonce)
	resp, _ = doForm(t, app, "/auth/device/pair/confirm", cookies2,
		url.Values{"token": {"tok-late"}, "user_code": {auth.FormatUserCode(userCode)}})
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("late correct code: status = %d, want 410", resp.StatusCode)
	}
}

func TestDeviceConfirmRequiresMatchingTokenAndCookie(t *testing.T) {
	app, st := newPairingApp(t)
	nonce, userCode := registerPairing(t, app, st, "verifier-verifier-verifier-verifier")
	seedConfirm(t, st, "tok-real", nonce)
	code := auth.FormatUserCode(userCode)

	cases := []struct {
		name    string
		cookies map[string]string
		token   string
	}{
		{"no cookie", nil, "tok-real"},
		{"no token field", map[string]string{PairConfirmCookieName: "tok-real"}, ""},
		{"token/cookie mismatch", map[string]string{PairConfirmCookieName: "tok-other"}, "tok-real"},
		{"unknown token pair", map[string]string{PairConfirmCookieName: "tok-fake"}, "tok-fake"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := doForm(t, app, "/auth/device/pair/confirm", tc.cookies,
				url.Values{"token": {tc.token}, "user_code": {code}})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}

	// None of those burned an attempt or advanced the pairing.
	pair, err := st.GetPair(context.Background(), nonce)
	if err != nil || pair == nil {
		t.Fatalf("pair row missing (err=%v)", err)
	}
	if pair.Status != store.PairStatusPending || pair.CodeAttempts != 0 {
		t.Errorf("pair = status %s attempts %d, want pending/0", pair.Status, pair.CodeAttempts)
	}
}

func TestDeviceConfirmEmptyCodeRepromptsWithoutBurningAttempt(t *testing.T) {
	app, st := newPairingApp(t)
	nonce, _ := registerPairing(t, app, st, "verifier-verifier-verifier-verifier")
	cookies := seedConfirm(t, st, "tok-empty", nonce)

	resp, page := doForm(t, app, "/auth/device/pair/confirm", cookies,
		url.Values{"token": {"tok-empty"}, "user_code": {"   "}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(page, "Enter the code shown on your device") {
		t.Errorf("missing empty-code prompt:\n%s", page)
	}
	pair, _ := st.GetPair(context.Background(), nonce)
	if pair.CodeAttempts != 0 {
		t.Errorf("empty submission burned an attempt (attempts = %d)", pair.CodeAttempts)
	}
}

func TestDeviceClaimPageForFailedPairing(t *testing.T) {
	app, st := newPairingApp(t)
	nonce, _ := registerPairing(t, app, st, "verifier-verifier-verifier-verifier")
	if err := st.UpdatePair(context.Background(), nonce,
		store.PairStatusPending, store.PairStatusFailed, "", ""); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/device/claim?nonce="+url.QueryEscape(nonce), nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want 410", resp.StatusCode)
	}
	page, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(page), "Pairing cancelled") {
		t.Errorf("claim page for failed pairing:\n%s", page)
	}
}
