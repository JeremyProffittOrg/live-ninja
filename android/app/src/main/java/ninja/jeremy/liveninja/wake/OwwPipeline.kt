package ninja.jeremy.liveninja.wake

/**
 * Streaming openWakeWord (oWW) three-model pipeline (plan.md M4, Android §3.1).
 *
 * openWakeWord's inference cadence, reproduced exactly:
 *  - Audio arrives as 16 kHz mono PCM in 1280-sample (80 ms) chunks.
 *  - Each chunk is prepended with [RAW_CONTEXT] samples of left context and run through the
 *    melspectrogram model; the last [MEL_FRAMES_PER_CHUNK] mel frames (32 bins each, 10 ms hop)
 *    are appended to a rolling mel buffer.
 *  - Once per chunk the last [MEL_WINDOW] mel frames (760 ms) go through the Google
 *    speech-embedding model, producing one 96-d embedding appended to a rolling buffer.
 *  - The last [EMB_WINDOW] embeddings (1.28 s stride window) go through the phrase head model,
 *    which outputs a sigmoid probability that the wake phrase just completed.
 *
 * The three model evaluations are injected as plain functions so this class stays free of any
 * ONNX Runtime dependency and is unit-testable on the JVM. [OpenWakeWordEngine] wires them to
 * real OrtSessions (including oWW's `x/10 + 2` mel post-transform).
 *
 * On [reset] the mel and embedding buffers are prefilled with the models' *silence* outputs
 * (computed once from all-zero audio) rather than raw zeros, so a detection can fire as soon as
 * the phrase itself has streamed through — no multi-second warm-up after a VAD gate opens.
 *
 * Not thread-safe; the engine confines it to its capture thread.
 *
 * @param melspec  raw samples (int16 values as float, length [RAW_CONTEXT]+[CHUNK_SAMPLES]) →
 *                 mel frames, each a FloatArray of [MEL_BINS] (already post-transformed).
 * @param embed    [MEL_WINDOW] mel frames → one embedding FloatArray of [EMB_DIM].
 * @param head     [EMB_WINDOW] embeddings → wake-phrase probability in [0,1].
 */
class OwwPipeline(
    private val melspec: (FloatArray) -> Array<FloatArray>,
    private val embed: (Array<FloatArray>) -> FloatArray,
    private val head: (Array<FloatArray>) -> Float,
) {
    companion object {
        const val SAMPLE_RATE = 16_000
        const val CHUNK_SAMPLES = 1280            // 80 ms per oWW chunk
        const val RAW_CONTEXT = 480               // left context so each chunk yields 8 full frames
        const val MEL_BINS = 32
        const val MEL_FRAMES_PER_CHUNK = 8        // 1280 samples / 160-sample hop
        const val MEL_WINDOW = 76                 // embedding model input: 76 frames x 32 bins
        const val EMB_DIM = 96
        const val EMB_WINDOW = 16                 // head model input: 16 embeddings x 96
    }

    private val rawTail = FloatArray(RAW_CONTEXT)
    private val melBuffer = ArrayDeque<FloatArray>(MEL_WINDOW)
    private val embBuffer = ArrayDeque<FloatArray>(EMB_WINDOW)

    private var silenceMelFrame: FloatArray? = null
    private var silenceEmbedding: FloatArray? = null

    /**
     * Clear all rolling state and prefill with silence features. Call before first use and
     * whenever the audio stream is discontinuous (VAD gate reopened, post-detection refractory,
     * capture restart).
     */
    fun reset() {
        rawTail.fill(0f)
        val silentMel = silenceMelFrame ?: run {
            val frames = melspec(FloatArray(RAW_CONTEXT + CHUNK_SAMPLES))
            val frame = if (frames.isNotEmpty()) frames.last() else FloatArray(MEL_BINS)
            silenceMelFrame = frame
            frame
        }
        melBuffer.clear()
        repeat(MEL_WINDOW) { melBuffer.addLast(silentMel) }
        val silentEmb = silenceEmbedding ?: run {
            val e = embed(melBuffer.toTypedArray())
            silenceEmbedding = e
            e
        }
        embBuffer.clear()
        repeat(EMB_WINDOW) { embBuffer.addLast(silentEmb) }
    }

    /**
     * Feed one 1280-sample chunk; returns the head model's wake probability for the stream
     * ending at this chunk.
     */
    fun process(chunk: ShortArray): Float {
        require(chunk.size == CHUNK_SAMPLES) { "expected $CHUNK_SAMPLES samples, got ${chunk.size}" }
        if (melBuffer.isEmpty()) reset()

        // Assemble [context | chunk] as float32 of raw int16 amplitudes (oWW convention:
        // un-normalized sample values).
        val input = FloatArray(RAW_CONTEXT + CHUNK_SAMPLES)
        rawTail.copyInto(input, 0)
        for (i in 0 until CHUNK_SAMPLES) input[RAW_CONTEXT + i] = chunk[i].toFloat()
        // Preserve the last RAW_CONTEXT samples as the next chunk's left context.
        for (i in 0 until RAW_CONTEXT) {
            rawTail[i] = input[CHUNK_SAMPLES + i]
        }

        val frames = melspec(input)
        val take = minOf(MEL_FRAMES_PER_CHUNK, frames.size)
        for (i in frames.size - take until frames.size) {
            if (melBuffer.size == MEL_WINDOW) melBuffer.removeFirst()
            melBuffer.addLast(frames[i])
        }

        val embedding = embed(melBuffer.toTypedArray())
        if (embBuffer.size == EMB_WINDOW) embBuffer.removeFirst()
        embBuffer.addLast(embedding)

        return head(embBuffer.toTypedArray())
    }
}
