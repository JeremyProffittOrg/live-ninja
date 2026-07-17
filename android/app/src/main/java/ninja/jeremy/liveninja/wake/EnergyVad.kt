package ninja.jeremy.liveninja.wake

import kotlin.math.sqrt

/**
 * Cheap energy voice-activity gate run on every 80 ms chunk BEFORE any ONNX inference
 * (plan.md M4 battery strategy: "energy-VAD gate before ONNX inference").
 *
 * RMS of the int16 chunk is compared against [thresholdRms]; once speech-level energy is seen
 * the gate stays open for [hangoverMs] past the last energetic chunk so the tail of a phrase
 * (and the oWW head's stride window) still gets inference. While closed, the engine skips the
 * melspectrogram/embedding/head runs entirely — the dominant battery win on an idle room.
 *
 * Pure arithmetic, injectable clock — unit-testable.
 *
 * @param thresholdRms RMS amplitude (int16 scale, 0..32767) above which a chunk counts as
 *        voice-ish energy. ~200 is comfortably above electret self-noise and quiet-room HVAC
 *        while far below any spoken wake phrase at conversational distance.
 * @param hangoverMs how long the gate stays open after the last energetic chunk.
 */
class EnergyVad(
    private val thresholdRms: Double = 200.0,
    private val hangoverMs: Long = 1_500,
) {
    private var lastVoiceAtMs = Long.MIN_VALUE
    private var open = false

    /** True when this call transitioned the gate closed → open (caller should reset + pre-roll). */
    var gateJustOpened = false
        private set

    val isOpen: Boolean get() = open

    /**
     * Feed one chunk; returns true when downstream inference should run for this chunk.
     */
    fun accept(chunk: ShortArray, nowMs: Long): Boolean {
        gateJustOpened = false
        var sumSq = 0.0
        for (s in chunk) {
            val v = s.toDouble()
            sumSq += v * v
        }
        val rms = sqrt(sumSq / chunk.size)
        if (rms >= thresholdRms) lastVoiceAtMs = nowMs

        val shouldBeOpen = lastVoiceAtMs != Long.MIN_VALUE && (nowMs - lastVoiceAtMs) <= hangoverMs
        if (shouldBeOpen && !open) gateJustOpened = true
        open = shouldBeOpen
        return open
    }

    fun reset() {
        lastVoiceAtMs = Long.MIN_VALUE
        open = false
        gateJustOpened = false
    }
}
