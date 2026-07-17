package ninja.jeremy.liveninja.audio

import kotlinx.coroutines.flow.Flow

/**
 * A single wake-word hit emitted by an engine.
 *
 * @param phrase the wake phrase that matched (e.g. "hey live ninja")
 * @param score engine confidence in [0, 1]
 * @param timestampMillis SystemClock.elapsedRealtime() at detection
 */
data class WakeWordDetection(
    val phrase: String,
    val score: Float,
    val timestampMillis: Long,
)

/**
 * Contract every wake-word engine implements (plan.md M4, Android §3.1).
 *
 * Default implementation: openWakeWord-style ONNX models via onnxruntime-android,
 * model fetched from the backend wake-word manifest (contracts/wakeword-manifest.md)
 * with SHA-256 verification. Optional implementation: Porcupine, compiled out by
 * default (requires a Picovoice key). Both sit behind this interface so the
 * WakeWordService and Settings engine-picker are engine-agnostic.
 *
 * Engines consume 16 kHz mono PCM from AudioRecord behind an energy-VAD pre-gate;
 * [start]/[stop] own that capture lifecycle.
 */
interface WakeWordEngine {
    /** Hot flow of detections while started; emits nothing when stopped. */
    val detections: Flow<WakeWordDetection>

    /** True between a successful [start] and the next [stop]. */
    val isRunning: Boolean

    /**
     * Begin capture + inference. Idempotent. Throws if the model is not
     * available/verified or RECORD_AUDIO permission is missing.
     */
    suspend fun start()

    /** Stop capture + inference and release audio resources. Idempotent. */
    suspend fun stop()
}
