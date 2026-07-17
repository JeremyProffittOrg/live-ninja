package ninja.jeremy.liveninja.wake

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * JVM tests for the streaming openWakeWord pipeline's buffer/cadence logic, with the three
 * ONNX evaluations replaced by recording fakes (the real sessions are exercised on-device).
 */
class OwwPipelineTest {

    private class Fakes {
        val melspecInputs = mutableListOf<FloatArray>()
        val embedInputs = mutableListOf<Array<FloatArray>>()
        val headInputs = mutableListOf<Array<FloatArray>>()
        var headScore = 0.1f
        var melValue = 0f

        val melspec: (FloatArray) -> Array<FloatArray> = { input ->
            melspecInputs.add(input.copyOf())
            // 9 frames per call; pipeline should keep only the last 8 per chunk.
            Array(9) { FloatArray(OwwPipeline.MEL_BINS) { _ -> melValue } }
        }
        val embed: (Array<FloatArray>) -> FloatArray = { window ->
            embedInputs.add(window)
            FloatArray(OwwPipeline.EMB_DIM) { window.last().first() }
        }
        val head: (Array<FloatArray>) -> Float = { window ->
            headInputs.add(window)
            headScore
        }
    }

    private fun chunk(value: Short = 1000): ShortArray =
        ShortArray(OwwPipeline.CHUNK_SAMPLES) { value }

    @Test
    fun `reset prefills mel and embedding buffers with silence features`() {
        val f = Fakes()
        val pipeline = OwwPipeline(f.melspec, f.embed, f.head)
        pipeline.reset()

        // One silence melspec run + one silence embedding run for the prefill artifacts.
        assertEquals(1, f.melspecInputs.size)
        assertEquals(1, f.embedInputs.size)
        assertEquals(OwwPipeline.MEL_WINDOW, f.embedInputs[0].size)

        // First real chunk can immediately produce a head score over a full window.
        val score = pipeline.process(chunk())
        assertEquals(f.headScore, score, 0f)
        assertEquals(1, f.headInputs.size)
        assertEquals(OwwPipeline.EMB_WINDOW, f.headInputs[0].size)
        assertEquals(OwwPipeline.EMB_DIM, f.headInputs[0][0].size)
    }

    @Test
    fun `each chunk runs exactly one embedding and one head evaluation`() {
        val f = Fakes()
        val pipeline = OwwPipeline(f.melspec, f.embed, f.head)
        pipeline.reset()
        val embedCallsAfterReset = f.embedInputs.size

        repeat(5) { pipeline.process(chunk()) }
        assertEquals(embedCallsAfterReset + 5, f.embedInputs.size)
        assertEquals(5, f.headInputs.size)
        // Embedding always sees exactly the 76-frame window.
        assertTrue(f.embedInputs.all { it.size == OwwPipeline.MEL_WINDOW })
    }

    @Test
    fun `melspec input carries left context from the previous chunk`() {
        val f = Fakes()
        val pipeline = OwwPipeline(f.melspec, f.embed, f.head)
        pipeline.reset()

        pipeline.process(chunk(500))
        pipeline.process(chunk(700))

        val expectedLen = OwwPipeline.RAW_CONTEXT + OwwPipeline.CHUNK_SAMPLES
        // Skip the silence-prefill call at index 0.
        val first = f.melspecInputs[1]
        val second = f.melspecInputs[2]
        assertEquals(expectedLen, first.size)
        assertEquals(expectedLen, second.size)

        // First chunk: context is silence.
        for (i in 0 until OwwPipeline.RAW_CONTEXT) assertEquals(0f, first[i], 0f)
        // Second chunk: context is the tail of the first chunk (all 500s).
        for (i in 0 until OwwPipeline.RAW_CONTEXT) assertEquals(500f, second[i], 0f)
        for (i in OwwPipeline.RAW_CONTEXT until expectedLen) assertEquals(700f, second[i], 0f)
    }

    @Test
    fun `mel buffer keeps only the newest MEL_WINDOW frames`() {
        val f = Fakes()
        val pipeline = OwwPipeline(f.melspec, f.embed, f.head)
        pipeline.reset()

        // Distinguish chunks by mel value; after enough chunks the embedding window must
        // contain only recent frames (silence prefill fully evicted).
        // 76 frames / 8 per chunk -> 10 chunks flushes the window.
        repeat(10) { i ->
            f.melValue = (i + 1).toFloat()
            pipeline.process(chunk())
        }
        val lastWindow = f.embedInputs.last()
        // Oldest frame in the window must come from chunk 1 or later — never the prefill (0).
        assertTrue(lastWindow.all { frame -> frame[0] >= 1f })
        assertEquals(10f, lastWindow.last()[0], 0f)
    }

    @Test(expected = IllegalArgumentException::class)
    fun `wrong chunk size is rejected`() {
        val f = Fakes()
        val pipeline = OwwPipeline(f.melspec, f.embed, f.head)
        pipeline.reset()
        pipeline.process(ShortArray(100))
    }
}
