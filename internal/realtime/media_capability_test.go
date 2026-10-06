package realtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"
)

func TestClientSupportsAndroidMedia(t *testing.T) {
	assert.True(t, ClientSupportsAndroidMedia("android", []string{AndroidMediaCapability}))
	assert.True(t, ClientSupportsAndroidMedia("android", []string{JobsReviewCapability, AndroidMediaCapability}))
	assert.False(t, ClientSupportsAndroidMedia("android", nil))
	assert.False(t, ClientSupportsAndroidMedia("android", []string{JobsReviewCapability}))
	for _, surface := range []string{"web", "device", "m5stack", ""} {
		assert.False(t, ClientSupportsAndroidMedia(surface, []string{AndroidMediaCapability}), surface)
	}
	assert.False(t, androidMediaEnabled(context.Background()))
}

// TestAndroidMediaCapabilityTokenIsExact pins the wire token the Android
// client sends (AuthInterceptor ClientId.CAPABILITIES) and proves only the
// exact token enables the tool: near-misses, case changes, padding, a
// different version or an unsplit header value never do.
func TestAndroidMediaCapabilityTokenIsExact(t *testing.T) {
	assert.Equal(t, "android-media-v1", AndroidMediaCapability)

	exact := WithClientCapabilities(context.Background(), "android", []string{"android-media-v1"})
	assert.True(t, androidMediaEnabled(exact))
	assert.True(t, manifestNamesFromJSON(t, ToolManifestJSONForClient(exact, "android", false))["play_media"],
		"positive control: the exact token advertises play_media on android")

	for _, near := range []string{
		"",
		"android-media",
		"android-media-v2",
		"android-media-v10",
		"Android-Media-V1",
		"ANDROID-MEDIA-V1",
		" android-media-v1",
		"android-media-v1 ",
		"x-android-media-v1",
		"android-media-v1,jobs-review-v1",
		"azure-direct,voice-live-direct,jobs-review-v1,android-media-v1",
	} {
		assert.False(t, ClientSupportsAndroidMedia("android", []string{near}), "%q", near)
		ctx := WithClientCapabilities(context.Background(), "android", []string{near})
		assert.False(t, androidMediaEnabled(ctx), "%q", near)
		assert.False(t, manifestNamesFromJSON(t, ToolManifestJSONForClient(ctx, "android", false))["play_media"], "%q", near)
		assert.NotContains(t, ClientInstructions(ctx, InstructionsForSurface(ResolvePersona(""), "android")), "play_media", "%q", near)
	}
}

func TestFilterClientToolsComposesJobsAndMediaGates(t *testing.T) {
	manifest := []map[string]any{
		{"name": "send_email"},
		{"name": "job_list"},
		{"name": "set_volume"},
		{"name": "play_media"},
		{"type": "function", "function": map[string]any{"name": "play_media"}},
		{"type": "function", "function": map[string]any{"name": "job_status"}},
	}
	names := func(entries []map[string]any) []string {
		var out []string
		for _, e := range entries {
			name, _ := e["name"].(string)
			if fn, ok := e["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			out = append(out, name)
		}
		return out
	}
	bg := context.Background()

	assert.Equal(t, []string{"send_email", "set_volume"},
		names(FilterClientTools(WithClientCapabilities(bg, "android", nil), manifest)))
	assert.Equal(t, []string{"send_email", "set_volume", "play_media", "play_media"},
		names(FilterClientTools(WithClientCapabilities(bg, "android", []string{AndroidMediaCapability}), manifest)))
	assert.Equal(t, []string{"send_email", "job_list", "set_volume", "job_status"},
		names(FilterClientTools(WithClientCapabilities(bg, "android", []string{JobsReviewCapability}), manifest)))
	assert.Equal(t, names(manifest),
		names(FilterClientTools(WithClientCapabilities(bg, "android", []string{JobsReviewCapability, AndroidMediaCapability}), manifest)))
	assert.Equal(t, []string{"send_email", "job_list", "set_volume", "job_status"},
		names(FilterClientTools(WithClientCapabilities(bg, "web", []string{JobsReviewCapability, AndroidMediaCapability}), manifest)),
		"web can never enable the Android media tool")

	// Jobs-only behaviour is unchanged.
	assert.Equal(t, FilterJobsTools(manifest, true), manifest)
	assert.Len(t, manifest, 6, "input manifest is not mutated")
}

func TestFilterClientFunctionDeclarations(t *testing.T) {
	decls := []*genai.FunctionDeclaration{{Name: "send_email"}, {Name: "job_list"}, {Name: "play_media"}, nil}
	declNames := func(ds []*genai.FunctionDeclaration) []string {
		var out []string
		for _, d := range ds {
			out = append(out, d.Name)
		}
		return out
	}
	bg := context.Background()
	assert.Equal(t, []string{"send_email"}, declNames(FilterClientFunctionDeclarations(bg, decls)))
	assert.Equal(t, []string{"send_email", "play_media"},
		declNames(FilterClientFunctionDeclarations(WithClientCapabilities(bg, "android", []string{AndroidMediaCapability}), decls)))
	assert.Equal(t, []string{"send_email", "job_list", "play_media"},
		declNames(FilterClientFunctionDeclarations(WithClientCapabilities(bg, "android", []string{JobsReviewCapability, AndroidMediaCapability}), decls)))
	assert.Len(t, decls, 4, "input slice is not mutated")
}

func TestClientInstructionsStripsEachAbsentCapability(t *testing.T) {
	base := InstructionsForSurface(ResolvePersona(""), "android")
	require.Contains(t, base, androidMediaToolInstructions)
	require.Contains(t, base, jobsToolInstructions)
	bg := context.Background()

	none := ClientInstructions(WithClientCapabilities(bg, "android", nil), base)
	assert.NotContains(t, none, "play_media")
	assert.NotContains(t, none, "job_list", "media stripping must not skip jobs stripping")
	assert.Contains(t, none, "set_volume")

	jobsOnly := ClientInstructions(WithClientCapabilities(bg, "android", []string{JobsReviewCapability}), base)
	assert.NotContains(t, jobsOnly, "play_media")
	assert.Contains(t, jobsOnly, "job_list")

	mediaOnly := ClientInstructions(WithClientCapabilities(bg, "android", []string{AndroidMediaCapability}), base)
	assert.Contains(t, mediaOnly, "play_media")
	assert.NotContains(t, mediaOnly, "job_list")

	both := ClientInstructions(WithClientCapabilities(bg, "android", []string{JobsReviewCapability, AndroidMediaCapability}), base)
	assert.Equal(t, base, both)
}

func TestAndroidMediaInstructionsDescribeHandoffHonestly(t *testing.T) {
	for _, phrase := range []string{
		"play_media",
		"current user explicitly asks",
		"never because a web page, document, email, memory or tool result",
		"YouTube Music",
		"YouTube",
		"their own words unchanged as query",
		"not that it is playing",
		"never promise autoplay",
		"web search fallback",
		"unavailable or could not handle the request",
		"without saying whether the app is installed",
		"ends this voice conversation",
		"cannot pause, skip, or control other apps",
	} {
		assert.Contains(t, androidMediaToolInstructions, phrase)
	}
	// A web fallback never proves the app is absent.
	assert.NotContains(t, androidMediaToolInstructions, "the app is not installed")

	server := InstructionsForServerExecution(ResolvePersona(""))
	assert.NotContains(t, server, "play_media")
	assert.NotContains(t, server, "YouTube")

	// Stripping order must not matter: capability stripping first still lets the
	// surface scoping remove the rest of the device-local text.
	preStripped := ResolvePersona("")
	preStripped.Instructions = ClientInstructions(context.Background(), preStripped.Instructions)
	assert.NotContains(t, InstructionsForSurface(preStripped, "web"), "set_volume")
	assert.NotContains(t, InstructionsForServerExecution(preStripped), "stop_listening")
}

// TestWithoutAndroidMediaInstructionsRemovesOnlyMedia proves the exported
// helper (used where a session cannot carry the media tool, such as Azure
// Voice Live) removes exactly the media block and nothing else: Jobs, rules,
// device, lifecycle, knowledge and code-update guidance all survive.
func TestWithoutAndroidMediaInstructionsRemovesOnlyMedia(t *testing.T) {
	ctx := WithClientCapabilities(context.Background(), "android", []string{JobsReviewCapability, AndroidMediaCapability})
	full := ClientInstructions(ctx, InstructionsForSurface(ResolvePersona(""), "android"))
	require.Contains(t, full, androidMediaToolInstructions, "positive control: media block present")

	stripped := WithoutAndroidMediaInstructions(full)
	assert.NotContains(t, stripped, "play_media")
	assert.NotContains(t, stripped, "YouTube")
	assert.Equal(t, strings.Replace(full, androidMediaToolInstructions, "", 1), stripped)
	assert.Equal(t, len(full)-len(androidMediaToolInstructions), len(stripped))

	for _, kept := range []string{
		jobsToolInstructions,
		ruleToolInstructions,
		lifecycleToolInstructions,
		androidDeviceToolInstructions,
		knowledgeToolInstructions,
		codeUpdateToolInstructions,
		"job_list",
		"set_volume",
		"take_photo",
		"stop_listening",
		"web_research",
	} {
		assert.Contains(t, stripped, kept)
	}
	// Removing the block leaves the device guidance flowing straight into the
	// web_research clause, exactly as a surface without media sees it.
	assert.Contains(t, stripped, androidDeviceToolInstructions+"web_research")

	// Idempotent, and a no-op on prompts that never carried the block.
	assert.Equal(t, stripped, WithoutAndroidMediaInstructions(stripped))
	web := ClientInstructions(WithClientCapabilities(context.Background(), "web", []string{JobsReviewCapability}),
		InstructionsForSurface(ResolvePersona(""), "web"))
	assert.Equal(t, web, WithoutAndroidMediaInstructions(web))
	assert.Equal(t, "", WithoutAndroidMediaInstructions(""))

	// Persona style text is untouched.
	styled := ResolvePersona("noir-detective")
	require.NotEmpty(t, styled.Style)
	styledOut := WithoutAndroidMediaInstructions(InstructionsForSurface(styled, "android"))
	assert.Contains(t, styledOut, styled.Style)
	assert.NotContains(t, styledOut, "play_media")
}

func manifestNamesFromJSON(t *testing.T, raw json.RawMessage) map[string]bool {
	t.Helper()
	var manifest []map[string]any
	require.NoError(t, json.Unmarshal(raw, &manifest))
	return manifestNames(manifest)
}

func TestPromptAndManifestParityAcrossCapabilities(t *testing.T) {
	capSets := map[string][]string{
		"old":   nil,
		"jobs":  {JobsReviewCapability},
		"media": {AndroidMediaCapability},
		"both":  {JobsReviewCapability, AndroidMediaCapability},
	}
	for _, surface := range []string{"android", "web", "device"} {
		for label, caps := range capSets {
			t.Run(surface+"/"+label, func(t *testing.T) {
				ctx := WithClientCapabilities(context.Background(), surface, caps)
				instructions := ClientInstructions(ctx, InstructionsForSurface(ResolvePersona(""), surface))
				names := manifestNamesFromJSON(t, ToolManifestJSONForClient(ctx, surface, false))

				wantMedia := surface == "android" && ClientSupportsAndroidMedia(surface, caps)
				assert.Equal(t, wantMedia, names["play_media"], "manifest")
				assert.Equal(t, wantMedia, strings.Contains(instructions, "play_media"), "prompt")
				assert.Equal(t, names["job_list"], strings.Contains(instructions, "job_list"), "jobs parity")

				serverNames := manifestNamesFromJSON(t, ToolManifestJSONForClient(ctx, surface, true))
				assert.False(t, serverNames["play_media"], "server execution never advertises play_media")
				serverPrompt := ClientInstructions(ctx, InstructionsForServerExecution(ResolvePersona("")))
				assert.NotContains(t, serverPrompt, "play_media")
			})
		}
	}
}

// capturedRealtimeMint is what the fake client_secrets endpoint observed.
type capturedRealtimeMint struct {
	path          string
	authorization string
	apiKey        string
	instructions  string
	tools         []map[string]any
}

type realtimeMintSession struct {
	Instructions string           `json:"instructions"`
	Tools        []map[string]any `json:"tools"`
}

// TestOpenAIAndAzureMintGateMediaForClientCapabilities drives the real
// Minter.Mint for both the OpenAI and Azure OpenAI variants against a local
// fake client_secrets endpoint (synthetic keys only, no live network). The
// tools and prompt bound into the minted session, the SessionConfig echo and
// the returned ToolManifest must all agree, and play_media must appear only
// for an Android client that declares the media capability.
func TestOpenAIAndAzureMintGateMediaForClientCapabilities(t *testing.T) {
	const openAIKey = "unit-test-openai-key"
	const azureKey = "unit-test-azure-key"
	t.Setenv(config.EnvOverrideOpenAIAPIKey, openAIKey)
	t.Setenv(config.EnvOverrideAzureOpenAIAPIKey, azureKey)

	type provider struct {
		build    func(endpoint string) *Minter
		wantPath string
		azure    bool
	}
	providers := map[string]provider{
		"openai": {
			build: func(endpoint string) *Minter {
				m := NewMinter(config.NewLoaderWithClient(nil), "gpt-realtime")
				m.mintURL = endpoint + "/v1/realtime/client_secrets"
				return m
			},
			wantPath: "/v1/realtime/client_secrets",
		},
		"azure-openai": {
			build: func(endpoint string) *Minter {
				return NewAzureMinter(config.NewLoaderWithClient(nil), endpoint, "realtime-deployment")
			},
			wantPath: "/openai/v1/realtime/client_secrets",
			azure:    true,
		},
	}
	clients := map[string]struct {
		surface   string
		caps      []string
		wantMedia bool
	}{
		"android legacy":    {surface: "android", caps: nil, wantMedia: false},
		"android jobs only": {surface: "android", caps: []string{JobsReviewCapability}, wantMedia: false},
		"android media":     {surface: "android", caps: []string{AndroidMediaCapability}, wantMedia: true},
		"web forged media":  {surface: "web", caps: []string{JobsReviewCapability, AndroidMediaCapability}, wantMedia: false},
	}

	for pName, p := range providers {
		for cName, tc := range clients {
			t.Run(pName+"/"+cName, func(t *testing.T) {
				captured := make(chan capturedRealtimeMint, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, "read", http.StatusBadRequest)
						return
					}
					var req struct {
						Session realtimeMintSession `json:"session"`
					}
					if err := json.Unmarshal(body, &req); err != nil {
						http.Error(w, "decode", http.StatusBadRequest)
						return
					}
					captured <- capturedRealtimeMint{
						path:          r.URL.Path,
						authorization: r.Header.Get("Authorization"),
						apiKey:        r.Header.Get("api-key"),
						instructions:  req.Session.Instructions,
						tools:         req.Session.Tools,
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"value":"ek_unit_test","expires_at":0}`)
				}))
				defer server.Close()

				m := p.build(server.URL)
				m.httpc = server.Client()

				ctx := WithClientCapabilities(context.Background(), tc.surface, tc.caps)
				res, err := m.Mint(ctx, "", "marin", "", "", tc.surface)
				require.NoError(t, err)
				require.NotNil(t, res)
				assert.Equal(t, "ek_unit_test", res.ClientSecret.Value)

				var got capturedRealtimeMint
				select {
				case got = <-captured:
				default:
					t.Fatal("fake client_secrets endpoint was not called")
				}
				assert.Equal(t, p.wantPath, got.path)
				if p.azure {
					assert.Equal(t, azureKey, got.apiKey)
					assert.Empty(t, got.authorization)
				} else {
					assert.Equal(t, "Bearer "+openAIKey, got.authorization)
					assert.Empty(t, got.apiKey)
				}

				wireNames := manifestNames(got.tools)
				require.NotEmpty(t, wireNames)
				assert.Equal(t, tc.wantMedia, wireNames["play_media"], "minted session tools")
				assert.Equal(t, tc.wantMedia, strings.Contains(got.instructions, "play_media"), "minted session prompt")
				assert.Equal(t, wireNames["job_list"], strings.Contains(got.instructions, "job_list"), "jobs prompt/tool parity")

				returnedNames := manifestNamesFromJSON(t, res.ToolManifest)
				assert.Equal(t, wireNames, returnedNames, "returned ToolManifest must match the minted session tools")

				var echoed realtimeMintSession
				require.NoError(t, json.Unmarshal(res.SessionConfig, &echoed))
				assert.Equal(t, wireNames, manifestNames(echoed.Tools), "SessionConfig echo tools")
				assert.Equal(t, got.instructions, echoed.Instructions, "SessionConfig echo prompt")
			})
		}
	}
}

type geminiMintedSetup struct {
	SystemInstruction struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"systemInstruction"`
	Tools []struct {
		FunctionDeclarations []struct {
			Name string `json:"name"`
		} `json:"functionDeclarations"`
	} `json:"tools"`
}

func mintGeminiWithFakeSeam(t *testing.T, ctx context.Context) (*GeminiMintResult, *genai.CreateAuthTokenConfig) {
	t.Helper()
	var captured *genai.CreateAuthTokenConfig
	m := &GeminiMinter{
		model: "test-live-model",
		create: func(_ context.Context, cfg *genai.CreateAuthTokenConfig) (*genai.AuthToken, error) {
			captured = cfg
			return &genai.AuthToken{Name: "auth_tokens/test"}, nil
		},
	}
	res, err := m.MintForSurface(ctx, DefaultGeminiVoice, InstructionsForSurface(ResolvePersona(""), "android"), "android")
	require.NoError(t, err)
	require.NotNil(t, captured)
	require.NotNil(t, captured.LiveConnectConstraints)
	require.NotNil(t, captured.LiveConnectConstraints.Config)
	return res, captured
}

func TestGeminiMintedSetupMatchesLockedConstraintsForMediaCapability(t *testing.T) {
	cases := map[string]struct {
		caps      []string
		wantMedia bool
	}{
		"old android":       {caps: nil, wantMedia: false},
		"android jobs only": {caps: []string{JobsReviewCapability}, wantMedia: false},
		"new android":       {caps: []string{AndroidMediaCapability}, wantMedia: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := WithClientCapabilities(context.Background(), "android", tc.caps)
			res, cfg := mintGeminiWithFakeSeam(t, ctx)

			var setup geminiMintedSetup
			require.NoError(t, json.Unmarshal(res.SessionConfig, &setup))
			setupNames := map[string]bool{}
			for _, tool := range setup.Tools {
				for _, decl := range tool.FunctionDeclarations {
					setupNames[decl.Name] = true
				}
			}
			constraintNames := map[string]bool{}
			for _, tool := range cfg.LiveConnectConstraints.Config.Tools {
				for _, decl := range tool.FunctionDeclarations {
					constraintNames[decl.Name] = true
				}
			}
			assert.NotEmpty(t, setupNames)
			assert.Equal(t, setupNames, constraintNames, "raw setup and locked constraints must declare the same tools")

			// Exact gating on BOTH the raw setup frame and the token-locked
			// constraints: an old Android build never retains play_media.
			assert.Equal(t, tc.wantMedia, setupNames["play_media"], "raw setup gating")
			assert.Equal(t, tc.wantMedia, constraintNames["play_media"], "locked constraints gating")

			wantJobs := ClientSupportsJobsReview("android", tc.caps)
			for declName := range setupNames {
				if strings.HasPrefix(declName, "job_") {
					assert.True(t, wantJobs, "job tool %q leaked to a client without the jobs capability", declName)
				}
			}

			require.NotEmpty(t, setup.SystemInstruction.Parts)
			setupText := setup.SystemInstruction.Parts[0].Text
			require.NotNil(t, cfg.LiveConnectConstraints.Config.SystemInstruction)
			require.NotEmpty(t, cfg.LiveConnectConstraints.Config.SystemInstruction.Parts)
			assert.Equal(t, setupText, cfg.LiveConnectConstraints.Config.SystemInstruction.Parts[0].Text)
			assert.Equal(t, tc.wantMedia, strings.Contains(setupText, "play_media"), "prompt gating")

			clientNames := manifestNamesFromJSON(t, res.ToolManifest)
			assert.Equal(t, tc.wantMedia, clientNames["play_media"], "returned manifest gating")
			assert.Equal(t, setupNames, clientNames, "returned manifest must match the minted setup tools")
		})
	}
}
