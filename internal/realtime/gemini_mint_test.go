package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"
)

type geminiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f geminiRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
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
	assert.Equal(t, "models/gemini-3.1-flash-live-preview", gotCfg.LiveConnectConstraints.Model)
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

func TestGeminiMintUsesCurrentV1BetaRESTContract(t *testing.T) {
	// Synthetic marker only. No production credential is loaded by this test.
	const apiKey = "unit-test-api-key"
	t.Setenv(config.EnvOverrideGeminiAPIKey, apiKey)
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "")

	type capturedRequest struct {
		path          string
		apiKeyQuery   string
		apiKeyHeader  string
		authorization string
		body          []byte
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		captured <- capturedRequest{
			path:          r.URL.Path,
			apiKeyQuery:   r.URL.Query().Get("key"),
			apiKeyHeader:  r.Header.Get("x-goog-api-key"),
			authorization: r.Header.Get("Authorization"),
			body:          body,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"auth_tokens/fake-v1beta-token"}`)
	}))
	defer server.Close()

	minter := NewGeminiMinter(config.NewLoaderWithClient(nil), "gemini-test-model")
	minter.provisioningClient = server.Client()
	minter.provisioningEndpoint = server.URL + "/v1beta/auth_tokens"
	result, err := minter.Mint(context.Background(), "Puck", "You are terse.")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "auth_tokens/fake-v1beta-token", result.AccessToken.Value)

	got := <-captured
	assert.Equal(t, "/v1beta/auth_tokens", got.path)
	assert.Empty(t, got.apiKeyQuery)
	assert.Equal(t, apiKey, got.apiKeyHeader)
	assert.Empty(t, got.authorization)

	var wireBody map[string]any
	require.NoError(t, json.Unmarshal(got.body, &wireBody))
	assert.Equal(t, float64(1), wireBody["uses"])
	assert.NotContains(t, wireBody, "authToken")
	assert.NotContains(t, wireBody, "config")
	assert.NotContains(t, wireBody, "liveConnectConstraints")
	wireSetup, ok := wireBody["bidiGenerateContentSetup"].(map[string]any)
	require.True(t, ok)
	fieldMask, ok := wireBody["fieldMask"].(string)
	require.True(t, ok)
	maskFields := strings.Split(fieldMask, ",")
	assert.ElementsMatch(t, []string{
		"contextWindowCompression.slidingWindow",
		"generationConfig.responseModalities",
		"generationConfig.speechConfig",
		"inputAudioTranscription",
		"model",
		"outputAudioTranscription",
		"systemInstruction.parts",
		"tools",
	}, maskFields)
	assert.NotContains(t, maskFields, "sessionResumption",
		"the reconnecting client must be able to supply the latest resumption handle")
	assert.Equal(t, "models/gemini-test-model", wireSetup["model"])
	wireConfig, ok := wireSetup["generationConfig"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"AUDIO"}, wireConfig["responseModalities"])
	assert.Contains(t, wireSetup, "tools")
	assert.Contains(t, wireSetup, "sessionResumption")
	assert.Contains(t, wireSetup, "inputAudioTranscription")
	assert.Contains(t, wireSetup, "outputAudioTranscription")

	var returnedSetup map[string]any
	require.NoError(t, json.Unmarshal(result.SessionConfig, &returnedSetup))
	assert.Equal(t, returnedSetup, wireSetup,
		"the token-locked setup and client-returned SessionConfig must be identical")
}

func TestGeminiProvisioningClientDoesNotFollowCredentialRedirect(t *testing.T) {
	const apiKey = "unit-test-redirect-key"
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetHits.Add(1)
	}))
	defer target.Close()

	type capturedAuth struct {
		query, apiKeyHeader, authorization string
	}
	captured := make(chan capturedAuth, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedAuth{
			query:         r.URL.Query().Get("key"),
			apiKeyHeader:  r.Header.Get("x-goog-api-key"),
			authorization: r.Header.Get("Authorization"),
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	_, err := createGeminiAuthToken(
		context.Background(),
		geminiProvisioningHTTPClient(),
		origin.URL,
		apiKey,
		&genai.CreateAuthTokenConfig{},
		json.RawMessage(`{"model":"models/unit-test"}`),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "307 Temporary Redirect")
	assert.NotContains(t, err.Error(), apiKey)
	assert.Zero(t, targetHits.Load())
	got := <-captured
	assert.Empty(t, got.query)
	assert.Equal(t, apiKey, got.apiKeyHeader)
	assert.Empty(t, got.authorization)
}

func TestGeminiProvisioningTransportRedactsCredentialFromErrors(t *testing.T) {
	const apiKey = "unit-test-key+/="
	client := &http.Client{
		Transport: geminiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("failed request with %s", req.Header.Get("x-goog-api-key"))
		}),
	}

	_, err := createGeminiAuthToken(
		context.Background(),
		client,
		"https://generativelanguage.googleapis.com/v1beta/auth_tokens",
		apiKey,
		&genai.CreateAuthTokenConfig{},
		json.RawMessage(`{"model":"models/unit-test"}`),
	)
	require.Error(t, err)
	assert.Equal(t, "realtime: Gemini auth-token transport failed", err.Error())
	assert.NotContains(t, err.Error(), apiKey)
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

// TestGeminiLiveEndpointIsConstrainedV1Beta guards the current documented
// endpoint: ephemeral tokens are honored by the v1beta *Constrained* method,
// never the API-key BidiGenerateContent endpoint.
func TestGeminiLiveEndpointIsConstrainedV1Beta(t *testing.T) {
	assert.Contains(t, GeminiLiveEndpoint, "v1beta")
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
