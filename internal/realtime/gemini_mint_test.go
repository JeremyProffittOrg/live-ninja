package realtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"
)

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
		{"Puck", "Kore", "Puck"},           // setting wins
		{"bogus", "Kore", "Kore"},          // unknown setting falls through
		{"", "Vindemiatrix", "Vindemiatrix"}, // persona mapping
		{"", "bogus", "Kore"},              // unknown persona voice -> default
		{"", "", "Kore"},                   // bottom of chain
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
