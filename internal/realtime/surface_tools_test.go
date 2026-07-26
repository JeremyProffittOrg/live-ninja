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

func TestToolManifestAndInstructionsAreScopedToSurface(t *testing.T) {
	tests := []struct {
		surface   string
		lifecycle bool
		android   bool
	}{
		{surface: "web", lifecycle: true},
		{surface: "android", lifecycle: true, android: true},
		{surface: "device"},
	}
	for _, tc := range tests {
		t.Run(tc.surface, func(t *testing.T) {
			names := manifestNames(toolManifestForSurface(tc.surface))
			assert.True(t, names["send_email"], "server tools stay available")
			assert.Equal(t, tc.lifecycle, names["stop_listening"])
			assert.Equal(t, tc.lifecycle, names["start_new_conversation"])
			for _, name := range []string{"set_volume", "take_photo", "record_video"} {
				assert.Equal(t, tc.android, names[name], name)
			}

			instructions := InstructionsForSurface(ResolvePersona(""), tc.surface)
			assert.Contains(t, instructions, "send_email")
			assert.Equal(t, tc.lifecycle, containsToolName(instructions, "stop_listening"))
			for _, name := range []string{"set_volume", "take_photo", "record_video"} {
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
	} {
		assert.False(t, names[name], name)
	}
}

func TestNovaSessionConfigUsesOnlyServerExecutableToolsAndStableDigest(t *testing.T) {
	config := BuildNovaSessionConfig("trusted system prompt")
	assert.Equal(t, "trusted system prompt", config.SystemPrompt)

	wantNames := manifestNames(toolManifestForServerExecution())
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
