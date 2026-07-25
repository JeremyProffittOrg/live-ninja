package ninja.jeremy.liveninja.realtime

import org.webrtc.MediaConstraints
import org.webrtc.audio.AudioProcessingComponentOptions
import org.webrtc.audio.AudioProcessingMode
import org.webrtc.audio.AudioProcessingOptions

/**
 * Capture-side audio-processing policy for [WebRtcTransport] (WS-5 M21.2).
 *
 * **The defect this exists for.** The transport used to build its audio device
 * module with `setUseHardwareAcousticEchoCanceler(true)`. That flag is not
 * additive: the Java ADM then answers `BuiltInAECIsAvailable()` = true, and
 * libwebrtc's voice engine responds by switching *its own* AEC3 off — platform
 * echo cancellation is treated as exclusive. On the Tab S9 FE the platform
 * canceller does not cancel loudspeaker output at all, so the assistant's own
 * speech came back up the mic and was transcribed as user input at every media
 * volume (3/15 and 15/15). Net effect: the device had a nominal canceller and no
 * real one. Turning the platform effects off is what lets AEC3 run, with the
 * render reference libwebrtc itself controls rather than one the HAL may never
 * have wired for a VoIP playout path.
 *
 * **Why the noise suppressor goes with it.** 144's ADM couples the two
 * components ("Platform echo cancellation cannot be combined with software
 * noise suppression" / "…with disabled noise suppression"), so a mixed
 * platform-EC + software-NS request is rejected outright. Beyond the API, a
 * platform NS sitting *ahead* of AEC3 is exactly the nonlinear pre-processing
 * that ruins AEC3's echo estimate. Both platform effects off with the full
 * software chain on is the only self-consistent combination.
 *
 * **Why this is data with pure mappers instead of literals at the call site.**
 * Building the real ADM needs a Context and the native factory, so a JVM test
 * can never assert the built module — only the decision. Keeping the policy
 * here makes "the hardware canceller stays off" a unit-tested property rather
 * than a comment someone can quietly revert.
 */
data class VoiceAudioProcessing(
    /** Platform (HAL) `AcousticEchoCanceler` effect on the capture session. */
    val useHardwareAec: Boolean,
    /** Platform (HAL) `NoiseSuppressor` effect on the capture session. */
    val useHardwareNs: Boolean,
    val echoCancellation: Boolean,
    val noiseSuppression: Boolean,
    val autoGainControl: Boolean,
    val highpassFilter: Boolean,
) {

    /**
     * `goog*` capture constraints for `createAudioSource`. Still parsed by
     * libwebrtc 144 (`CopyConstraintsIntoAudioOptions`) and still the only way
     * to configure the audio *source*, so they stay alongside — not replaced by
     * — [toAudioProcessingOptions], which addresses the track.
     */
    fun toAudioSourceConstraints(): MediaConstraints = MediaConstraints().apply {
        mandatory.add(MediaConstraints.KeyValuePair(KEY_ECHO_CANCELLATION, echoCancellation.toString()))
        mandatory.add(MediaConstraints.KeyValuePair(KEY_NOISE_SUPPRESSION, noiseSuppression.toString()))
        mandatory.add(MediaConstraints.KeyValuePair(KEY_AUTO_GAIN_CONTROL, autoGainControl.toString()))
        mandatory.add(MediaConstraints.KeyValuePair(KEY_HIGHPASS_FILTER, highpassFilter.toString()))
    }

    /**
     * 144's per-track request (`AudioTrack.setAudioProcessingOptions`) — the
     * successor to the goog* constraints and the only API that can say
     * "echo cancellation, in SOFTWARE" out loud instead of inferring it from
     * whether the platform effect happens to be available.
     *
     * AGC and the high-pass filter stay [AudioProcessingMode.AUTOMATIC]: the ADM
     * exposes no platform toggle for them, so there is nothing to disambiguate,
     * and only EC/NS carry the platform-vs-software combination rules that
     * reject a request.
     */
    fun toAudioProcessingOptions(): AudioProcessingOptions = AudioProcessingOptions(
        AudioProcessingComponentOptions(echoCancellation, modeFor(useHardwareAec)),
        AudioProcessingComponentOptions(noiseSuppression, modeFor(useHardwareNs)),
        AudioProcessingComponentOptions(autoGainControl, AudioProcessingMode.AUTOMATIC),
        AudioProcessingComponentOptions(highpassFilter, AudioProcessingMode.AUTOMATIC),
    )

    /**
     * SOFTWARE is explicit (not AUTOMATIC) whenever we disabled the platform
     * effect: AUTOMATIC lets the resolver reach for the platform component if
     * the device offers it, which is the behaviour M21.2 is fixing.
     */
    private fun modeFor(useHardware: Boolean): AudioProcessingMode =
        if (useHardware) AudioProcessingMode.AUTOMATIC else AudioProcessingMode.SOFTWARE

    companion object {
        const val KEY_ECHO_CANCELLATION = "googEchoCancellation"
        const val KEY_NOISE_SUPPRESSION = "googNoiseSuppression"
        const val KEY_AUTO_GAIN_CONTROL = "googAutoGainControl"
        const val KEY_HIGHPASS_FILTER = "googHighpassFilter"

        /**
         * The M21.2 configuration: platform AEC/NS off, full software APM on.
         * Do not flip [useHardwareAec] back on without on-device evidence that
         * the platform canceller works — the self-echo loop is what it caused.
         */
        val SOFTWARE_APM = VoiceAudioProcessing(
            useHardwareAec = false,
            useHardwareNs = false,
            echoCancellation = true,
            noiseSuppression = true,
            autoGainControl = true,
            highpassFilter = true,
        )
    }
}
