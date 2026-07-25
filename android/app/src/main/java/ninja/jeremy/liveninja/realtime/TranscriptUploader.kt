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
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
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
    suspend fun upload(body: TranscriptUploadRequest)
}

/** Production [TranscriptSink], backed by Retrofit. */
@Singleton
class ApiTranscriptSink @Inject constructor(
    private val api: LiveNinjaApi,
) : TranscriptSink {
    override suspend fun upload(body: TranscriptUploadRequest) = api.uploadTranscript(body)
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

    private val mutex = Mutex()

    private var sessionId: String? = null
    private var engine: String = ENGINE_OPENAI
    private var nextSeq = 0
    private val pending = mutableListOf<TranscriptUploadTurnDto>()
    private var timerJob: Job? = null

    /**
     * Begin buffering for a new session. [sessionId] is the broker-issued id — with no id there is
     * nothing the server can key rows against, so uploads stay disabled for that session rather
     * than inventing one client-side.
     */
    suspend fun begin(sessionId: String?, engine: String) {
        mutex.withLock {
            this.sessionId = sessionId?.takeIf { it.isNotBlank() }
            this.engine = engine
            nextSeq = 0
            pending.clear()
            timerJob?.cancel()
            timerJob = null
            if (this.sessionId == null) {
                Log.w(TAG, "no sessionId for this session; transcript will not be uploaded")
            }
        }
    }

    /**
     * Record one completed turn. Deltas are not uploaded — only finished turns, which is what the
     * server's `LOG#` rows and the topic extractor expect.
     */
    fun record(role: TranscriptRole, text: String) {
        val trimmed = text.trim()
        if (trimmed.isEmpty()) return
        scope.launch {
            val batch = mutex.withLock {
                if (sessionId == null) return@launch
                pending += TranscriptUploadTurnDto(
                    seq = nextSeq++,
                    role = role.wireName(),
                    text = trimmed,
                    engine = engine,
                )
                if (pending.size >= BATCH_SIZE) drainLocked() else { armTimerLocked(); null }
            }
            batch?.let { send(it, final = false) }
        }
    }

    /**
     * Final flush for the session. Always posts — even with an empty batch — because `final:true`
     * is the session-end seam the server uses to invoke the topic extractor and write the `CONV`
     * record (`internal/webapp/api_routes.go`: "A final-only flush with zero turns is valid").
     */
    fun finish(cost: SessionCost? = null) {
        scope.launch {
            val (id, batch) = mutex.withLock {
                val id = sessionId ?: return@launch
                timerJob?.cancel()
                timerJob = null
                val batch = pending.toList()
                pending.clear()
                sessionId = null
                id to batch
            }
            send(batch, final = true, sessionIdOverride = id, cost = cost)
        }
    }

    /** Drain the buffer under the lock, returning what to send. */
    private fun drainLocked(): List<TranscriptUploadTurnDto> {
        timerJob?.cancel()
        timerJob = null
        val batch = pending.toList()
        pending.clear()
        return batch
    }

    /** Start the time-based flush if one isn't already pending. */
    private fun armTimerLocked() {
        if (timerJob != null) return
        timerJob = scope.launch {
            delay(BATCH_INTERVAL_MS)
            val batch = mutex.withLock {
                timerJob = null
                if (pending.isEmpty() || sessionId == null) return@launch
                val b = pending.toList()
                pending.clear()
                b
            }
            send(batch, final = false)
        }
    }

    private suspend fun send(
        turns: List<TranscriptUploadTurnDto>,
        final: Boolean,
        sessionIdOverride: String? = null,
        cost: SessionCost? = null,
    ) {
        val id = sessionIdOverride ?: mutex.withLock { sessionId } ?: return
        // The server rejects a non-final flush with no turns; nothing to do.
        if (turns.isEmpty() && !final) return
        try {
            sink.upload(
                TranscriptUploadRequest(
                    sessionId = id,
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
            )
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

        /** Map a [RealtimeSession.mode] to the engine label the backend stores on each turn. */
        fun engineForMode(mode: String): String = when (mode) {
            RealtimeSession.MODE_NOVA_BRIDGE -> ENGINE_NOVA
            RealtimeSession.MODE_GEMINI_DIRECT -> ENGINE_GEMINI
            else -> ENGINE_OPENAI
        }
    }
}
