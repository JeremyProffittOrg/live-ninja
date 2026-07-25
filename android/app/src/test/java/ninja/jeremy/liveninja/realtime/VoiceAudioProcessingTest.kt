package ninja.jeremy.liveninja.realtime

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import org.webrtc.audio.AudioProcessingMode

/**
 * WS-5 M21.2: the audio-processing *decision* is what a JVM test can hold onto —
 * building the real audio device module needs a Context and the native factory.
 * These lock in the configuration whose absence produced the self-echo loop: the
 * platform (HAL) canceller stays off so libwebrtc's AEC3 is the one running, and
 * every software component is requested on both surfaces that carry it.
 */
class VoiceAudioProcessingTest {

    private val config = VoiceAudioProcessing.SOFTWARE_APM

    @Test
    fun `platform effects stay off and the software chain stays on`() {
        // Flipping either of these back on hands echo cancellation to a platform
        // canceller that cancelled nothing on the Tab S9 FE — and, worse, makes
        // libwebrtc switch AEC3 off in its favour.
        assertFalse("hardware AEC must stay disabled", config.useHardwareAec)
        assertFalse("hardware NS must stay disabled", config.useHardwareNs)

        assertTrue(config.echoCancellation)
        assertTrue(config.noiseSuppression)
        assertTrue(config.autoGainControl)
        assertTrue(config.highpassFilter)
    }

    @Test
    fun `source constraints request the full goog software chain as mandatory`() {
        val pairs = config.toAudioSourceConstraints().mandatory.associate { it.key to it.value }

        assertEquals(
            mapOf(
                VoiceAudioProcessing.KEY_ECHO_CANCELLATION to "true",
                VoiceAudioProcessing.KEY_NOISE_SUPPRESSION to "true",
                VoiceAudioProcessing.KEY_AUTO_GAIN_CONTROL to "true",
                VoiceAudioProcessing.KEY_HIGHPASS_FILTER to "true",
            ),
            pairs,
        )
        assertTrue(config.toAudioSourceConstraints().optional.isEmpty())
    }

    @Test
    fun `track options pin echo cancellation and noise suppression to SOFTWARE`() {
        val options = config.toAudioProcessingOptions()

        assertTrue(options.echoCancellation)
        assertTrue(options.noiseSuppression)
        // Explicit SOFTWARE, not AUTOMATIC: AUTOMATIC lets the resolver reach for
        // the platform component whenever the device advertises one.
        assertEquals(AudioProcessingMode.SOFTWARE, options.echoCancellationMode)
        assertEquals(AudioProcessingMode.SOFTWARE, options.noiseSuppressionMode)
        // AGC/HPF have no platform toggle on the ADM, so there is nothing to pin.
        assertEquals(AudioProcessingMode.AUTOMATIC, options.autoGainControlMode)
        assertEquals(AudioProcessingMode.AUTOMATIC, options.highPassFilterMode)
    }

    @Test
    fun `a platform-canceller configuration maps to AUTOMATIC modes`() {
        // Keeps the mapping honest in both directions: the mode is derived from the
        // ADM flags, not hard-coded, so a future device-specific policy that trusts
        // the platform canceller does not silently keep asking for SOFTWARE.
        val platform = config.copy(useHardwareAec = true, useHardwareNs = true)
        val options = platform.toAudioProcessingOptions()

        assertEquals(AudioProcessingMode.AUTOMATIC, options.echoCancellationMode)
        assertEquals(AudioProcessingMode.AUTOMATIC, options.noiseSuppressionMode)
    }
}
