package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCameraDefinitionsAdvertiseSafeDefaultsAndLimits(t *testing.T) {
	manifest := renderManifest([]*Definition{
		takePhotoDefinition(),
		recordVideoDefinition(),
	})
	require.Len(t, manifest, 2)

	photoParams := manifest[0]["parameters"].(map[string]any)
	photoProps := photoParams["properties"].(map[string]any)
	assert.Empty(t, photoParams["required"])
	assert.Equal(t, deviceCameraLenses, photoProps["camera"].(map[string]any)["enum"])
	assert.Contains(t, photoProps["camera"].(map[string]any)["description"], "back is the default")

	videoParams := manifest[1]["parameters"].(map[string]any)
	videoProps := videoParams["properties"].(map[string]any)
	assert.Empty(t, videoParams["required"])
	duration := videoProps["durationSeconds"].(map[string]any)
	assert.Equal(t, float64(1), duration["minimum"])
	assert.Equal(t, float64(maxVideoDurationSeconds), duration["maximum"])
	assert.Contains(t, duration["description"], "default to 60 seconds")
}

func TestCameraDefinitionsValidateBeforeDeviceLocalFallback(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())

	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"unknown photo lens", "take_photo", map[string]any{"camera": "external"}},
		{"unknown video lens", "record_video", map[string]any{"camera": "wide"}},
		{"zero duration", "record_video", map[string]any{"durationSeconds": float64(0)}},
		{"over maximum", "record_video", map[string]any{"durationSeconds": float64(301)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := r.Invoke(context.Background(), invocation(tc.tool, tc.args))
			require.False(t, res.OK)
			require.NotNil(t, res.Error)
			assert.Equal(t, CodeInvalidArgs, res.Error.Code)
		})
	}

	for _, tool := range []string{"take_photo", "record_video"} {
		res := r.Invoke(context.Background(), invocation(tool, map[string]any{}))
		require.False(t, res.OK)
		require.NotNil(t, res.Error)
		assert.Equal(t, CodeNotConfigured, res.Error.Code)
		assert.Contains(t, res.Error.Message, tool+" runs on the user's device")
	}
}
