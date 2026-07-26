package ninja.jeremy.liveninja.realtime

import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import ninja.jeremy.liveninja.net.TranscriptUploadRequest
import ninja.jeremy.liveninja.ui.state.TranscriptRole
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * WS-5 M21.1 regression tests. Android conversations were silently lost because nothing ever
 * POSTed to `/api/v1/transcript`; these lock in the two properties that make History work:
 * finished turns get uploaded, and a `final:true` flush always closes the session.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class TranscriptUploaderTest {

    @Test
    fun `final flush posts even with no pending turns`() = runTest {
        val api = FakeSink()
        val uploader = TranscriptUploader(api, this)

        uploader.begin("sess-1", TranscriptUploader.ENGINE_OPENAI)
        uploader.finish()
        advanceUntilIdle()

        // A final-only flush with zero turns is valid and is the seam that triggers
        // topics-extract server-side — it must be sent, not optimized away.
        assertEquals(1, api.requests.size)
        assertTrue(api.requests[0].final)
        assertEquals("sess-1", api.requests[0].sessionId)
        assertTrue(api.requests[0].turns.isEmpty())
    }

    @Test
    fun `turns are uploaded with monotonic seq, role and engine`() = runTest {
        val api = FakeSink()
        val uploader = TranscriptUploader(api, this)

        uploader.begin("sess-2", TranscriptUploader.ENGINE_GEMINI)
        uploader.record(TranscriptRole.USER, "what is the weather")
        uploader.record(TranscriptRole.ASSISTANT, "it is overcast and 72")
        uploader.finish()
        advanceUntilIdle()

        val turns = api.requests.flatMap { it.turns }
        assertEquals(2, turns.size)
        assertEquals(listOf(1, 2), turns.map { it.seq })
        assertEquals(listOf("user", "assistant"), turns.map { it.role })
        assertTrue(turns.all { it.engine == TranscriptUploader.ENGINE_GEMINI })
        assertTrue("the last request must close the session", api.requests.last().final)
    }

    @Test
    fun `blank turns are dropped rather than uploaded`() = runTest {
        val api = FakeSink()
        val uploader = TranscriptUploader(api, this)

        uploader.begin("sess-3", TranscriptUploader.ENGINE_OPENAI)
        uploader.record(TranscriptRole.USER, "   ")
        uploader.record(TranscriptRole.USER, "")
        uploader.finish()
        advanceUntilIdle()

        assertTrue(api.requests.flatMap { it.turns }.isEmpty())
    }

    @Test
    fun `no sessionId means nothing is uploaded`() = runTest {
        val api = FakeSink()
        val uploader = TranscriptUploader(api, this)

        // The broker didn't return an id — there is nothing to key rows against, so the
        // uploader must stay quiet rather than invent one.
        uploader.begin(null, TranscriptUploader.ENGINE_OPENAI)
        uploader.record(TranscriptRole.USER, "hello")
        uploader.finish()
        advanceUntilIdle()

        assertTrue(api.requests.isEmpty())
    }

    @Test
    fun `a full batch flushes without waiting for session end`() = runTest {
        val api = FakeSink()
        val uploader = TranscriptUploader(api, this)

        uploader.begin("sess-4", TranscriptUploader.ENGINE_OPENAI)
        repeat(TranscriptUploader.BATCH_SIZE) { uploader.record(TranscriptRole.USER, "turn $it") }
        advanceUntilIdle()

        // Long conversations must not be all-or-nothing at the end.
        assertEquals(1, api.requests.size)
        assertEquals(TranscriptUploader.BATCH_SIZE, api.requests[0].turns.size)
        assertTrue(!api.requests[0].final)
    }

    @Test
    fun `an upload failure never propagates into the session`() = runTest {
        val api = FakeSink(failing = true)
        val uploader = TranscriptUploader(api, this)

        uploader.begin("sess-5", TranscriptUploader.ENGINE_OPENAI)
        uploader.record(TranscriptRole.USER, "hello")
        uploader.finish()
        advanceUntilIdle()
        // Reaching here without an exception is the assertion: a lost history row must
        // never interrupt a live conversation.
    }

    @Test
    fun `engine label is derived from the session mode`() {
        assertEquals(
            TranscriptUploader.ENGINE_OPENAI,
            TranscriptUploader.engineForMode(RealtimeSession.MODE_OPENAI_DIRECT),
        )
        assertEquals(
            TranscriptUploader.ENGINE_NOVA,
            TranscriptUploader.engineForMode(RealtimeSession.MODE_NOVA_BRIDGE),
        )
        assertEquals(
            TranscriptUploader.ENGINE_GEMINI,
            TranscriptUploader.engineForMode(RealtimeSession.MODE_GEMINI_DIRECT),
        )
        // An unknown//future mode must still upload under a sane label.
        assertEquals(TranscriptUploader.ENGINE_OPENAI, TranscriptUploader.engineForMode("something-new"))
    }

    /** One-method fake for the [TranscriptSink] seam. */
    private class FakeSink(private val failing: Boolean = false) : TranscriptSink {
        val requests = mutableListOf<TranscriptUploadRequest>()
        override suspend fun upload(body: TranscriptUploadRequest) {
            if (failing) throw IllegalStateException("simulated network failure")
            requests += body
        }
    }
}
