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

func TestTTSInstructionsMatchesLiveAccentDirectives(t *testing.T) {
	assert.Equal(t, "", TTSInstructions(""))
	assert.Equal(t, "", TTSInstructions("none"))
	assert.Equal(t, "", TTSInstructions("not-a-real-accent"))
	assert.Equal(t, "Speak with a natural British accent.", TTSInstructions("british"))
	assert.Equal(t, "Speak with a light, natural Irish accent.", TTSInstructions("irish"))
	assert.Equal(t, "\n\n"+TTSInstructions("british"), AccentDirective("british"))
	assert.Equal(t, "", AccentDirective(""))
}
