package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rewriteDoer routes the LWAClient's hardcoded amazon.com requests to a
// local httptest server standing in for LWA, preserving path and query.
type rewriteDoer struct {
	server *httptest.Server
}

func (d *rewriteDoer) Do(req *http.Request) (*http.Response, error) {
	target, _ := url.Parse(d.server.URL)
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	req.Host = target.Host
	return d.server.Client().Do(req)
}

// newMockLWA builds an httptest server emulating the three LWA endpoints.
// tokeninfoAud controls the aud the mock returns — the substitution knob.
func newMockLWA(t *testing.T, tokeninfoAud, tokeninfoUserID, profileUserID string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/o2/token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		if r.PostForm.Get("grant_type") != "authorization_code" ||
			r.PostForm.Get("code") == "" ||
			r.PostForm.Get("code_verifier") == "" ||
			r.PostForm.Get("client_secret") == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_request"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"atza|mock-access","refresh_token":"atzr|mock-refresh","token_type":"bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/auth/o2/tokeninfo", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("access_token") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"aud":"` + tokeninfoAud + `","user_id":"` + tokeninfoUserID + `","app_id":"amzn1.application.x","exp":"3600"}`))
	})
	mux.HandleFunc("/user/profile", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"user_id":"` + profileUserID + `","email":"Jane@Example.com","name":"Jane"}`))
	})
	return httptest.NewServer(mux)
}

const mockClientID = "amzn1.application-oa2-client.mock"

func TestBuildAuthorizeURL(t *testing.T) {
	c := newLWAClientForTest(mockClientID, "secret", nil)
	raw := c.BuildAuthorizeURL("state-1", "challenge-1", "https://live.jeremy.ninja/auth/lwa/callback")

	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "www.amazon.com", u.Host)
	assert.Equal(t, "/ap/oa", u.Path)
	q := u.Query()
	assert.Equal(t, mockClientID, q.Get("client_id"))
	assert.Equal(t, "profile", q.Get("scope"))
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "state-1", q.Get("state"))
	assert.Equal(t, "challenge-1", q.Get("code_challenge"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.Equal(t, "https://live.jeremy.ninja/auth/lwa/callback", q.Get("redirect_uri"))
}

func TestExchangeCodeHappyPath(t *testing.T) {
	srv := newMockLWA(t, mockClientID, "amzn1.account.u1", "amzn1.account.u1")
	defer srv.Close()
	c := newLWAClientForTest(mockClientID, "secret", &rewriteDoer{server: srv})

	tokens, err := c.ExchangeCode(context.Background(), "code-1", "verifier-1", "https://live.jeremy.ninja/auth/lwa/callback")
	require.NoError(t, err)
	assert.Equal(t, "atza|mock-access", tokens.AccessToken)
	assert.Equal(t, "atzr|mock-refresh", tokens.RefreshToken)
}

func TestExchangeCodeFailureStatus(t *testing.T) {
	srv := newMockLWA(t, mockClientID, "u", "u")
	defer srv.Close()
	c := newLWAClientForTest(mockClientID, "secret", &rewriteDoer{server: srv})

	// Missing code -> mock returns 400; the error must carry only the
	// status, never the body (which may echo secrets).
	_, err := c.ExchangeCode(context.Background(), "", "verifier-1", "https://cb")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 400")
	assert.NotContains(t, err.Error(), "invalid_request")
}

func TestValidateHappyPath(t *testing.T) {
	srv := newMockLWA(t, mockClientID, "amzn1.account.u1", "amzn1.account.u1")
	defer srv.Close()
	c := newLWAClientForTest(mockClientID, "secret", &rewriteDoer{server: srv})

	profile, err := c.Validate(context.Background(), "atza|mock-access")
	require.NoError(t, err)
	assert.Equal(t, "amzn1.account.u1", profile.UserID)
	assert.Equal(t, "Jane@Example.com", profile.Email)
	assert.Equal(t, "Jane", profile.Name)
}

func TestValidateRejectsAudSubstitution(t *testing.T) {
	// A token minted for a DIFFERENT application: tokeninfo succeeds but
	// aud does not match our client_id. Validate must fail with
	// ErrAudienceMismatch — the confused-deputy guard.
	srv := newMockLWA(t, "amzn1.application-oa2-client.SOMEOTHERAPP", "amzn1.account.u1", "amzn1.account.u1")
	defer srv.Close()
	c := newLWAClientForTest(mockClientID, "secret", &rewriteDoer{server: srv})

	_, err := c.Validate(context.Background(), "atza|stolen-token")
	require.ErrorIs(t, err, ErrAudienceMismatch)
}

func TestValidateRejectsEmptyAud(t *testing.T) {
	srv := newMockLWA(t, "", "amzn1.account.u1", "amzn1.account.u1")
	defer srv.Close()
	c := newLWAClientForTest(mockClientID, "secret", &rewriteDoer{server: srv})

	_, err := c.Validate(context.Background(), "atza|token")
	require.ErrorIs(t, err, ErrAudienceMismatch)
}

func TestValidateRejectsTokenInfoFailure(t *testing.T) {
	srv := newMockLWA(t, mockClientID, "u1", "u1")
	defer srv.Close()
	c := newLWAClientForTest(mockClientID, "secret", &rewriteDoer{server: srv})

	// Empty access token is rejected before any HTTP call.
	_, err := c.Validate(context.Background(), "")
	require.Error(t, err)

	// A tokeninfo non-200 maps to ErrTokenInfoFailed.
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/o2/tokeninfo", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	failSrv := httptest.NewServer(mux)
	defer failSrv.Close()
	c2 := newLWAClientForTest(mockClientID, "secret", &rewriteDoer{server: failSrv})
	_, err = c2.Validate(context.Background(), "atza|bad")
	require.ErrorIs(t, err, ErrTokenInfoFailed)
}

func TestValidateRejectsUserIDMismatch(t *testing.T) {
	// tokeninfo and profile disagree on user_id -> reject (defense in depth).
	srv := newMockLWA(t, mockClientID, "amzn1.account.AAA", "amzn1.account.BBB")
	defer srv.Close()
	c := newLWAClientForTest(mockClientID, "secret", &rewriteDoer{server: srv})

	_, err := c.Validate(context.Background(), "atza|token")
	require.ErrorIs(t, err, ErrProfileFailed)
}

func TestValidateRejectsMissingProfileUserID(t *testing.T) {
	srv := newMockLWA(t, mockClientID, "amzn1.account.u1", "")
	defer srv.Close()
	c := newLWAClientForTest(mockClientID, "secret", &rewriteDoer{server: srv})

	_, err := c.Validate(context.Background(), "atza|token")
	require.ErrorIs(t, err, ErrProfileFailed)
}
