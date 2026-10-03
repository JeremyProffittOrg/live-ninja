package realtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"
)

func assertJobsCatalog(t *testing.T, raw any, enabled bool) {
	t.Helper()
	b, err := json.Marshal(raw)
	require.NoError(t, err)
	for _, name := range []string{"job_list", "job_status", "job_create", "job_start", "job_pause", "job_resume", "job_cancel", "job_retry", "job_command"} {
		if enabled {
			require.Contains(t, string(b), `"`+name+`"`)
		} else {
			require.NotContains(t, string(b), `"`+name+`"`)
		}
	}
	require.Contains(t, string(b), `"get_weather"`, "unrelated tools remain available")
}

func TestJobsCapabilityBindsActualProviderAndFallbackSchemas(t *testing.T) {
	t.Setenv(config.EnvOverrideOpenAIAPIKey, "fake-unit-test-key")
	for _, tc := range []struct {
		name, surface string
		caps          []string
		enabled       bool
	}{
		{name: "legacy web", surface: "web"},
		{name: "legacy Android", surface: "android", caps: []string{"azure-direct"}},
		{name: "updated web", surface: "web", caps: []string{JobsReviewCapability}, enabled: true},
		{name: "updated Android", surface: "android", caps: []string{"voice-live-direct", JobsReviewCapability}, enabled: true},
		{name: "device cannot opt in", surface: "m5stack", caps: []string{JobsReviewCapability}},
		{name: "token exactness", surface: "web", caps: []string{"jobs-review-v10"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithClientCapabilities(context.Background(), tc.surface, tc.caps)
			m := NewMinter(config.NewLoaderWithClient(nil), DefaultRealtimeModel)
			var request struct {
				Session map[string]any `json:"session"`
			}
			m.httpc = &http.Client{Transport: mintRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				require.NoError(t, json.NewDecoder(req.Body).Decode(&request))
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"value":"fake-ephemeral","expires_at":1785096000}`))}, nil
			})}
			minted, err := m.Mint(ctx, "", "cedar", "", "", tc.surface)
			require.NoError(t, err)
			assertJobsCatalog(t, request.Session["tools"], tc.enabled)
			assertJobsCatalog(t, minted.ToolManifest, tc.enabled)
			require.Equal(t, tc.enabled, strings.Contains(request.Session["instructions"].(string), "job_list"))

			var constraints *genai.CreateAuthTokenConfig
			gm := &GeminiMinter{model: "fake-gemini", create: func(_ context.Context, cfg *genai.CreateAuthTokenConfig) (*genai.AuthToken, error) {
				constraints = cfg
				return &genai.AuthToken{Name: "auth_tokens/fake"}, nil
			}}
			gemini, err := gm.MintForSurface(ctx, "Puck", InstructionsForSurface(ResolvePersona(""), tc.surface), tc.surface)
			require.NoError(t, err)
			assertJobsCatalog(t, gemini.ToolManifest, tc.enabled)
			var setup map[string]any
			require.NoError(t, json.Unmarshal(gemini.SessionConfig, &setup))
			assertJobsCatalog(t, setup["tools"], tc.enabled)
			assertJobsCatalog(t, constraints.LiveConnectConstraints.Config.Tools, tc.enabled)
			require.Equal(t, tc.enabled, strings.Contains(constraints.LiveConnectConstraints.Config.SystemInstruction.Parts[0].Text, "job_list"))

			var fallback map[string]any
			fc := newMockOpenAIClient(t, func(w http.ResponseWriter, req *http.Request) {
				require.NoError(t, json.NewDecoder(req.Body).Decode(&fallback))
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			})
			_, err = fc.TurnWithToolsForSurface(ctx, "", tc.surface, []ChatMessage{{Role: "user", Content: "hello"}}, "")
			require.NoError(t, err)
			assertJobsCatalog(t, fallback["tools"], tc.enabled)
			messages := fallback["messages"].([]any)
			require.Equal(t, tc.enabled, strings.Contains(messages[0].(map[string]any)["content"].(string), "job_list"))
		})
	}
}

func TestJobsCapabilityDefaultsClosedAndNeverMutatesSharedCatalog(t *testing.T) {
	before, err := json.Marshal(toolManifest)
	require.NoError(t, err)
	assertJobsCatalog(t, ToolManifestJSONForClient(context.Background(), "web", false), false)
	assertJobsCatalog(t, ToolManifestJSONForClient(WithClientCapabilities(context.Background(), "web", []string{JobsReviewCapability}), "web", false), true)
	after, err := json.Marshal(toolManifest)
	require.NoError(t, err)
	require.Equal(t, before, after)
	nova := BuildNovaSessionConfigForClient(context.Background(), InstructionsForServerExecution(ResolvePersona("")))
	assertJobsCatalog(t, nova.Tools, false)
	require.NotContains(t, nova.SystemPrompt, "job_list")
}
