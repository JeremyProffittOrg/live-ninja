package ninja.jeremy.liveninja.wake

import ai.onnxruntime.OnnxTensor
import ai.onnxruntime.OrtEnvironment
import ai.onnxruntime.OrtSession
import android.content.Context
import android.util.Base64
import android.util.Log
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.io.ByteArrayInputStream
import java.io.Closeable
import java.nio.FloatBuffer
import java.util.zip.GZIPInputStream
import kotlin.math.max
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

/**
 * M4 FRR/FAR regression gate over the exact ONNX models and streaming pipeline shipped in the
 * APK. The fixed dual-voice corpus lives in [WakeCorpusFixtures] as gzip-compressed, signed
 * 8-bit PCM (expanded to int16 here); keeping it in androidTest means it never bloats the app.
 *
 * This is deliberately instrumented rather than a JVM test: onnxruntime-android's native
 * execution provider must be exercised on the same ABI/runtime as the app. CI runs the suite on
 * an API-35 x86_64 emulator, and the same test can be run unchanged on a connected physical
 * device with `./gradlew connectedDebugAndroidTest`.
 *
 * Baseline for hey_jarvis_v0.1 at the product's default 0.50 threshold:
 *  - false rejects: 0
 *  - false accepts: 1 ("Okay Jarvis", retained as a hard negative so the known weakness cannot
 *    silently grow)
 *
 * The gate is "no regression versus baseline", matching PRD KPI-04. It is not a claim that this
 * compact synthetic corpus replaces the owner/device far-field verification.
 */
@RunWith(AndroidJUnit4::class)
class WakeCorpusInstrumentedTest {

    @Test
    fun shippedOpenWakeWordModelDoesNotRegressFrrOrFar() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        CorpusModelRunner(context).use { runner ->
            val scored = WakeCorpusFixtures.clips.map { clip ->
                clip to runner.maxScore(clip.decodePcm16())
            }
            val falseRejects = scored.count { (clip, score) ->
                clip.wakeExpected && score < DEFAULT_THRESHOLD
            }
            val falseAccepts = scored.count { (clip, score) ->
                !clip.wakeExpected && score >= DEFAULT_THRESHOLD
            }
            val summary = scored.joinToString { (clip, score) ->
                "${clip.name}=${"%.4f".format(score)}"
            }
            Log.i(TAG, "wake corpus: $summary; FR=$falseRejects FA=$falseAccepts")

            assertTrue(
                "FRR regressed: $falseRejects false rejects, baseline " +
                    "$BASELINE_FALSE_REJECTS; $summary",
                falseRejects <= BASELINE_FALSE_REJECTS,
            )
            assertTrue(
                "FAR regressed: $falseAccepts false accepts, baseline " +
                    "$BASELINE_FALSE_ACCEPTS; $summary",
                falseAccepts <= BASELINE_FALSE_ACCEPTS,
            )
        }
    }

    private class CorpusModelRunner(context: Context) : Closeable {
        private val env = OrtEnvironment.getEnvironment()
        private val options = OrtSession.SessionOptions().apply {
            setIntraOpNumThreads(1)
            setInterOpNumThreads(1)
        }
        private val mel = env.createSession(
            context.assets.open("wakeword/melspectrogram.onnx").use { it.readBytes() },
            options,
        )
        private val embedding = env.createSession(
            context.assets.open("wakeword/embedding_model.onnx").use { it.readBytes() },
            options,
        )
        private val head = env.createSession(
            context.assets.open("wakeword/hey_jarvis_v0.1.onnx").use { it.readBytes() },
            options,
        )

        fun maxScore(samples: ShortArray): Float {
            val pipeline = OwwPipeline(::runMelspec, ::runEmbedding, ::runHead)
            val vad = EnergyVad()
            val preRoll = ArrayDeque<ShortArray>(PRE_ROLL_CHUNKS)
            val leading = PRE_ROLL_CHUNKS + 1
            val padded = ShortArray(
                leading * OwwPipeline.CHUNK_SAMPLES +
                    samples.size +
                    TRAILING_SILENCE_CHUNKS * OwwPipeline.CHUNK_SAMPLES,
            )
            samples.copyInto(padded, leading * OwwPipeline.CHUNK_SAMPLES)

            EnergyVad.chargingActive = false
            var peak = 0f
            for (offset in padded.indices step OwwPipeline.CHUNK_SAMPLES) {
                val end = minOf(offset + OwwPipeline.CHUNK_SAMPLES, padded.size)
                val chunk = padded.copyOfRange(offset, end).let {
                    if (it.size == OwwPipeline.CHUNK_SAMPLES) it
                    else it.copyOf(OwwPipeline.CHUNK_SAMPLES)
                }
                val nowMs = (offset / OwwPipeline.CHUNK_SAMPLES) * CHUNK_MS
                if (!vad.accept(chunk, nowMs)) {
                    if (preRoll.size == PRE_ROLL_CHUNKS) preRoll.removeFirst()
                    preRoll.addLast(chunk)
                    continue
                }
                if (vad.gateJustOpened) {
                    pipeline.reset()
                    for (buffered in preRoll) peak = max(peak, pipeline.process(buffered))
                    preRoll.clear()
                }
                peak = max(peak, pipeline.process(chunk))
            }
            return peak
        }

        private fun runMelspec(input: FloatArray): Array<FloatArray> {
            OnnxTensor.createTensor(
                env,
                FloatBuffer.wrap(input),
                longArrayOf(1, input.size.toLong()),
            ).use { tensor ->
                mel.run(mapOf(mel.inputNames.first() to tensor)).use { result ->
                    val flat = flattenFloats(result[0].value)
                    val frames = flat.size / OwwPipeline.MEL_BINS
                    return Array(frames) { frame ->
                        FloatArray(OwwPipeline.MEL_BINS) { bin ->
                            flat[frame * OwwPipeline.MEL_BINS + bin] / 10f + 2f
                        }
                    }
                }
            }
        }

        private fun runEmbedding(melWindow: Array<FloatArray>): FloatArray {
            val flat = FloatArray(OwwPipeline.MEL_WINDOW * OwwPipeline.MEL_BINS)
            for (frame in 0 until OwwPipeline.MEL_WINDOW) {
                melWindow[frame].copyInto(flat, frame * OwwPipeline.MEL_BINS)
            }
            OnnxTensor.createTensor(
                env,
                FloatBuffer.wrap(flat),
                longArrayOf(
                    1,
                    OwwPipeline.MEL_WINDOW.toLong(),
                    OwwPipeline.MEL_BINS.toLong(),
                    1,
                ),
            ).use { tensor ->
                embedding.run(mapOf(embedding.inputNames.first() to tensor)).use { result ->
                    return flattenFloats(result[0].value)
                }
            }
        }

        private fun runHead(embeddingWindow: Array<FloatArray>): Float {
            val flat = FloatArray(OwwPipeline.EMB_WINDOW * OwwPipeline.EMB_DIM)
            for (index in 0 until OwwPipeline.EMB_WINDOW) {
                embeddingWindow[index].copyInto(flat, index * OwwPipeline.EMB_DIM)
            }
            OnnxTensor.createTensor(
                env,
                FloatBuffer.wrap(flat),
                longArrayOf(
                    1,
                    OwwPipeline.EMB_WINDOW.toLong(),
                    OwwPipeline.EMB_DIM.toLong(),
                ),
            ).use { tensor ->
                head.run(mapOf(head.inputNames.first() to tensor)).use { result ->
                    return flattenFloats(result[0].value).first()
                }
            }
        }

        override fun close() {
            head.close()
            embedding.close()
            mel.close()
            options.close()
        }

        private fun flattenFloats(value: Any?): FloatArray {
            val out = ArrayList<Float>(256)
            fun walk(node: Any?) {
                when (node) {
                    is FloatArray -> node.forEach(out::add)
                    is Array<*> -> node.forEach(::walk)
                    is Float -> out.add(node)
                    else -> error("unexpected ONNX output type: ${node?.javaClass}")
                }
            }
            walk(value)
            return out.toFloatArray()
        }
    }

    private companion object {
        const val TAG = "WakeCorpus"
        const val DEFAULT_THRESHOLD = 0.50f
        const val BASELINE_FALSE_REJECTS = 0
        const val BASELINE_FALSE_ACCEPTS = 1
        const val PRE_ROLL_CHUNKS = 3
        const val TRAILING_SILENCE_CHUNKS = 22
        const val CHUNK_MS = 80L
    }
}

internal data class WakeCorpusClip(
    val name: String,
    val wakeExpected: Boolean,
    val gzipPcm8Base64: String,
) {
    fun decodePcm16(): ShortArray {
        val packed = Base64.decode(gzipPcm8Base64, Base64.DEFAULT)
        val pcm8 = GZIPInputStream(ByteArrayInputStream(packed)).use { it.readBytes() }
        return ShortArray(pcm8.size) { index -> (pcm8[index].toInt() * 256).toShort() }
    }
}
