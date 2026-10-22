package realtime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

func manifestNames(manifest []map[string]any) map[string]bool {
	out := make(map[string]bool, len(manifest))
	for _, tool := range manifest {
		name, _ := tool["name"].(string)
		out[name] = true
	}
	return out
}

// Owner decision 2026-10-22: owner calls share all server tools and their
// operational guidance. Device-local controls still need a connected app.
func TestPhoneManifestIsTheOwnerCallToolSet(t *testing.T) {
	names := manifestNames(PhoneToolManifest())
	require.Equal(t, manifestNames(toolManifestForServerExecution()), names)
	for name := range names {
		require.True(t, PhoneToolAllowed(name), name)
	}
	for _, tool := range []string{"phone_message_send", "set_volume", "take_photo", "record_video",
		"play_media", "stop_listening", "start_new_conversation", " memory_search", "MEMORY_SEARCH", "future_read"} {
		require.False(t, PhoneToolAllowed(tool), tool)
		require.False(t, names[tool], tool)
	}
	prompt := PhoneInstructions(ResolvePersona(""))
	for _, want := range []string{"confirm=true", "read the exact change back", "same server tools as the other voice paths",
		"web_lookup", "web_research", "not the live web", "Web research searches the public web", "omit a recency limit for historical events",
		"file_read", "deliverable_create", "code_update_start", "call_message", "device_control",
		"ghost_nodes", "ghost_session_command",
		"read the proposal back", "confirm=true and that proposalId", "not enabled on the fleet yet"} {
		require.Contains(t, prompt, want)
	}
	require.NotContains(t, prompt, "kill", "KILL is not offered on any surface")
	require.NotContains(t, prompt, "A spoken yes or model-supplied confirm cannot approve it")
	require.NotContains(t, prompt, "Model-supplied confirm=true cannot approve the proposal")
	for _, name := range []string{"set_volume", "take_photo", "record_video", "play_media", "stop_listening", "start_new_conversation"} {
		require.NotContains(t, prompt, name)
	}
	require.False(t, ClientSupportsJobsReview("phone", []string{JobsReviewCapability}))
}

func TestToolManifestAndInstructionsAreScopedToSurface(t *testing.T) {
	tests := []struct {
		surface  string
		stop     bool
		startNew bool
		android  bool
	}{
		{surface: "web", stop: true, startNew: true},
		{surface: "android", stop: true, startNew: true, android: true},
		{surface: "device", stop: true},
	}
	for _, tc := range tests {
		t.Run(tc.surface, func(t *testing.T) {
			names := manifestNames(toolManifestForSurface(tc.surface))
			assert.True(t, names["send_email"], "server tools stay available")
			assert.Equal(t, tc.stop, names["stop_listening"])
			assert.Equal(t, tc.startNew, names["start_new_conversation"])
			for _, name := range []string{"set_volume", "take_photo", "record_video", "play_media"} {
				assert.Equal(t, tc.android, names[name], name)
			}

			instructions := InstructionsForSurface(ResolvePersona(""), tc.surface)
			assert.Contains(t, instructions, "send_email")
			assert.Equal(t, tc.stop, containsToolName(instructions, "stop_listening"))
			for _, name := range []string{"set_volume", "take_photo", "record_video", "play_media"} {
				assert.Equal(t, tc.android, containsToolName(instructions, name), name)
			}

			declNames := make(map[string]bool)
			for _, decl := range geminiToolDeclarationsForSurface(tc.surface) {
				name, _ := decl["name"].(string)
				declNames[name] = true
			}
			assert.Equal(t, names, declNames, "Gemini declarations must match the response manifest")
		})
	}
}

func TestFallbackToolRequestUsesServerExecutionManifest(t *testing.T) {
	body, err := buildToolTurnRequestForSurface(
		"", "android", []ChatMessage{{Role: "user", Content: "help"}}, "",
	)
	require.NoError(t, err)
	var request struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(body, &request))
	require.NotEmpty(t, request.Messages)
	assert.Contains(t, request.Messages[0].Content, "send_email")
	assert.NotContains(t, request.Messages[0].Content, "stop_listening")
	assert.NotContains(t, request.Messages[0].Content, "set_volume")
	assert.NotContains(t, request.Messages[0].Content, "take_photo")
	assert.NotContains(t, request.Messages[0].Content, "play_media")

	names := map[string]bool{}
	for _, tool := range request.Tools {
		names[tool.Function.Name] = true
	}
	assert.True(t, names["send_email"])
	for _, name := range []string{
		"stop_listening",
		"start_new_conversation",
		"set_volume",
		"take_photo",
		"record_video",
		"play_media",
	} {
		assert.False(t, names[name], name)
	}
}

func TestNovaSessionConfigUsesOnlyServerExecutableToolsAndStableDigest(t *testing.T) {
	config := BuildNovaSessionConfig("trusted system prompt")
	assert.Equal(t, "trusted system prompt", config.SystemPrompt)

	wantNames := manifestNames(FilterJobsTools(toolManifestForServerExecution(), false))
	gotNames := make(map[string]bool, len(config.Tools))
	for _, spec := range config.Tools {
		gotNames[spec.Name] = true
		assert.NotEmpty(t, spec.Description)
		assert.True(t, json.Valid(spec.InputSchema), spec.Name)
	}
	assert.Equal(t, wantNames, gotNames)
	for _, local := range []string{
		"stop_listening",
		"start_new_conversation",
		"set_volume",
		"take_photo",
		"record_video",
		"play_media",
	} {
		assert.False(t, gotNames[local], local)
	}

	digest := NovaConfigDigest(config)
	assert.NotEmpty(t, digest)
	assert.Equal(t, digest, NovaConfigDigest(config), "same semantic config must hash identically")
	config.SystemPrompt = "changed"
	assert.NotEqual(t, digest, NovaConfigDigest(config), "prompt changes must invalidate the signed digest")

	orderedA := voiceengine.Config{Tools: []voiceengine.ToolSpec{{
		Name: "example", InputSchema: json.RawMessage(
			`{"type":"object","properties":{"text":{"type":"string"},"count":{"type":"integer"}}}`,
		),
	}}}
	orderedB := voiceengine.Config{Tools: []voiceengine.ToolSpec{{
		Name: "example", InputSchema: json.RawMessage(
			`{"properties":{"count":{"type":"integer"},"text":{"type":"string"}},"type":"object"}`,
		),
	}}}
	assert.Equal(t, NovaConfigDigest(orderedA), NovaConfigDigest(orderedB),
		"JSON object ordering introduced by a client must not invalidate the config")
}

func containsToolName(text, name string) bool {
	return strings.Contains(text, name)
}
