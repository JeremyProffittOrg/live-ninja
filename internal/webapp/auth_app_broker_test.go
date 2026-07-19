package webapp

// Tests for the Android broker claim leg (auth_routes.go appClaim): the
// PKCE-bound, single-use handoff code is exchanged for a real session, and
// a wrong verifier / reused code / unknown code are all refused. The
// app-login → callback legs hit Amazon's LWA endpoints, so they are covered
// by internal/auth; here we seed the APPHANDOFF row the callback would have
// written and drive the claim the app makes.

import (
	"context"
	"net/http"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// seedHandoff plants an authorized user + the APPHANDOFF row the broker
// callback writes, PKCE-bound to codeVerifier. Returns the handoff code.
func seedHandoff(t *testing.T, st *store.Store, code, userID, codeVerifier string) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateUser(ctx, &store.User{
		UserID:       userID,
		AmazonUserID: "amzn1.account.broker",
		Email:        "owner@example.com",
		Name:         "Owner",
		Role:         store.RoleOwner,
		Status:       store.UserStatusActive,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := st.PutAppHandoff(ctx, &store.AppHandoff{
		Code:         code,
		UserID:       userID,
		AppChallenge: base64RawURLSHA256(codeVerifier),
	}); err != nil {
		t.Fatalf("seed handoff: %v", err)
	}
}

func TestAppClaimSuccess(t *testing.T) {
	app, st := newPairingApp(t)
	verifier := "verifier-verifier-verifier-verifier-verifier"
	seedHandoff(t, st, "code-1", "USER#broker", verifier)

	resp, body := doJSON(t, app, http.MethodPost, "/auth/lwa/app-claim",
		map[string]any{"code": "code-1", "codeVerifier": verifier})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", resp.StatusCode, body)
	}
	if body["accessToken"] == "" || body["accessToken"] == nil {
		t.Errorf("no accessToken in grant: %v", body)
	}
	refresh, _ := body["refreshToken"].(string)
	if refresh == "" {
		t.Errorf("no refreshToken in grant: %v", body)
	}
	if sid, _ := body["sessionId"].(string); sid == "" {
		t.Errorf("no sessionId in grant: %v", body)
	}
}

func TestAppClaimIsSingleUse(t *testing.T) {
	app, st := newPairingApp(t)
	verifier := "verifier-verifier-verifier-verifier-verifier"
	seedHandoff(t, st, "code-2", "USER#broker", verifier)

	resp, _ := doJSON(t, app, http.MethodPost, "/auth/lwa/app-claim",
		map[string]any{"code": "code-2", "codeVerifier": verifier})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first claim status = %d, want 200", resp.StatusCode)
	}
	// A second claim of the same code must fail — the row was consumed.
	resp2, _ := doJSON(t, app, http.MethodPost, "/auth/lwa/app-claim",
		map[string]any{"code": "code-2", "codeVerifier": verifier})
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401", resp2.StatusCode)
	}
}

func TestAppClaimWrongVerifierRejected(t *testing.T) {
	app, st := newPairingApp(t)
	seedHandoff(t, st, "code-3", "USER#broker", "the-real-verifier-the-real-verifier-abcdef")

	resp, _ := doJSON(t, app, http.MethodPost, "/auth/lwa/app-claim",
		map[string]any{"code": "code-3", "codeVerifier": "a-different-verifier-entirely-0123456789"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (verifier mismatch)", resp.StatusCode)
	}
}

func TestAppClaimUnknownCodeRejected(t *testing.T) {
	app, _ := newPairingApp(t)
	resp, _ := doJSON(t, app, http.MethodPost, "/auth/lwa/app-claim",
		map[string]any{"code": "no-such-code", "codeVerifier": "whatever-whatever-whatever-whatever-what"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (unknown code)", resp.StatusCode)
	}
}

func TestAppClaimMissingFields(t *testing.T) {
	app, _ := newPairingApp(t)
	resp, _ := doJSON(t, app, http.MethodPost, "/auth/lwa/app-claim",
		map[string]any{"code": "code-only"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (missing codeVerifier)", resp.StatusCode)
	}
}
