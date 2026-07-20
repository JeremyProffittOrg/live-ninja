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
import okio.ByteString.Companion.toByteString
import org.json.JSONObject

/**
 * Nova Sonic bridge implementation of [RealtimeTransport] (M12 FR-VE-02/03).
 *
 * Bedrock's `InvokeModelWithBidirectionalStream` is a server-held HTTP/2 +
 * SigV4 stream, so it can't be reached client-direct the way OpenAI Realtime
 * is. A Nova-pinned device instead opens a WebSocket to the backend bridge
 * (`nova.live.jeremy.ninja`), which holds the Bedrock stream for the session
 * and normalizes Nova's events to the FR-VE-01 common schema. This transport
 * speaks the bridge protocol and re-emits the *same* [RealtimeEvent]s the
 * WebRTC path does, so [RealtimeSessionCoordinator] is engine-agnostic.
 *
 * Wire protocol (mirrors web/static/js/realtime.mjs):
 *   * Binary WS frames = raw little-endian mono PCM16. Outbound = mic input
 *     (`audio.in`); inbound = assistant output (`audio.out`). Sample rates are
 *     announced in `session.start` (default 16 kHz in / 24 kHz out).
 *   * Text WS frames = JSON control/lifecycle in the common schema:
 *     session.start / turn.start / turn.end / transcript / tool.call /
 *     speaking.start|stop / error. Outbound control (translated from the
 *     shared OpenAI-shaped [sendEvent] calls): tool.result / user.text /
 *     turn.commit / barge-in.
 *
 * Barge-in is server-driven (Nova VAD): the bridge sends `turn.start`
 * role=user when it hears the user over the assistant; this transport flushes
 * local playback immediately (Android §4.3) and reports [RealtimeEvent.SpeechStarted].
 */
@Singleton
class NovaBridgeTransport @Inject constructor(
    @ApplicationContext private val context: Context,
    private val httpClient: OkHttpClient,
) : RealtimeTransport {

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val connectMutex = Mutex()

    private val _state = MutableStateFlow(TransportState.IDLE)
    override val state: StateFlow<TransportState> = _state.asStateFlow()

    private val _events = MutableSharedFlow<RealtimeEvent>(extraBufferCapacity = 256)
    override val events: SharedFlow<RealtimeEvent> = _events.asSharedFlow()

    @Volatile
    override var halfDuplex: Boolean = false

    private var webSocket: WebSocket? = null
    private var sessionReady: CompletableDeferred<Unit>? = null

    @Volatile
    private var inRate = DEFAULT_IN_RATE

    @Volatile
    private var outRate = DEFAULT_OUT_RATE

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

    // Per-item accumulators so a final transcript event can carry full text.
    private val assistantText = HashMap<String, String>()
    private val userText = HashMap<String, String>()

    override suspend fun connect(ephemeralToken: String, callsUrl: String) {
        // Interface params reused engine-agnostically: ephemeralToken = the
        // single-use bridge token, callsUrl = the bridge WebSocket URL.
        connectMutex.withLock {
            check(_state.value != TransportState.CONNECTING && _state.value != TransportState.CONNECTED) {
                "transport already ${_state.value}"
            }
            _state.value = TransportState.CONNECTING
            try {
                doConnect(bridgeToken = ephemeralToken, wsUrl = callsUrl)
                _state.value = TransportState.CONNECTED
            } catch (t: Throwable) {
                releaseSession()
                _state.value = TransportState.FAILED
                throw t
            }
        }
    }

    private suspend fun doConnect(bridgeToken: String, wsUrl: String) {
        val url = buildUrl(wsUrl, bridgeToken)
        if (url.isEmpty()) throw IOException("nova bridge URL was empty")

        val ready = CompletableDeferred<Unit>()
        sessionReady = ready

        val request = Request.Builder().url(url).build()
        val ws = httpClient.newWebSocket(request, BridgeListener())
        webSocket = ws

        // Wait for the bridge to announce the session (sample rates, etc.).
        try {
            withTimeout(SESSION_START_TIMEOUT_MS) { ready.await() }
        } catch (_: TimeoutCancellationException) {
            throw IOException("nova bridge did not start within ${SESSION_START_TIMEOUT_MS}ms")
        }

        configureAudioForCall()
        startCapture()
        startPlayback()
        running = true
    }

    private fun buildUrl(wsUrl: String, token: String): String {
        var url = wsUrl.trim()
        if (url.startsWith("https://")) url = "wss://" + url.removePrefix("https://")
        if (url.startsWith("http://")) url = "ws://" + url.removePrefix("http://")
        if (token.isNotEmpty() && !url.contains("token=")) {
            url += (if (url.contains("?")) "&" else "?") + "token=" + token
        }
        return url
    }

    // ---- outbound control (translate the shared OpenAI-shaped events) ----

    override fun sendEvent(event: JSONObject) {
        val ws = webSocket ?: return
        val translated = translateOutbound(event) ?: return
        runCatching { ws.send(translated.toString()) }
            .onFailure { LNLog.w(LogCategory.REALTIME, TAG, "nova sendEvent failed", it) }
    }

    /**
     * Map the coordinator's OpenAI-shaped control events onto the bridge's
     * common schema. Returns null for events with no Nova equivalent (e.g.
     * `response.create`, which Nova performs implicitly).
     */
    private fun translateOutbound(event: JSONObject): JSONObject? {
        when (event.optString("type")) {
            "conversation.item.create" -> {
                val item = event.optJSONObject("item") ?: return null
                return when (item.optString("type")) {
                    "function_call_output" -> {
                        val rawOutput = item.optString("output")
                        val result: Any = runCatching { JSONObject(rawOutput) }.getOrElse { rawOutput }
                        JSONObject()
                            .put("type", "tool.result")
                            .put("callId", item.optString("call_id"))
                            .put("result", result)
                    }

                    "message" -> {
                        val text = item.optJSONArray("content")
                            ?.optJSONObject(0)?.optString("text").orEmpty()
                        if (text.isEmpty()) null
                        else JSONObject().put("type", "user.text").put("text", text)
                    }

                    else -> null
                }
            }

            "response.create" -> return null // implicit on Nova
            "input_audio_buffer.commit" -> return JSONObject().put("type", "turn.commit")
            "response.cancel", "output_audio_buffer.clear" ->
                return JSONObject().put("type", "barge-in")
        }
        return event // forward-compat passthrough
    }

    override fun setMicMuted(muted: Boolean) {
        userMuted = muted
    }

    override fun stopPlayback() {
        interruptPlayback(sendBargeIn = true)
    }

    override suspend fun disconnect() {
        connectMutex.withLock {
            releaseSession()
            if (_state.value != TransportState.IDLE) {
                _state.value = TransportState.CLOSED
            }
        }
    }

    // ---- inbound bridge messages ----

    private fun handleTextMessage(raw: String) {
        val json = runCatching { JSONObject(raw) }.getOrNull() ?: return
        when (json.optString("type")) {
            "session.start" -> {
                json.optJSONObject("input")?.optInt("sampleRate", 0)?.takeIf { it > 0 }?.let { inRate = it }
                json.optJSONObject("output")?.optInt("sampleRate", 0)?.takeIf { it > 0 }?.let { outRate = it }
                emit(RealtimeEvent.SessionCreated(json.optString("sessionId").ifEmpty { null }))
                sessionReady?.complete(Unit)
            }

            "turn.start" -> when (json.optString("role")) {
                "user" -> {
                    if (assistantSpeaking) interruptPlayback(sendBargeIn = false)
                    emit(RealtimeEvent.SpeechStarted)
                }
                "assistant" -> emit(RealtimeEvent.ResponseStarted(null))
            }

            "turn.end" -> when (json.optString("role")) {
                "user" -> emit(RealtimeEvent.SpeechStopped)
                "assistant" -> {
                    if (assistantSpeaking) {
                        assistantSpeaking = false
                        emit(RealtimeEvent.AssistantAudioStopped)
                    }
                    emit(RealtimeEvent.ResponseDone(null))
                }
            }

            "speaking.start" -> if (!assistantSpeaking) {
                assistantSpeaking = true
                emit(RealtimeEvent.AssistantAudioStarted)
            }

            "speaking.stop" -> if (assistantSpeaking) {
                assistantSpeaking = false
                emit(RealtimeEvent.AssistantAudioStopped)
            }

            "transcript" -> handleTranscript(json)

            "tool.call" -> {
                val args = json.opt("args")
                val argsJson = when (args) {
                    is JSONObject -> args.toString()
                    is String -> args.ifEmpty { "{}" }
                    null -> "{}"
                    else -> args.toString()
                }
                emit(
                    RealtimeEvent.FunctionCall(
                        callId = json.optString("callId"),
                        name = json.optString("tool"),
                        argumentsJson = argsJson,
                    ),
                )
            }

            "error" -> {
                val err = json.optJSONObject("error")
                emit(
                    RealtimeEvent.ServerError(
                        code = err?.optString("code")?.ifEmpty { null },
                        message = err?.optString("message").orEmpty().ifEmpty { "nova bridge error" },
                    ),
                )
            }
        }
    }

    private fun handleTranscript(json: JSONObject) {
        val user = json.optString("role") == "user"
        val map = if (user) userText else assistantText
        val itemId = json.optString("itemId").ifEmpty { if (user) "current-user" else "current" }
        if (json.optBoolean("final")) {
            val text = if (json.has("text")) json.optString("text") else map[itemId].orEmpty()
            map.remove(itemId)
            emit(
                if (user) RealtimeEvent.UserTranscriptCompleted(itemId, text)
                else RealtimeEvent.AssistantTranscriptDone(itemId, text),
            )
        } else {
            val delta = json.optString("delta")
            if (delta.isEmpty()) return
            map[itemId] = (map[itemId] ?: "") + delta
            emit(
                if (user) RealtimeEvent.UserTranscriptDelta(itemId, delta)
                else RealtimeEvent.AssistantTranscriptDelta(itemId, delta),
            )
        }
    }

    private fun handleBinaryMessage(bytes: ByteString) {
        if (!running) return
        if (!assistantSpeaking) {
            assistantSpeaking = true
            emit(RealtimeEvent.AssistantAudioStarted)
        }
        playbackQueue.trySend(bytes.toByteArray())
    }

    // ---- barge-in ----

    private fun interruptPlayback(sendBargeIn: Boolean) {
        assistantSpeaking = false
        // Drop buffered playback locally so the assistant goes silent at once.
        while (playbackQueue.tryReceive().isSuccess) { /* drain */ }
        runCatching {
            audioTrack?.let { track ->
                track.pause()
                track.flush()
                track.play()
            }
        }
        if (sendBargeIn) {
            runCatching { webSocket?.send(JSONObject().put("type", "barge-in").toString()) }
        }
    }

    // ---- audio capture / playback ----

    private fun startCapture() {
        val minBuf = AudioRecord.getMinBufferSize(
            inRate,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        ).coerceAtLeast(2048)
        val frameBytes = (inRate / 50) * 2 // ~20 ms mono PCM16
        val bufSize = maxOf(minBuf, frameBytes * 2)

        val record = try {
            @Suppress("MissingPermission")
            AudioRecord(
                MediaRecorder.AudioSource.VOICE_COMMUNICATION,
                inRate,
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
                val frame = if (read == buffer.size) buffer else buffer.copyOf(read)
                runCatching { ws.send(frame.toByteString(0, read.coerceAtMost(frame.size))) }
            }
        }
    }

    /** Mic is live only when not user-muted and not half-duplex-gated. */
    private fun micLive(): Boolean = !userMuted && !(halfDuplex && assistantSpeaking)

    private fun startPlayback() {
        val minBuf = AudioTrack.getMinBufferSize(
            outRate,
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
                    .setSampleRate(outRate)
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
            am.availableCommunicationDevices
                .firstOrNull { it.type == AudioDeviceInfo.TYPE_BUILTIN_SPEAKER }
                ?.let { am.setCommunicationDevice(it) }
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

        captureJob?.cancel()
        captureJob = null
        playbackJob?.cancel()
        playbackJob = null
        while (playbackQueue.tryReceive().isSuccess) { /* drain */ }

        sessionReady?.let { if (!it.isCompleted) it.completeExceptionally(IOException("session closed")) }
        sessionReady = null

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

        assistantText.clear()
        userText.clear()
        restoreAudioMode()
    }

    private fun emit(event: RealtimeEvent) {
        if (!_events.tryEmit(event)) {
            LNLog.w(LogCategory.REALTIME, TAG, "event buffer full; dropped ${event::class.simpleName}")
        }
    }

    private inner class BridgeListener : WebSocketListener() {
        override fun onMessage(webSocket: WebSocket, text: String) = handleTextMessage(text)

        override fun onMessage(webSocket: WebSocket, bytes: ByteString) = handleBinaryMessage(bytes)

        override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
            val ready = sessionReady
            if (ready != null && !ready.isCompleted) {
                ready.completeExceptionally(IOException("nova bridge connection failed: ${t.message}", t))
            } else if (_state.value == TransportState.CONNECTED) {
                _state.value = TransportState.FAILED
            }
        }

        override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
            webSocket.close(code, reason)
        }

        override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
            val ready = sessionReady
            if (ready != null && !ready.isCompleted) {
                ready.completeExceptionally(IOException("nova bridge closed before starting ($code)"))
            } else if (_state.value == TransportState.CONNECTED) {
                _state.value = TransportState.CLOSED
            }
        }
    }

    private companion object {
        const val TAG = "NovaBridgeTransport"
        const val SESSION_START_TIMEOUT_MS = 12_000L
        const val DEFAULT_IN_RATE = 16_000
        const val DEFAULT_OUT_RATE = 24_000
    }
}
