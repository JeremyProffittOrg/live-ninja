package realtime

import (
	"cloud.google.com/go/auth"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"
)

type staticGeminiTokenProvider struct {
	token *auth.Token
	err   error
}

func (p staticGeminiTokenProvider) Token(context.Context) (*auth.Token, error) {
	return p.token, p.err
}

type geminiRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f geminiRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestGeminiMintBuildsConstrainedTokenAndSetup drives Mint through a fake
// token creator and pins the M13 wire contract: single-use 30-min token,
// constraints locking model/voice/instructions, and a SessionConfig echo in
// the RAW wire shape (generationConfig nesting — the Phase 0 spike proved
// the SDK's flattened shape is NOT what the socket accepts).
func TestGeminiMintBuildsConstrainedTokenAndSetup(t *testing.T) {
	var gotCfg *genai.CreateAuthTokenConfig
	m := &GeminiMinter{
		model: "gemini-3.1-flash-live-preview",
		create: func(_ context.Context, cfg *genai.CreateAuthTokenConfig) (*genai.AuthToken, error) {
			gotCfg = cfg
			return &genai.AuthToken{Name: "auth_tokens/fake"}, nil
		},
	}

	res, err := m.Mint(context.Background(), "Puck", "You are terse.")
	require.NoError(t, err)

	// Token + windows.
	assert.Equal(t, "auth_tokens/fake", res.AccessToken.Value)
	require.NotNil(t, gotCfg)
	require.NotNil(t, gotCfg.Uses)
	assert.Equal(t, int32(1), *gotCfg.Uses)
	assert.InDelta(t, geminiTokenTTL, time.Until(gotCfg.ExpireTime), float64(time.Minute))
	assert.InDelta(t, geminiNewSessionWindow, time.Until(gotCfg.NewSessionExpireTime), float64(time.Minute))

	// Constraints lock the exact session the client is allowed to open.
	require.NotNil(t, gotCfg.LiveConnectConstraints)
	assert.Equal(t, "gemini-3.1-flash-live-preview", gotCfg.LiveConnectConstraints.Model)
	cc := gotCfg.LiveConnectConstraints.Config
	require.NotNil(t, cc)
	assert.Equal(t, "Puck", cc.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName)
	assert.Equal(t, "You are terse.", cc.SystemInstruction.Parts[0].Text)
	assert.NotNil(t, cc.SessionResumption)
	assert.NotNil(t, cc.ContextWindowCompression.SlidingWindow)
	assert.NotNil(t, cc.InputAudioTranscription)
	assert.NotNil(t, cc.OutputAudioTranscription)
	// D3 (was count-only: len(toolManifest) == len(cc.Tools[0].FunctionDeclarations),
	// exactly the weak assertion form that let the manifest/registry drift
	// (P2) survive undetected). Assert content: every manifest tool crosses
	// the JSON round trip into the SDK-typed declarations in order, and one
	// representative tool with every string constraint kind (file_create)
	// carries its minLength/maxLength/pattern/required intact — proving
	// genai.Schema really does model those keywords (gemini_mint.go
	// geminiSchemaKeywords) and the D1 sanitizer left them untouched.
	require.NotEmpty(t, cc.Tools)
	gotDecls := cc.Tools[0].FunctionDeclarations
	require.Len(t, gotDecls, len(toolManifest))
	for i, d := range gotDecls {
		assert.Equal(t, toolManifest[i]["name"], d.Name, "declaration %d name", i)
	}

	var fileCreate *genai.FunctionDeclaration
	for _, d := range gotDecls {
		if d.Name == "file_create" {
			fileCreate = d
			break
		}
	}
	require.NotNil(t, fileCreate, "file_create must be present in the SDK-typed declarations")
	require.NotNil(t, fileCreate.Parameters)
	nameSchema := fileCreate.Parameters.Properties["name"]
	require.NotNil(t, nameSchema, "file_create.name schema must survive the SDK round trip")
	require.NotNil(t, nameSchema.MinLength)
	assert.EqualValues(t, 1, *nameSchema.MinLength)
	require.NotNil(t, nameSchema.MaxLength)
	assert.EqualValues(t, 100, *nameSchema.MaxLength)
	assert.Equal(t, "^[A-Za-z0-9][A-Za-z0-9._-]*$", nameSchema.Pattern)
	assert.Contains(t, fileCreate.Parameters.Required, "name")
	assert.Contains(t, fileCreate.Parameters.Required, "content")

	// SessionConfig echo: raw wire nesting.
	var setup map[string]any
	require.NoError(t, json.Unmarshal(res.SessionConfig, &setup))
	assert.Equal(t, "models/gemini-3.1-flash-live-preview", setup["model"])
	gen, ok := setup["generationConfig"].(map[string]any)
	require.True(t, ok, "responseModalities/speechConfig must nest under generationConfig on the wire")
	assert.Equal(t, []any{"AUDIO"}, gen["responseModalities"])
	assert.Contains(t, setup, "systemInstruction")
	assert.Contains(t, setup, "sessionResumption")
	assert.Contains(t, setup, "contextWindowCompression")
	assert.Contains(t, setup, "inputAudioTranscription")
	assert.Contains(t, setup, "outputAudioTranscription")

	// The wsUrl-family ban applies to the whole bootstrap payload; guard the
	// config blob too.
	lower := strings.ToLower(string(res.SessionConfig))
	assert.NotContains(t, lower, "wsurl")
	assert.NotContains(t, lower, "bridgeurl")
}

func TestGeminiRESTTokenMintUsesBearerAndAPIKeyQuery(t *testing.T) {
	t.Parallel()

	// Synthetic markers only. No credential or service-account value is loaded
	// by this test.
	const apiKey = "unit-test-api-key"
	const oauthToken = "unit-test-oauth-token"
	expiresAt := time.Date(2026, 7, 25, 21, 30, 0, 0, time.UTC)
	newSessionExpiresAt := time.Date(2026, 7, 25, 21, 2, 0, 0, time.UTC)
	uses := int32(1)
	setup := buildGeminiSetup("gemini-test-model", "Puck", "You are terse.")
	cfg := &genai.CreateAuthTokenConfig{
		ExpireTime:           expiresAt,
		NewSessionExpireTime: newSessionExpiresAt,
		Uses:                 &uses,
	}

	type capturedRequest struct {
		method        string
		path          string
		apiKey        string
		authorization string
		apiKeyHeader  string
		contentType   string
		body          []byte
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		captured <- capturedRequest{
			method:        r.Method,
			path:          r.URL.Path,
			apiKey:        r.URL.Query().Get("key"),
			authorization: r.Header.Get("Authorization"),
			apiKeyHeader:  r.Header.Get("x-goog-api-key"),
			contentType:   r.Header.Get("Content-Type"),
			body:          body,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"auth_tokens/fake-rest-token"}`)
	}))
	defer server.Close()

	tok, err := createGeminiAuthTokenREST(
		context.Background(),
		server.Client(),
		server.URL+"/v1alpha/auth_tokens",
		apiKey,
		staticGeminiTokenProvider{token: &auth.Token{Value: oauthToken}},
		cfg,
		setup,
	)
	require.NoError(t, err)
	require.NotNil(t, tok)
	assert.Equal(t, "auth_tokens/fake-rest-token", tok.Name)

	got := <-captured
	assert.Equal(t, http.MethodPost, got.method)
	assert.Equal(t, "/v1alpha/auth_tokens", got.path)
	assert.Equal(t, apiKey, got.apiKey)
	assert.Equal(t, "Bearer "+oauthToken, got.authorization)
	assert.Empty(t, got.apiKeyHeader, "the API key must not be sent as an auth header")
	assert.Equal(t, "application/json", got.contentType)

	wantBody, err := json.Marshal(geminiAuthTokenRESTRequest{
		ExpireTime:               expiresAt,
		NewSessionExpireTime:     newSessionExpiresAt,
		Uses:                     &uses,
		BidiGenerateContentSetup: setup,
	})
	require.NoError(t, err)
	assert.JSONEq(t, string(wantBody), string(got.body))

	var wireBody map[string]any
	require.NoError(t, json.Unmarshal(got.body, &wireBody))
	require.Len(t, wireBody, 4)
	assert.Equal(t, float64(1), wireBody["uses"])
	assert.Equal(t, expiresAt.Format(time.RFC3339Nano), wireBody["expireTime"])
	assert.Equal(t, newSessionExpiresAt.Format(time.RFC3339Nano), wireBody["newSessionExpireTime"])
	assert.NotContains(t, wireBody, "authToken")
	assert.NotContains(t, wireBody, "config")
	wireSetup, ok := wireBody["bidiGenerateContentSetup"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "models/gemini-test-model", wireSetup["model"])
}

func TestGeminiMintFallsBackToSDKAPIKeyWithoutServiceAccount(t *testing.T) {
	// Synthetic marker only. A whitespace value is intentionally treated as
	// an absent optional service-account credential without reaching SSM.
	const apiKey = "unit-test-fallback-api-key"
	t.Setenv(config.EnvOverrideGeminiServiceAccountJSON, " ")
	t.Setenv(config.EnvOverrideGeminiAPIKey, apiKey)
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "")

	type capturedRequest struct {
		path          string
		apiKeyQuery   string
		apiKeyHeader  string
		authorization string
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedRequest{
			path:          r.URL.Path,
			apiKeyQuery:   r.URL.Query().Get("key"),
			apiKeyHeader:  r.Header.Get("x-goog-api-key"),
			authorization: r.Header.Get("Authorization"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"auth_tokens/fake-fallback-token"}`)
	}))
	defer server.Close()
	t.Setenv("GOOGLE_GEMINI_BASE_URL", server.URL+"/")

	minter := NewGeminiMinter(config.NewLoaderWithClient(nil), "gemini-test-model")
	result, err := minter.Mint(context.Background(), "Puck", "You are terse.")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "auth_tokens/fake-fallback-token", result.AccessToken.Value)

	got := <-captured
	assert.Equal(t, "/v1alpha/auth_tokens", got.path)
	assert.Empty(t, got.apiKeyQuery)
	assert.Equal(t, apiKey, got.apiKeyHeader)
	assert.Empty(t, got.authorization)
}

func TestGeminiRESTTokenMintRedactsCredentialsFromErrors(t *testing.T) {
	t.Parallel()

	const apiKey = "unit-test-key+/="
	const oauthToken = "unit-test-token+/="
	uses := int32(1)
	cfg := &genai.CreateAuthTokenConfig{
		ExpireTime:           time.Now().Add(time.Hour),
		NewSessionExpireTime: time.Now().Add(time.Minute),
		Uses:                 &uses,
	}
	provider := staticGeminiTokenProvider{token: &auth.Token{Value: oauthToken}}

	t.Run("API response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"status": "UNAUTHENTICATED",
					"message": fmt.Sprintf("rejected key %s (%s) and token %s",
						apiKey, url.QueryEscape(apiKey), oauthToken),
				},
			})
		}))
		defer server.Close()

		_, err := createGeminiAuthTokenREST(
			context.Background(), server.Client(), server.URL+"/v1alpha/auth_tokens",
			apiKey, provider, cfg, map[string]any{"model": "models/test"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "401 Unauthorized")
		assert.Contains(t, err.Error(), "[REDACTED]")
		assert.NotContains(t, err.Error(), apiKey)
		assert.NotContains(t, err.Error(), url.QueryEscape(apiKey))
		assert.NotContains(t, err.Error(), oauthToken)
	})

	t.Run("transport error", func(t *testing.T) {
		client := &http.Client{
			Transport: geminiRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("failed URL %s with %s", req.URL, req.Header.Get("Authorization"))
			}),
		}
		_, err := createGeminiAuthTokenREST(
			context.Background(), client, geminiAuthTokensEndpoint,
			apiKey, provider, cfg, map[string]any{"model": "models/test"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "[REDACTED]")
		assert.NotContains(t, err.Error(), apiKey)
		assert.NotContains(t, err.Error(), url.QueryEscape(apiKey))
		assert.NotContains(t, err.Error(), oauthToken)
	})
}

// TestGeminiToolDeclarationsMirrorManifest: every OpenAI-manifest tool
// crosses to Gemini with name intact, description at least prefix-equal
// (D-c), sanitized-but-equivalent parameters (D1), and the OpenAI "type"
// discriminator dropped.
//
// D-c: this is the ONE sanctioned exception to "a parity test needing
// edits is a bug" (tool-parity-plan.md B4). Folding stripped constraints
// into description prose (Q4) means the Gemini description is no longer
// required to be byte-identical to the manifest's — only to start with it.
// At today's SDK pin (genai v1.64.0) every keyword the 20 real tools use
// (minLength/maxLength/pattern/minimum/maximum/enum) IS modeled by
// genai.Schema (verified by reading types.go's Schema struct, see
// gemini_mint.go geminiSchemaKeywords), so nothing is actually stripped
// from any of today's tools and the "begins with" check currently holds as
// exact equality with an empty suffix. The assertion is written as a
// prefix check anyway (not reverted to Equal) because that is what stays
// correct if a future tool's ParamSpec ever needs a keyword genai.Schema
// doesn't model — see gemini_schema_sanitizer_test.go for the sanitizer
// exercised against synthetic schemas that DO trigger stripping.
func TestGeminiToolDeclarationsMirrorManifest(t *testing.T) {
	decls := geminiToolDeclarations()
	require.Equal(t, len(toolManifest), len(decls))
	for i, d := range decls {
		manifestDesc, _ := toolManifest[i]["description"].(string)
		geminiDesc, _ := d["description"].(string)
		assert.Equal(t, toolManifest[i]["name"], d["name"])
		assert.True(t, strings.HasPrefix(geminiDesc, manifestDesc),
			"tool %v: gemini description %q must begin with manifest description %q",
			toolManifest[i]["name"], geminiDesc, manifestDesc)
		if suffix := strings.TrimPrefix(geminiDesc, manifestDesc); suffix != "" {
			// Any appended text must be sanitizer-generated prose, not a
			// hand-authored per-tool addition — it always reads as one or
			// more space-joined sentences ending in '.'.
			assert.True(t, strings.HasPrefix(suffix, " "), "appended suffix must be space-separated: %q", suffix)
			assert.True(t, strings.HasSuffix(strings.TrimSpace(suffix), "."), "appended suffix must be sentence(s): %q", suffix)
		}
		// Parameters equality is untouched by D-c: at today's SDK pin
		// nothing is actually stripped from any real tool (see the doc
		// comment above), so the sanitized copy is still exactly equal in
		// content to the manifest's — only its identity differs (D1's
		// deep-copy fix). If this ever fails because a future ParamSpec
		// keyword needs sanitizing, that is real drift to fix at the
		// source, per B4 — not license to weaken this assertion too.
		assert.Equal(t, toolManifest[i]["parameters"], d["parameters"])
		assert.NotContains(t, d, "type")
	}
}

// TestGeminiLiveEndpointIsConstrainedV1Alpha guards the Phase-0-proven
// endpoint: ephemeral tokens are only honored by the v1alpha *Constrained*
// method (gemini-plan.md §10).
func TestGeminiLiveEndpointIsConstrainedV1Alpha(t *testing.T) {
	assert.Contains(t, GeminiLiveEndpoint, "v1alpha")
	assert.True(t, strings.HasSuffix(GeminiLiveEndpoint, "BidiGenerateContentConstrained"))
}

func TestResolveGeminiVoiceChain(t *testing.T) {
	cases := []struct {
		setting, persona, want string
	}{
		{"Puck", "Kore", "Puck"},             // setting wins
		{"bogus", "Kore", "Kore"},            // unknown setting falls through
		{"", "Vindemiatrix", "Vindemiatrix"}, // persona mapping
		{"", "bogus", "Kore"},                // unknown persona voice -> default
		{"", "", "Kore"},                     // bottom of chain
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, ResolveGeminiVoiceChain(tc.setting, tc.persona),
			"setting=%q persona=%q", tc.setting, tc.persona)
	}
}

// TestEveryBuiltinPersonaHasValidGeminiVoice keeps the D4b mapping total and
// catalog-valid: a persona added without a Gemini voice (or with a typo)
// fails here instead of silently minting Kore.
func TestEveryBuiltinPersonaHasValidGeminiVoice(t *testing.T) {
	for _, p := range BuiltinPersonas() {
		assert.Truef(t, IsGeminiVoice(p.GeminiVoice),
			"persona %q has invalid geminiVoice %q", p.ID, p.GeminiVoice)
	}
}

// TestGeminiVoiceCatalogMatchesSpikeValidation pins the shipped catalog to
// the 30 spike-accepted names (Phase 0 T1) and the Kore default.
func TestGeminiVoiceCatalogMatchesSpikeValidation(t *testing.T) {
	assert.Equal(t, 30, len(SupportedGeminiVoices))
	defaults := 0
	for _, v := range SupportedGeminiVoices {
		if v.Default {
			defaults++
			assert.Equal(t, DefaultGeminiVoice, v.ID)
		}
	}
	assert.Equal(t, 1, defaults, "exactly one default Gemini voice")
}
