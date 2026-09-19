package realtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSpeechVoiceMapsRealtimeOnlyVoices(t *testing.T) {
	assert.Equal(t, DefaultTTSVoice, SpeechVoice(""))
	assert.Equal(t, "ash", SpeechVoice("cedar"))
	assert.Equal(t, "ash", SpeechVoice("Cedar"))
	assert.Equal(t, "coral", SpeechVoice("marin"))
	assert.Equal(t, "sage", SpeechVoice("sage"))
	assert.Equal(t, "alloy", SpeechVoice("alloy"))
}
