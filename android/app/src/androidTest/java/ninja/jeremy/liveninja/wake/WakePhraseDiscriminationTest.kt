package ninja.jeremy.liveninja.wake

import ai.onnxruntime.OnnxTensor
import ai.onnxruntime.OrtEnvironment
import ai.onnxruntime.OrtSession
import android.util.Log
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.nio.ByteBuffer
import java.nio.ByteOrder
import java.nio.FloatBuffer
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

/**
 * The wake pipeline must actually DISCRIMINATE, not merely produce a number.
 *
 * Added 2026-08-08 after "it responds to Hey <anything>". Nothing in the suite ran real speech
 * through the three-model openWakeWord stack, so a pipeline that scored every utterance alike
 * would have passed everything — [OwwPipelineTest] exercises the buffering with synthetic model
 * functions and never loads an ONNX model at all.
 *
 * Runs recorded 16 kHz mono clips through the REAL melspectrogram + embedding + head models, all
 * three from bundled assets, so this needs no network, no sign-in and no downloaded model and is
 * safe in CI. The head is the packaged `hey-jarvis` model, so "Hey Live Ninja" is a NEGATIVE here
 * — the assertion is about the phrase the loaded head was trained on, whichever that is.
 *
 * Measured when written: hey-jarvis peaks at 0.998 with 8 consecutive frames over threshold,
 * while every near-miss ("Hey Charlie", "Hey Marvin", "Hey Jennifer", "Hey Banana", …) peaks at
 * 0.135 or below and never crosses it once. That margin is what this test defends; if it narrows,
 * the wake word starts firing on anything and the only symptom a user reports is "it responds to
 * everything".
 *
 * Clips are Windows SAPI text-to-speech, committed rather than generated, so the numbers are
 * reproducible on every machine and in CI.
 */
@RunWith(AndroidJUnit4::class)
class WakePhraseDiscriminationTest {

    private val env: OrtEnvironment = OrtEnvironment.getEnvironment()

    /** Threshold the engine uses at the default 0.5 sensitivity: `1 - sensitivity`. */
    private val defaultThreshold = 0.5f

    @Test
    fun theLoadedHeadModelFiresOnItsPhraseAndOnNothingElse() {
        val appAssets = InstrumentationRegistry.getInstrumentation().targetContext.assets
        // Clips ship in the TEST apk, models in the APP apk — different asset managers, and
        // mixing them up silently yields an empty clip list and a vacuously green test.
        val testAssets = InstrumentationRegistry.getInstrumentation().context.assets

        val opts = OrtSession.SessionOptions().apply {
            setIntraOpNumThreads(1)
            setInterOpNumThreads(1)
        }
        val mel = env.createSession(
            appAssets.open(ModelManager.ASSET_MELSPECTROGRAM).use { it.readBytes() }, opts,
        )
        val emb = env.createSession(
            appAssets.open(ModelManager.ASSET_EMBEDDING).use { it.readBytes() }, opts,
        )
        val head = env.createSession(
            appAssets.open(ModelManager.ASSET_DEFAULT_HEAD).use { it.readBytes() }, opts,
        )

        try {
            val pipeline = OwwPipeline(
                melspec = { input -> runMel(mel, input) },
                embed = { window -> runEmb(emb, window) },
                head = { window -> runHead(head, window) },
            )

            val clips = testAssets.list(CLIP_DIR).orEmpty().sorted()
            assertTrue("no wake audio clips found in assets/$CLIP_DIR", clips.isNotEmpty())

            val failures = mutableListOf<String>()
            for (name in clips) {
                val peak = peakScore(pipeline, testAssets.open("$CLIP_DIR/$name").use { it.readBytes() })
                val shouldFire = name.startsWith("true_")
                Log.i(TAG, "%-30s peak=%.3f (expected %s)".format(name, peak, if (shouldFire) "FIRE" else "silent"))
                if (shouldFire && peak < defaultThreshold) {
                    failures += "$name should have fired but peaked at %.3f".format(peak)
                }
                if (!shouldFire && peak >= defaultThreshold) {
                    failures += "$name is a FALSE POSITIVE at %.3f".format(peak)
                }
            }
            assertTrue(failures.joinToString("; "), failures.isEmpty())
        } finally {
            mel.close(); emb.close(); head.close()
        }
    }

    private fun peakScore(pipeline: OwwPipeline, wav: ByteArray): Float {
        val samples = readWavPcm16(wav)
        pipeline.reset()
        var peak = 0f
        var i = 0
        while (i + OwwPipeline.CHUNK_SAMPLES <= samples.size) {
            val chunk = ShortArray(OwwPipeline.CHUNK_SAMPLES) { samples[i + it] }
            val s = pipeline.process(chunk)
            if (s > peak) peak = s
            i += OwwPipeline.CHUNK_SAMPLES
        }
        return peak
    }

    private fun runMel(session: OrtSession, input: FloatArray): Array<FloatArray> =
        OnnxTensor.createTensor(env, FloatBuffer.wrap(input), longArrayOf(1, input.size.toLong()))
            .use { t ->
                session.run(mapOf(session.inputNames.first() to t)).use { r ->
                    val flat = flatten(r[0].value)
                    Array(flat.size / OwwPipeline.MEL_BINS) { f ->
                        FloatArray(OwwPipeline.MEL_BINS) { b ->
                            flat[f * OwwPipeline.MEL_BINS + b] / 10f + 2f
                        }
                    }
                }
            }

    private fun runEmb(session: OrtSession, window: Array<FloatArray>): FloatArray {
        val flat = FloatArray(OwwPipeline.MEL_WINDOW * OwwPipeline.MEL_BINS)
        for (f in 0 until OwwPipeline.MEL_WINDOW) window[f].copyInto(flat, f * OwwPipeline.MEL_BINS)
        return OnnxTensor.createTensor(
            env,
            FloatBuffer.wrap(flat),
            longArrayOf(1, OwwPipeline.MEL_WINDOW.toLong(), OwwPipeline.MEL_BINS.toLong(), 1),
        ).use { t -> session.run(mapOf(session.inputNames.first() to t)).use { r -> flatten(r[0].value) } }
    }

    private fun runHead(session: OrtSession, window: Array<FloatArray>): Float {
        val flat = FloatArray(OwwPipeline.EMB_WINDOW * OwwPipeline.EMB_DIM)
        for (i in 0 until OwwPipeline.EMB_WINDOW) window[i].copyInto(flat, i * OwwPipeline.EMB_DIM)
        return OnnxTensor.createTensor(
            env,
            FloatBuffer.wrap(flat),
            longArrayOf(1, OwwPipeline.EMB_WINDOW.toLong(), OwwPipeline.EMB_DIM.toLong()),
        ).use { t -> session.run(mapOf(session.inputNames.first() to t)).use { r -> flatten(r[0].value)[0] } }
    }

    private fun flatten(value: Any?): FloatArray {
        val out = ArrayList<Float>(256)
        fun walk(v: Any?) {
            when (v) {
                is FloatArray -> for (x in v) out.add(x)
                is Array<*> -> for (c in v) walk(c)
                is Float -> out.add(v)
                else -> error("unexpected ONNX output type: ${v?.javaClass}")
            }
        }
        walk(value)
        return out.toFloatArray()
    }

    /** Minimal RIFF reader: walk to the `data` chunk and return little-endian PCM16 samples. */
    private fun readWavPcm16(bytes: ByteArray): ShortArray {
        val bb = ByteBuffer.wrap(bytes).order(ByteOrder.LITTLE_ENDIAN)
        var pos = 12 // past "RIFF" <size> "WAVE"
        while (pos + 8 <= bytes.size) {
            val id = String(bytes, pos, 4, Charsets.US_ASCII)
            val size = bb.getInt(pos + 4)
            if (id == "data") {
                return ShortArray(size / 2) { bb.getShort(pos + 8 + it * 2) }
            }
            pos += 8 + size + (size and 1)
        }
        error("no data chunk in wav")
    }

    private companion object {
        const val TAG = "WakeDiscrimination"
        const val CLIP_DIR = "wakeaudio"
    }
}
