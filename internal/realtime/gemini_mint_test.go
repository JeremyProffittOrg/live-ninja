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
	require.NotEmpty(t, cc.Tools)
	assert.Equal(t, len(toolManifest), len(cc.Tools[0].FunctionDeclarations))

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
// crosses to Gemini with name/description/parameters intact and the OpenAI
// "type" discriminator dropped.
func TestGeminiToolDeclarationsMirrorManifest(t *testing.T) {
	decls := geminiToolDeclarations()
	require.Equal(t, len(toolManifest), len(decls))
	for i, d := range decls {
		assert.Equal(t, toolManifest[i]["name"], d["name"])
		assert.Equal(t, toolManifest[i]["description"], d["description"])
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
