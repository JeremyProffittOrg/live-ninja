package ninja.jeremy.liveninja.realtime

import android.content.Context
import android.media.AudioAttributes
import android.media.AudioDeviceInfo
import android.media.AudioFormat
import android.media.AudioManager
import android.media.AudioRecord
import android.media.AudioTrack
import android.media.MediaRecorder
import android.os.Build
import dagger.hilt.android.qualifiers.ApplicationContext
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
import java.io.IOException
import java.net.URLEncoder
import java.time.Instant
import java.util.Base64
import java.util.Collections
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withTimeout
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.ByteString
import org.json.JSONObject

/**
 * Gemini Live implementation of [RealtimeTransport] (M13 gemini-flash-live),
 * forked from the [NovaBridgeTransport] WSS skeleton.
 *
 * Unlike Nova this is *client-direct*: the device opens a WebSocket straight
 * to `generativelanguage.googleapis.com` (the v1alpha `…Constrained` method)
 * authenticated by a single-use ephemeral token in the `?access_token=` query
 * param (URL-encoded — kept as URL auth for parity with web, gemini-plan.md
 * §3.5). The broker's API key never reaches the device.
 *
 * Wire protocol (Phase 0 spike, gemini-plan.md §10 — corrections override §3.2):
 *   * On open the client sends `{"setup": <sessionConfig verbatim>}` (the raw
 *     frame body minted server-side); readiness gates on a `setupComplete`
 *     server message (replacing Nova's `session.start`).
 *   * Uplink audio is JSON + base64 — `realtimeInput.audio {data, mimeType
 *     "audio/pcm;rate=16000"}` — NOT raw binary frames like Nova.
 *   * Downlink audio is `serverContent.modelTurn.parts[].inlineData.data`
 *     (base64 PCM16 @ 24 kHz) into the same AudioTrack playback path.
 *   * Transcripts stream as `serverContent.inputTranscription` /
 *     `.outputTranscription` text deltas; `serverContent.turnComplete` ends
 *     the assistant turn.
 *   * Barge-in is server-VAD driven: `serverContent.interrupted: true` means
 *     flush local playback. There is NO client→server cancel primitive —
 *     `response.cancel` is never sent.
 *   * Tools: `toolCall.functionCalls[]{id,name,args}` → the shared
 *     `POST /api/v1/tools/invoke` flow → `toolResponse.functionResponses[]`.
 *     `toolCallCancellation.ids` drops the matching pending responses.
 *   * Lifecycle: connections recycle ~10 min. `sessionResumptionUpdate`
 *     handles are stored; on `goAway` the transport reconnects to the same
 *     endpoint+token (valid for resumption reconnects within the token's
 *     ~30-min window) sending `setup` + `sessionResumption.handle`; past the
 *     token expiry it re-fetches the session bootstrap for a fresh token —
 *     the same reconnect-re-mint pattern as the Nova bridge.
 */
@Singleton
class GeminiLiveTransport @Inject constructor(
    @ApplicationContext private val context: Context,
    private val httpClient: OkHttpClient,
    private val sessionApi: RealtimeSessionApi,
) : RealtimeTransport {

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val connectMutex = Mutex()

    private val _state = MutableStateFlow(TransportState.IDLE)
    override val state: StateFlow<TransportState> = _state.asStateFlow()

    private val _events = MutableSharedFlow<RealtimeEvent>(extraBufferCapacity = 256)
    override val events: SharedFlow<RealtimeEvent> = _events.asSharedFlow()

    @Volatile
    override var halfDuplex: Boolean = false

    @Volatile
    private var webSocket: WebSocket? = null
    private var setupReady: CompletableDeferred<Unit>? = null

    // ---- session bootstrap (prime() + connect() params) ----

    /** Raw `setup` frame body from the bootstrap; sent verbatim on every open. */
    @Volatile
    private var sessionConfig: JSONObject? = null

    @Volatile
    private var endpoint: String = ""

    @Volatile
    private var tokenValue: String = ""

    /** Epoch ms end of the token's message window; 0 = unknown (treat as expired on reconnect). */
    @Volatile
    private var tokenExpiresAtMs: Long = 0L

    /** Latest resumable `sessionResumptionUpdate` handle (survives goAway reconnects). */
    @Volatile
    private var resumptionHandle: String? = null

    @Volatile
    private var reconnecting = false
    private var reconnectJob: Job? = null

    private var audioRecord: AudioRecord? = null
    private var audioTrack: AudioTrack? = null
    private var captureJob: Job? = null
    private var playbackJob: Job? = null
    private val playbackQueue = Channel<ByteArray>(Channel.UNLIMITED)

    @Volatile
    private var assistantSpeaking = false

    @Volatile
    private var userMuted = false

    @Volatile
    private var running = false

    private var previousAudioMode = AudioManager.MODE_NORMAL
    private var previousSpeakerphone = false

    // Per-turn transcript accumulators (Gemini streams bare text deltas with
    // no item ids, so turns get synthetic ids for the common event schema).
    private val turnLock = Any()
    private var userTurnSeq = 0
    private var assistantTurnSeq = 0
    private var userTurnId: String? = null
    private var assistantTurnId: String? = null
    private val userTurnText = StringBuilder()
    private val assistantTurnText = StringBuilder()

    // Tool-call bookkeeping: Gemini's functionResponse requires the call's
    // name alongside its id, and cancelled calls must not be answered.
    private val pendingToolNames = Collections.synchronizedMap(HashMap<String, String>())
    private val cancelledToolCalls = Collections.synchronizedSet(HashSet<String>())

    override fun prime(session: RealtimeSession) {
        // Deep-copy so later bootstrap reuse can't be mutated from outside.
        sessionConfig = session.sessionConfig?.let { JSONObject(it.toString()) }
        tokenExpiresAtMs = parseRfc3339Ms(session.accessToken?.expiresAt)
        resumptionHandle = null // fresh session — no handle yet
    }

    override suspend fun connect(ephemeralToken: String, callsUrl: String) {
        // Interface params reused engine-agnostically: ephemeralToken = the
        // single-use Gemini access token, callsUrl = the Gemini WSS endpoint.
        connectMutex.withLock {
            check(_state.value != TransportState.CONNECTING && _state.value != TransportState.CONNECTED) {
                "transport already ${_state.value}"
            }
            _state.value = TransportState.CONNECTING
            try {
                doConnect(token = ephemeralToken, wsEndpoint = callsUrl)
                _state.value = TransportState.CONNECTED
            } catch (t: Throwable) {
                releaseSession()
                _state.value = TransportState.FAILED
                throw t
            }
        }
    }

    private suspend fun doConnect(token: String, wsEndpoint: String) {
        if (wsEndpoint.isEmpty()) throw IOException("gemini endpoint was empty")
        if (token.isEmpty()) throw IOException("gemini access token was empty")
        if (sessionConfig == null) throw IOException("gemini transport not primed with sessionConfig")
        endpoint = wsEndpoint
        tokenValue = token

        openSocket()
        configureAudioForCall()
        startCapture()
        startPlayback()
        running = true
    }

    /** Opens a socket to [endpoint]+[tokenValue] and awaits `setupComplete`. */
    private suspend fun openSocket() {
        val ready = CompletableDeferred<Unit>()
        setupReady = ready

        val request = Request.Builder().url(buildUrl(endpoint, tokenValue)).build()
        val ws = httpClient.newWebSocket(request, GeminiListener())
        webSocket = ws

        try {
            withTimeout(SETUP_TIMEOUT_MS) { ready.await() }
        } catch (_: TimeoutCancellationException) {
            runCatching { ws.close(1000, "setup timeout") }
            throw IOException("gemini live did not complete setup within ${SETUP_TIMEOUT_MS}ms")
        }
    }

    private fun buildUrl(wsEndpoint: String, token: String): String {
        var url = wsEndpoint.trim()
        if (url.startsWith("https://")) url = "wss://" + url.removePrefix("https://")
        if (url.startsWith("http://")) url = "ws://" + url.removePrefix("http://")
        // URL auth parity with web (gemini-plan.md §3.5); token name must be
        // URL-escaped ("auth_tokens/…" carries a slash).
        return url + (if (url.contains("?")) "&" else "?") +
            "access_token=" + URLEncoder.encode(token, "UTF-8")
    }

    /** `{"setup": <sessionConfig>}`, with the stored resumption handle when resuming. */
    private fun buildSetupFrame(): JSONObject {
        val body = JSONObject(requireNotNull(sessionConfig).toString())
        resumptionHandle?.let { body.put("sessionResumption", JSONObject().put("handle", it)) }
        return JSONObject().put("setup", body)
    }

    // ---- outbound control (translate the shared OpenAI-shaped events) ----

    override fun sendEvent(event: JSONObject) {
        val ws = webSocket ?: return
        val translated = translateOutbound(event) ?: return
        runCatching { ws.send(translated.toString()) }
            .onFailure { LNLog.w(LogCategory.REALTIME, TAG, "gemini sendEvent failed", it) }
    }

    /**
     * Map the coordinator's OpenAI-shaped control events onto Gemini Live
     * client messages. Returns null for events with no Gemini equivalent
     * (`response.create` is implicit; there is no cancel primitive, so
     * `response.cancel`/`output_audio_buffer.clear` map to the local flush in
     * [stopPlayback] only). Unknown events are dropped — Gemini rejects
     * unrecognized frames, so there is no forward-compat passthrough.
     */
    private fun translateOutbound(event: JSONObject): JSONObject? {
        when (event.optString("type")) {
            "conversation.item.create" -> {
                val item = event.optJSONObject("item") ?: return null
                return when (item.optString("type")) {
                    "function_call_output" -> {
                        val callId = item.optString("call_id")
                        if (cancelledToolCalls.remove(callId)) {
                            LNLog.d(LogCategory.REALTIME, TAG, "dropping tool response for cancelled call $callId")
                            return null
                        }
                        val name = pendingToolNames.remove(callId).orEmpty()
                        val rawOutput = item.optString("output")
                        val result: Any = runCatching { JSONObject(rawOutput) }.getOrElse { rawOutput }
                        JSONObject().put(
                            "toolResponse",
                            JSONObject().put(
                                "functionResponses",
                                org.json.JSONArray().put(
                                    JSONObject()
                                        .put("id", callId)
                                        .put("name", name)
                                        .put("response", JSONObject().put("result", result)),
                                ),
                            ),
                        )
                    }

                    "message" -> {
                        val text = item.optJSONArray("content")
                            ?.optJSONObject(0)?.optString("text").orEmpty()
                        if (text.isEmpty()) {
                            null
                        } else {
                            JSONObject().put(
                                "clientContent",
                                JSONObject()
                                    .put(
                                        "turns",
                                        org.json.JSONArray().put(
                                            JSONObject()
                                                .put("role", "user")
                                                .put(
                                                    "parts",
                                                    org.json.JSONArray().put(JSONObject().put("text", text)),
                                                ),
                                        ),
                                    )
                                    .put("turnComplete", true),
                            )
                        }
                    }

                    else -> null
                }
            }

            "response.create" -> return null // implicit on Gemini
            "input_audio_buffer.commit" ->
                return JSONObject().put("realtimeInput", JSONObject().put("audioStreamEnd", true))
            "response.cancel", "output_audio_buffer.clear" -> return null // no cancel primitive
        }
        return null
    }

    override fun setMicMuted(muted: Boolean) {
        userMuted = muted
    }

    override fun stopPlayback() {
        // Manual stop / barge-in: local flush only — Gemini has no
        // client→server cancel; its VAD interrupts generation server-side.
        interruptPlayback()
    }

    override suspend fun disconnect() {
        connectMutex.withLock {
            releaseSession()
            if (_state.value != TransportState.IDLE) {
                _state.value = TransportState.CLOSED
            }
        }
    }

    // ---- inbound server messages ----

    private fun handleTextMessage(raw: String) {
        val json = runCatching { JSONObject(raw) }.getOrNull() ?: return

        // Fields ride on the same BidiGenerateContentServerMessage envelope,
        // so check independently rather than treating them as exclusive.
        if (json.has("setupComplete")) {
            emit(RealtimeEvent.SessionCreated(null))
            setupReady?.complete(Unit)
        }

        json.optJSONObject("serverContent")?.let(::handleServerContent)

        json.optJSONObject("toolCall")?.let { toolCall ->
            val calls = toolCall.optJSONArray("functionCalls") ?: return@let
            for (i in 0 until calls.length()) {
                val call = calls.optJSONObject(i) ?: continue
                val id = call.optString("id")
                val name = call.optString("name")
                val args = call.opt("args")
                val argsJson = when (args) {
                    is JSONObject -> args.toString()
                    is String -> args.ifEmpty { "{}" }
                    null -> "{}"
                    else -> args.toString()
                }
                if (id.isNotEmpty()) pendingToolNames[id] = name
                emit(RealtimeEvent.FunctionCall(callId = id, name = name, argumentsJson = argsJson))
            }
        }

        json.optJSONObject("toolCallCancellation")?.let { cancellation ->
            val ids = cancellation.optJSONArray("ids") ?: return@let
            for (i in 0 until ids.length()) {
                val id = ids.optString(i)
                if (id.isEmpty()) continue
                pendingToolNames.remove(id)
                cancelledToolCalls.add(id)
            }
        }

        json.optJSONObject("sessionResumptionUpdate")?.let { update ->
            val handle = update.optString("newHandle")
            if (update.optBoolean("resumable") && handle.isNotEmpty()) {
                resumptionHandle = handle
            }
        }

        if (json.has("goAway")) {
            // Server recycles the connection (~10 min). Reconnect proactively
            // with the stored resumption handle; the session continues.
            LNLog.i(LogCategory.REALTIME, TAG, "gemini goAway (timeLeft=${json.optJSONObject("goAway")?.optString("timeLeft")})")
            scheduleReconnect()
        }

        if (json.has("usageMetadata")) {
            // No cost/usage surface exists in the Android transports (the
            // OpenAI path doesn't propagate usage either); republish for
            // diagnostics observers, mirroring the parser's Other events.
            emit(RealtimeEvent.Other("usageMetadata", json))
        }
    }

    private fun handleServerContent(sc: JSONObject) {
        if (sc.optBoolean("interrupted")) {
            // Server VAD heard the user over the assistant: flush local
            // playback at once (no server cancel exists or is needed) and
            // close out the cut-off assistant turn with what was heard.
            interruptPlayback()
            finalizeAssistantTurn()
            emit(RealtimeEvent.SpeechStarted)
        }

        sc.optJSONObject("inputTranscription")?.optString("text")?.takeIf { it.isNotEmpty() }?.let { delta ->
            val itemId = synchronized(turnLock) {
                userTurnId ?: "gemini-user-${++userTurnSeq}".also {
                    userTurnId = it
                    userTurnText.setLength(0)
                }
            }
            synchronized(turnLock) { userTurnText.append(delta) }
            emit(RealtimeEvent.UserTranscriptDelta(itemId, delta))
        }

        sc.optJSONObject("outputTranscription")?.optString("text")?.takeIf { it.isNotEmpty() }?.let { delta ->
            beginAssistantTurn()
            val itemId = synchronized(turnLock) { assistantTurnId.orEmpty() }
            synchronized(turnLock) { assistantTurnText.append(delta) }
            emit(RealtimeEvent.AssistantTranscriptDelta(itemId, delta))
        }

        sc.optJSONObject("modelTurn")?.optJSONArray("parts")?.let { parts ->
            for (i in 0 until parts.length()) {
                val data = parts.optJSONObject(i)?.optJSONObject("inlineData")
                    ?.optString("data").orEmpty()
                if (data.isEmpty()) continue
                val pcm = runCatching { Base64.getDecoder().decode(data) }.getOrNull() ?: continue
                enqueuePlayback(pcm)
            }
        }

        if (sc.optBoolean("turnComplete")) {
            finalizeUserTurn()
            finalizeAssistantTurn()
        }
    }

    /** The model started answering: the user's utterance is complete. */
    private fun beginAssistantTurn() {
        finalizeUserTurn()
        synchronized(turnLock) {
            if (assistantTurnId == null) {
                assistantTurnId = "gemini-assistant-${++assistantTurnSeq}"
                assistantTurnText.setLength(0)
            } else {
                return
            }
        }
        emit(RealtimeEvent.ResponseStarted(null))
    }

    private fun finalizeUserTurn() {
        val (itemId, text) = synchronized(turnLock) {
            val id = userTurnId ?: return
            userTurnId = null
            id to userTurnText.toString().also { userTurnText.setLength(0) }
        }
        emit(RealtimeEvent.SpeechStopped)
        emit(RealtimeEvent.UserTranscriptCompleted(itemId, text))
    }

    private fun finalizeAssistantTurn() {
        val turn = synchronized(turnLock) {
            val id = assistantTurnId
            assistantTurnId = null
            val text = assistantTurnText.toString()
            assistantTurnText.setLength(0)
            id?.let { it to text }
        }
        val wasSpeaking = assistantSpeaking
        if (turn == null && !wasSpeaking) return // nothing in flight
        if (wasSpeaking) {
            assistantSpeaking = false
            emit(RealtimeEvent.AssistantAudioStopped)
        }
        turn?.let { (itemId, text) -> emit(RealtimeEvent.AssistantTranscriptDone(itemId, text)) }
        emit(RealtimeEvent.ResponseDone(null))
    }

    private fun enqueuePlayback(pcm: ByteArray) {
        if (!running) return
        beginAssistantTurn()
        if (!assistantSpeaking) {
            assistantSpeaking = true
            emit(RealtimeEvent.AssistantAudioStarted)
        }
        playbackQueue.trySend(pcm)
    }

    // ---- barge-in (local flush only — never a server frame) ----

    private fun interruptPlayback() {
        val wasSpeaking = assistantSpeaking
        assistantSpeaking = false
        while (playbackQueue.tryReceive().isSuccess) { /* drain */ }
        runCatching {
            audioTrack?.let { track ->
                track.pause()
                track.flush()
                track.play()
            }
        }
        if (wasSpeaking) emit(RealtimeEvent.AssistantAudioStopped)
    }

    // ---- goAway / resumption lifecycle ----

    private fun scheduleReconnect() {
        if (!running) return
        if (reconnectJob?.isActive == true) return
        reconnectJob = scope.launch {
            try {
                reconnect()
                LNLog.i(LogCategory.REALTIME, TAG, "gemini session resumed (handle=${resumptionHandle != null})")
            } catch (t: Throwable) {
                LNLog.w(LogCategory.REALTIME, TAG, "gemini resumption reconnect failed", t)
                if (_state.value == TransportState.CONNECTED) {
                    _state.value = TransportState.FAILED
                }
            }
        }
    }

    /**
     * Reopen the WSS session, resuming via the stored handle. Within the
     * token's ~30-min window the same endpoint+token reconnects (resumption
     * reconnects are not bounded by the first-connect window); past it, the
     * session bootstrap is re-fetched for a fresh token — mirroring the Nova
     * bridge's reconnect-re-mint pattern. Audio capture/playback keep running
     * throughout; uplink pauses while [webSocket] is null.
     */
    private suspend fun reconnect() {
        reconnecting = true
        try {
            val old = webSocket
            webSocket = null // capture loop skips frames until the new socket is ready
            old?.let { runCatching { it.close(1000, "resuming") } }

            if (System.currentTimeMillis() >= tokenExpiresAtMs - TOKEN_EXPIRY_MARGIN_MS) {
                val session = sessionApi.fetchSession()
                if (session.mode != RealtimeSession.MODE_GEMINI_DIRECT ||
                    session.geminiEndpoint == null || session.accessToken == null ||
                    session.sessionConfig == null
                ) {
                    throw IOException("re-fetched session is no longer gemini-direct")
                }
                endpoint = session.geminiEndpoint
                tokenValue = session.accessToken.value
                tokenExpiresAtMs = parseRfc3339Ms(session.accessToken.expiresAt)
                sessionConfig = JSONObject(session.sessionConfig.toString())
            }

            openSocket()
        } finally {
            reconnecting = false
        }
    }

    private fun parseRfc3339Ms(value: String?): Long =
        value?.let { runCatching { Instant.parse(it).toEpochMilli() }.getOrNull() } ?: 0L

    // ---- audio capture / playback ----

    private fun startCapture() {
        val minBuf = AudioRecord.getMinBufferSize(
            IN_RATE,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        ).coerceAtLeast(2048)
        val frameBytes = (IN_RATE / 50) * 2 // ~20 ms mono PCM16
        val bufSize = maxOf(minBuf, frameBytes * 2)

        val record = try {
            @Suppress("MissingPermission")
            AudioRecord(
                MediaRecorder.AudioSource.VOICE_COMMUNICATION,
                IN_RATE,
                AudioFormat.CHANNEL_IN_MONO,
                AudioFormat.ENCODING_PCM_16BIT,
                bufSize,
            )
        } catch (t: Throwable) {
            throw IOException("could not create AudioRecord", t)
        }
        if (record.state != AudioRecord.STATE_INITIALIZED) {
            record.release()
            throw IOException("AudioRecord failed to initialize")
        }
        audioRecord = record
        record.startRecording()

        val buffer = ByteArray(frameBytes)
        captureJob = scope.launch(Dispatchers.IO) {
            while (isActive) {
                val read = record.read(buffer, 0, buffer.size)
                if (read <= 0) continue
                if (!micLive()) continue
                val ws = webSocket ?: continue
                // JSON + base64 uplink framing (NOT raw binary like Nova).
                val b64 = Base64.getEncoder().encodeToString(
                    if (read == buffer.size) buffer else buffer.copyOf(read),
                )
                val frame = JSONObject().put(
                    "realtimeInput",
                    JSONObject().put(
                        "audio",
                        JSONObject().put("data", b64).put("mimeType", UPLINK_MIME),
                    ),
                )
                runCatching { ws.send(frame.toString()) }
            }
        }
    }

    /** Mic is live only when not user-muted and not half-duplex-gated. */
    private fun micLive(): Boolean = !userMuted && !(halfDuplex && assistantSpeaking)

    private fun startPlayback() {
        val minBuf = AudioTrack.getMinBufferSize(
            OUT_RATE,
            AudioFormat.CHANNEL_OUT_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        ).coerceAtLeast(4096)

        val track = AudioTrack.Builder()
            .setAudioAttributes(
                AudioAttributes.Builder()
                    .setUsage(AudioAttributes.USAGE_VOICE_COMMUNICATION)
                    .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH)
                    .build(),
            )
            .setAudioFormat(
                AudioFormat.Builder()
                    .setSampleRate(OUT_RATE)
                    .setEncoding(AudioFormat.ENCODING_PCM_16BIT)
                    .setChannelMask(AudioFormat.CHANNEL_OUT_MONO)
                    .build(),
            )
            .setBufferSizeInBytes(minBuf * 2)
            .setTransferMode(AudioTrack.MODE_STREAM)
            .build()
        audioTrack = track
        track.play()

        playbackJob = scope.launch(Dispatchers.IO) {
            for (chunk in playbackQueue) {
                if (!isActive) break
                runCatching { track.write(chunk, 0, chunk.size) }
            }
        }
    }

    // ---- audio focus / routing (mirror WebRtcTransport) ----

    private fun configureAudioForCall() {
        val am = context.getSystemService(AudioManager::class.java) ?: return
        previousAudioMode = am.mode
        am.mode = AudioManager.MODE_IN_COMMUNICATION
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            // Prefer BT SCO → wired → speaker (mirror WebRtcTransport, 02-voice §B3).
            val devices = am.availableCommunicationDevices
            PREFERRED_ROUTE_TYPES.firstNotNullOfOrNull { type ->
                devices.firstOrNull { it.type == type }
            }?.let { am.setCommunicationDevice(it) }
        } else {
            previousSpeakerphone = am.isSpeakerphoneOn
            @Suppress("DEPRECATION")
            am.isSpeakerphoneOn = true
        }
    }

    private fun restoreAudioMode() {
        val am = context.getSystemService(AudioManager::class.java) ?: return
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            am.clearCommunicationDevice()
        } else {
            @Suppress("DEPRECATION")
            am.isSpeakerphoneOn = previousSpeakerphone
        }
        am.mode = previousAudioMode
    }

    // ---- teardown ----

    private fun releaseSession() {
        running = false
        assistantSpeaking = false
        userMuted = false
        reconnecting = false
        reconnectJob?.cancel()
        reconnectJob = null

        captureJob?.cancel()
        captureJob = null
        playbackJob?.cancel()
        playbackJob = null
        while (playbackQueue.tryReceive().isSuccess) { /* drain */ }

        setupReady?.let { if (!it.isCompleted) it.completeExceptionally(IOException("session closed")) }
        setupReady = null

        webSocket?.let { runCatching { it.close(1000, "client closing") } }
        webSocket = null

        audioRecord?.let {
            runCatching { it.stop() }
            runCatching { it.release() }
        }
        audioRecord = null
        audioTrack?.let {
            runCatching { it.pause() }
            runCatching { it.flush() }
            runCatching { it.release() }
        }
        audioTrack = null

        synchronized(turnLock) {
            userTurnId = null
            assistantTurnId = null
            userTurnText.setLength(0)
            assistantTurnText.setLength(0)
        }
        pendingToolNames.clear()
        cancelledToolCalls.clear()
        resumptionHandle = null
        sessionConfig = null
        tokenExpiresAtMs = 0L
        restoreAudioMode()
    }

    private fun emit(event: RealtimeEvent) {
        if (!_events.tryEmit(event)) {
            LNLog.w(LogCategory.REALTIME, TAG, "event buffer full; dropped ${event::class.simpleName}")
        }
    }

    private inner class GeminiListener : WebSocketListener() {

        override fun onOpen(webSocket: WebSocket, response: Response) {
            // Client-sent session setup (Gemini's replacement for OpenAI's
            // server-side session config): the raw frame body minted by the
            // broker, plus the resumption handle when reconnecting.
            runCatching { webSocket.send(buildSetupFrame().toString()) }
                .onFailure {
                    setupReady?.let { ready ->
                        if (!ready.isCompleted) {
                            ready.completeExceptionally(IOException("gemini setup send failed", it))
                        }
                    }
                }
        }

        override fun onMessage(webSocket: WebSocket, text: String) {
            if (isStale(webSocket)) return
            handleTextMessage(text)
        }

        override fun onMessage(webSocket: WebSocket, bytes: ByteString) {
            // Gemini Live serves JSON messages as binary WS frames too; the
            // payload is still UTF-8 JSON.
            if (isStale(webSocket)) return
            handleTextMessage(bytes.utf8())
        }

        override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
            val ready = setupReady
            if (ready != null && !ready.isCompleted) {
                ready.completeExceptionally(IOException("gemini live connection failed: ${t.message}", t))
                return
            }
            if (isStale(webSocket) || reconnecting) return
            if (_state.value == TransportState.CONNECTED) {
                _state.value = TransportState.FAILED
            }
        }

        override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
            webSocket.close(code, reason)
        }

        override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
            val ready = setupReady
            if (ready != null && !ready.isCompleted) {
                ready.completeExceptionally(IOException("gemini live closed before setup ($code $reason)"))
                return
            }
            if (isStale(webSocket) || reconnecting) return
            if (running && resumptionHandle != null) {
                // Recycle without (or racing) a goAway: resume the session.
                scheduleReconnect()
                return
            }
            if (_state.value == TransportState.CONNECTED) {
                _state.value = TransportState.CLOSED
            }
        }

        /** True when this callback belongs to a socket we already replaced. */
        private fun isStale(ws: WebSocket): Boolean {
            val current = this@GeminiLiveTransport.webSocket
            return current != null && current !== ws
        }
    }

    private companion object {
        const val TAG = "GeminiLiveTransport"
        const val SETUP_TIMEOUT_MS = 12_000L

        /** Gemini Live native-audio rates — identical to the platform's (§3.1). */
        const val IN_RATE = 16_000
        const val OUT_RATE = 24_000
        const val UPLINK_MIME = "audio/pcm;rate=$IN_RATE"

        /** Refresh the token this early before expiry when reconnecting. */
        const val TOKEN_EXPIRY_MARGIN_MS = 60_000L

        /** Communication-device routing preference: headset first, speaker last. */
        val PREFERRED_ROUTE_TYPES = listOf(
            AudioDeviceInfo.TYPE_BLUETOOTH_SCO,
            AudioDeviceInfo.TYPE_WIRED_HEADSET,
            AudioDeviceInfo.TYPE_WIRED_HEADPHONES,
            AudioDeviceInfo.TYPE_USB_HEADSET,
            AudioDeviceInfo.TYPE_BUILTIN_SPEAKER,
        )
    }
}
