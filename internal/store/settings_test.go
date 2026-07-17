package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetSettingsDefaultsWhenAbsent(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	doc, err := st.GetSettings(ctx, "uid-1")
	require.NoError(t, err)
	assert.EqualValues(t, 1, doc["version"])
	assert.Equal(t, "cedar", doc["voice"])
	assert.Equal(t, "hey-live-ninja", doc["wakeWord"])
	assert.Equal(t, "semantic_vad", doc["turnDetection"])
	privacy, ok := doc["privacy"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, privacy["storeAudio"])
}

func TestPutSettingsOptimisticConcurrency(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// First write against the synthesized default (version 1, no item).
	doc := DefaultSettings()
	doc["voice"] = "marin"
	v2, err := st.PutSettings(ctx, "uid-1", doc, 1)
	require.NoError(t, err)
	assert.EqualValues(t, 2, v2)

	// Read-back: stored fields + bumped version, no pk/sk leakage.
	got, err := st.GetSettings(ctx, "uid-1")
	require.NoError(t, err)
	assert.Equal(t, "marin", got["voice"])
	assert.EqualValues(t, 2, got["version"])
	_, hasPK := got["pk"]
	assert.False(t, hasPK, "table plumbing keys must not leak into the document")

	// A racing writer that also read version 1 must conflict.
	stale := DefaultSettings()
	stale["voice"] = "verse"
	_, err = st.PutSettings(ctx, "uid-1", stale, 1)
	require.ErrorIs(t, err, ErrVersionConflict)

	// The winning write is untouched by the losing attempt.
	got, err = st.GetSettings(ctx, "uid-1")
	require.NoError(t, err)
	assert.Equal(t, "marin", got["voice"])

	// The reader who re-fetched (version 2) succeeds.
	stale["voice"] = "verse"
	v3, err := st.PutSettings(ctx, "uid-1", stale, 2)
	require.NoError(t, err)
	assert.EqualValues(t, 3, v3)
}

func TestSettingsUnknownFieldPreservation(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// A future/foreign surface writes a field this build doesn't know.
	doc := DefaultSettings()
	doc["futureFeature"] = map[string]any{"enabled": true}
	_, err := st.PutSettings(ctx, "uid-1", doc, 1)
	require.NoError(t, err)

	got, err := st.GetSettings(ctx, "uid-1")
	require.NoError(t, err)
	ff, ok := got["futureFeature"].(map[string]any)
	require.True(t, ok, "unknown field must survive the round trip")
	assert.Equal(t, true, ff["enabled"])
}

func TestGetSettingsFillsMissingSubDefaults(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// Simulate an old document written before privacy/voiceEngine existed.
	doc := map[string]any{
		"wakeWord":      "ninja",
		"wakeEngine":    "openwakeword",
		"sensitivity":   0.7,
		"persona":       map[string]any{"presetId": "default"},
		"voice":         "ash",
		"turnDetection": "server_vad",
		"theme":         "dark",
		"micDeviceId":   nil,
	}
	_, err := st.PutSettings(ctx, "uid-1", doc, 1)
	require.NoError(t, err)

	got, err := st.GetSettings(ctx, "uid-1")
	require.NoError(t, err)
	// Stored values intact…
	assert.Equal(t, "ash", got["voice"])
	assert.Equal(t, "ninja", got["wakeWord"])
	// …missing objects filled from defaults on read.
	ve, ok := got["voiceEngine"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "openai-realtime", ve["default"])
	privacy, ok := got["privacy"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 30, privacy["retentionDays"])
	// …and missing required sub-keys fill without clobbering present ones.
	persona, ok := got["persona"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "default", persona["presetId"])
	assert.Nil(t, persona["systemInstructions"])
}
