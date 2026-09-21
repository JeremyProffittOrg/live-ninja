package realtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEntraTokenCachesUntilEightyPercent(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "client_credentials", r.Form.Get("grant_type"))
		assert.Equal(t, VoiceLiveScope, r.Form.Get("scope"))
		assert.Equal(t, "client-id", r.Form.Get("client_id"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type":   "Bearer",
			"expires_in":   1000,
			"access_token": "tok-one",
		})
	}))
	t.Cleanup(srv.Close)

	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	c := NewEntraTokenClient(config.NewLoaderWithClient(nil), EntraTenantID)
	c.httpc = srv.Client()
	c.tokenURL = srv.URL
	c.now = func() time.Time { return now }
	t.Setenv(config.EnvOverrideVoiceLiveClientID, "client-id")
	t.Setenv(config.EnvOverrideVoiceLiveClientSecret, "client-secret")

	tok, err := c.Token(context.Background(), VoiceLiveScope)
	require.NoError(t, err)
	assert.Equal(t, "tok-one", tok.Value)
	assert.Equal(t, now.Add(1000*time.Second), tok.ExpiresAt)
	assert.Equal(t, 1, hits)

	now = now.Add(700 * time.Second) // 70% of 1000s — still inside 80%
	tok, err = c.Token(context.Background(), VoiceLiveScope)
	require.NoError(t, err)
	assert.Equal(t, "tok-one", tok.Value)
	assert.Equal(t, 1, hits, "cached token must be reused before 80% of lifetime")

	now = now.Add(200 * time.Second) // 90% — must refresh
	tok, err = c.Token(context.Background(), VoiceLiveScope)
	require.NoError(t, err)
	assert.Equal(t, "tok-one", tok.Value)
	assert.Equal(t, 2, hits)
}

func TestEntraTokenErrorNeverIncludesToken(t *testing.T) {
	const leaked = "super-secret-access-token-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_client","access_token":"`+leaked+`"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewEntraTokenClient(config.NewLoaderWithClient(nil), EntraTenantID)
	c.httpc = srv.Client()
	c.tokenURL = srv.URL
	t.Setenv(config.EnvOverrideVoiceLiveClientID, "client-id")
	t.Setenv(config.EnvOverrideVoiceLiveClientSecret, "client-secret")

	_, err := c.Token(context.Background(), VoiceLiveScope)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), leaked)
	assert.NotContains(t, strings.ToLower(err.Error()), "access_token")
}
