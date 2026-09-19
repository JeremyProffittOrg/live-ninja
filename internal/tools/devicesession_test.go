package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStopListeningStaysArmedForWakeWord(t *testing.T) {
	def := stopListeningDefinition()
	assert.True(t, def.DeviceLocal)
	assert.Equal(t, []string{"web", "android", "device"}, def.Surfaces)
	assert.Contains(t, def.Description, "wake word")
	assert.NotContains(t, def.Description, "Stop always-on")
	assert.True(t, supportsSurface(def, "web"))
	assert.True(t, supportsSurface(def, "android"))
	assert.True(t, supportsSurface(def, "device"))
	assert.False(t, supportsSurface(def, "unknown"))
}

func TestCatalogManifestForDeviceIncludesStopListening(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range CatalogManifestForSurface("device") {
		name, _ := tool["name"].(string)
		names[name] = true
	}
	require.True(t, names["stop_listening"])
	assert.True(t, names["send_email"], "server tools stay available on device")
	assert.False(t, names["set_volume"], "android-only tools stay off the Tab5")
}
