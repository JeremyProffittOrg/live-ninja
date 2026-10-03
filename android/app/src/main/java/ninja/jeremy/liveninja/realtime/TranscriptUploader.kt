package ninja.jeremy.liveninja.realtime

import android.util.Log
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.CancellationException
import ninja.jeremy.liveninja.net.AuthBoundRequest
import ninja.jeremy.liveninja.net.BoundJobsSession
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.ui.state.TranscriptRole
import ninja.jeremy.liveninja.net.TranscriptUploadTurnDto
import ninja.jeremy.liveninja.net.SessionCostDto
import ninja.jeremy.liveninja.net.TranscriptUploadRequest

/**
 * Narrow seam over `POST /api/v1/transcript`. Deliberately one method wide: the uploader needs
 * exactly this call, and a test fake shouldn't have to implement the whole [LiveNinjaApi] surface.
 */
interface TranscriptSink {
    suspend fun upload(body: TranscriptUploadRequest, expectedSessionId: String)
}

/** Production [TranscriptSink], backed by Retrofit. */
@Singleton
class ApiTranscriptSink @Inject constructor(
    private val api: LiveNinjaApi,
    private val boundSession: BoundJobsSession,
) : TranscriptSink {
    override suspend fun upload(body: TranscriptUploadRequest, expectedSessionId: String) {
        val credentials = boundSession.credentials(expectedSessionId)
        api.uploadTranscriptBound(body, "Bearer ${credentials.accessToken}", AuthBoundRequest())
    }
}

/**
 * Ships finished transcript turns to `POST /api/v1/transcript` (WS-5 M21.1).
 *
 * Until this existed, Android conversations were **lost**: turns accumulated only in the
 * process-wide [TranscriptStore], nothing was ever uploaded, so no `LOG#` rows were written, the
 * session-end `final:true` flush that triggers `cmd/topics-extract` never fired, and no `CONV`
 * record was ever created. Verified on hardware 2026-07-24 — three real sessions produced zero
 * History entries. The web client has always done this (`web/static/js/transcriptsink.mjs`); this
 * is the Android half of the same contract.
 *
 * Batching mirrors the web sink: flush on [BATCH_SIZE] turns or after [BATCH_INTERVAL_MS], so a
 * long conversation survives a mid-session process death rather than being all-or-nothing at the
 * end.
 *
 * Every failure is swallowed and logged. A transcript upload must never surface as an error in a
 * live conversation — losing a history row is bad, interrupting the user mid-sentence is worse.
 */
@Singleton
class TranscriptUploader internal constructor(
    private val sink: TranscriptSink,
    /**
     * Where uploads run. Injected so tests can supply a [kotlinx.coroutines.test.TestScope] and
     * drive the batching deterministically; production gets its own IO scope below.
     */
    private val scope: CoroutineScope,
) {

    @Inject
    constructor(sink: TranscriptSink) : this(sink, CoroutineScope(SupervisorJob() + Dispatchers.IO))

    private val lock = Any()
    private class Binding(val sessionId: String, val authSessionId: String, val engine: String) {
        @Volatile var valid = true
    }
    private data class Batch(val binding: Binding, val turns: List<TranscriptUploadTurnDto>)
    private var binding: Binding? = null
    // Sequence zero belongs to the broker's session-start ledger marker.
    // Starting client turns at zero makes the first spoken turn collide
    // with that marker and disappear under the idempotent write contract.
    private var nextSeq = 1
    private val pending = mutableListOf<TranscriptUploadTurnDto>()
    private var timerJob: Job? = null

    /**
     * Begin buffering for a new session. [sessionId] is the broker-issued id — with no id there is
     * nothing the server can key rows against, so uploads stay disabled for that session rather
     * than inventing one client-side.
     */
    suspend fun begin(sessionId: String?, engine: String, authSessionId: String) {
        synchronized(lock) {
            binding?.valid = false
            binding = sessionId?.takeIf { it.isNotBlank() && authSessionId.isNotBlank() }
                ?.let { Binding(it, authSessionId, engine) }
            nextSeq = 1
            pending.clear()
            timerJob?.cancel()
            timerJob = null
            if (binding == null) {
                Log.w(TAG, "no sessionId for this session; transcript will not be uploaded")
            }
        }
    }

    /** Auth teardown discards private buffered turns; it must not finish as a new user. */
    fun discard() = synchronized(lock) {
        binding?.valid = false
        binding = null
        pending.clear()
        timerJob?.cancel()
        timerJob = null
    }

    /**
     * Record one completed turn. Deltas are not uploaded — only finished turns, which is what the
     * server's `LOG#` rows and the topic extractor expect.
     */
    fun record(role: TranscriptRole, text: String) {
        val trimmed = text.trim()
        if (trimmed.isEmpty()) return
        val batch = synchronized(lock) {
            val current = binding ?: return
            pending += TranscriptUploadTurnDto(
                seq = nextSeq++,
                role = role.wireName(),
                text = trimmed,
                engine = current.engine,
            )
            if (pending.size >= BATCH_SIZE) drainLocked(current) else { armTimerLocked(current); null }
        }
        batch?.let { scope.launch { send(it, final = false) } }
    }

    /**
     * Final flush for the session. Always posts — even with an empty batch — because `final:true`
     * is the session-end seam the server uses to invoke the topic extractor and write the `CONV`
     * record (`internal/webapp/api_routes.go`: "A final-only flush with zero turns is valid").
     */
    fun finish(cost: SessionCost? = null) {
        val batch = synchronized(lock) {
            val current = binding ?: return
            drainLocked(current).also { binding = null }
        }
        scope.launch { send(batch, final = true, cost = cost) }
    }

    /** Drain the buffer under the lock, returning what to send. */
    private fun drainLocked(current: Binding): Batch {
        timerJob?.cancel()
        timerJob = null
        val batch = pending.toList()
        pending.clear()
        return Batch(current, batch)
    }

    /** Start the time-based flush if one isn't already pending. */
    private fun armTimerLocked(current: Binding) {
        if (timerJob != null) return
        timerJob = scope.launch {
            delay(BATCH_INTERVAL_MS)
            val batch = synchronized(lock) {
                if (binding !== current) return@launch
                timerJob = null
                if (pending.isEmpty()) return@launch
                drainLocked(current)
            }
            send(batch, final = false)
        }
    }

    private suspend fun send(
        batch: Batch,
        final: Boolean,
        cost: SessionCost? = null,
    ) {
        if (!batch.binding.valid) return
        val turns = batch.turns
        // The server rejects a non-final flush with no turns; nothing to do.
        if (turns.isEmpty() && !final) return
        try {
            sink.upload(
                TranscriptUploadRequest(
                    sessionId = batch.binding.sessionId,
                    final = final,
                    turns = turns,
                    // Only ever on the final flush, and only with real usage
                    // behind it — a zeroed cost would overwrite nothing useful
                    // but would claim a priced session that never was.
                    cost = cost?.takeIf { final && it.hasData }?.let {
                        SessionCostDto(
                            usd = it.usd,
                            textTokens = it.textTokens,
                            audioTokens = it.audioTokens,
                        )
                    },
                ),
                batch.binding.authSessionId,
            )
        } catch (e: CancellationException) {
            throw e
        } catch (t: Throwable) {
            Log.w(TAG, "transcript upload failed (final=$final, turns=${turns.size})", t)
        }
    }

    private fun TranscriptRole.wireName(): String = when (this) {
        TranscriptRole.USER -> "user"
        TranscriptRole.ASSISTANT -> "assistant"
    }

    companion object {
        private const val TAG = "TranscriptUploader"

        /** Turns per batch, matching the web sink. */
        const val BATCH_SIZE = 25

        /** Time-based flush window, matching the web sink's 5 s. */
        const val BATCH_INTERVAL_MS = 5_000L

        const val ENGINE_OPENAI = "gpt-realtime"
        const val ENGINE_NOVA = "nova-sonic"
        const val ENGINE_GEMINI = "gemini-flash-live"
        const val ENGINE_VOICE_LIVE = "azure-voice-live"

        /** Map a [RealtimeSession.mode] to the engine label the backend stores on each turn. */
        fun engineForMode(mode: String): String = when (mode) {
            RealtimeSession.MODE_NOVA_BRIDGE -> ENGINE_NOVA
            RealtimeSession.MODE_GEMINI_DIRECT -> ENGINE_GEMINI
            RealtimeSession.MODE_VOICE_LIVE_DIRECT -> ENGINE_VOICE_LIVE
            else -> ENGINE_OPENAI
        }
    }
}
