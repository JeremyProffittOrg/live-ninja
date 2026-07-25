package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetVolumeDefinitionAdvertisesEveryAddressableStream(t *testing.T) {
	def := setVolumeDefinition()
	assert.True(t, def.DeviceLocal)
	require.NotNil(t, def.Handler)

	manifest := renderManifest([]*Definition{def})
	require.Len(t, manifest, 1)
	params := manifest[0]["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)

	assert.Equal(t, []string{"action"}, params["required"])
	assert.Equal(t, deviceVolumeActions, props["action"].(map[string]any)["enum"])
	assert.Equal(t, deviceVolumeStreams, props["stream"].(map[string]any)["enum"])
	assert.Equal(t, float64(0), props["level"].(map[string]any)["minimum"])
	assert.Equal(t, float64(100), props["level"].(map[string]any)["maximum"])
	assert.NotContains(t, params["required"], "stream", "omitted stream must default to media")
	assert.Contains(t, props["stream"].(map[string]any)["description"], "media is the default")
}

func TestSetVolumeDefinitionValidatesBeforeDeviceLocalFallback(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"missing action", map[string]any{}},
		{"unknown action", map[string]any{"action": "maximum"}},
		{"unknown stream", map[string]any{"action": "mute", "stream": "bluetooth"}},
		{"level below range", map[string]any{"action": "set", "level": float64(-1)}},
		{"level above range", map[string]any{"action": "set", "level": float64(101)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := r.Invoke(context.Background(), invocation("set_volume", tc.args))
			require.False(t, res.OK)
			require.NotNil(t, res.Error)
			assert.Equal(t, CodeInvalidArgs, res.Error.Code)
		})
	}

	// A schema-valid call reaching the backend is a surface mismatch, not a
	// fabricated success: Android is expected to intercept it locally.
	res := r.Invoke(context.Background(), invocation("set_volume", map[string]any{
		"action": "set",
		"level":  float64(55),
	}))
	require.False(t, res.OK)
	require.NotNil(t, res.Error)
	assert.Equal(t, CodeNotConfigured, res.Error.Code)
	assert.Contains(t, res.Error.Message, "set_volume runs on the user's device")
}
