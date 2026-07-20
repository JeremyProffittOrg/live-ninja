package ninja.jeremy.liveninja.realtime

import android.content.Context
import android.media.AudioDeviceInfo
import android.media.AudioManager
import android.os.Build
import android.util.Log
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.IOException
import java.nio.ByteBuffer
import java.nio.charset.StandardCharsets
import javax.inject.Inject
import javax.inject.Singleton
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject
import org.webrtc.AudioSource
import org.webrtc.AudioTrack
import org.webrtc.DataChannel
import org.webrtc.IceCandidate
import org.webrtc.MediaConstraints
import org.webrtc.MediaStream
import org.webrtc.PeerConnection
import org.webrtc.PeerConnectionFactory
import org.webrtc.RtpReceiver
import org.webrtc.RtpTransceiver
import org.webrtc.SdpObserver
import org.webrtc.SessionDescription
import org.webrtc.audio.JavaAudioDeviceModule

/**
 * WebRTC implementation of [RealtimeTransport] on the prebuilt
 * io.github.webrtc-sdk:android artifact (plan.md M4, Android §4).
 *
 * Media path: mic (VOICE_COMMUNICATION source, hardware+software AEC/NS/AGC)
 * -> PeerConnection audio m-line -> OpenAI; assistant audio arrives on the
 * remote track and plays through the voice-call stream
 * (MODE_IN_COMMUNICATION, routed to the built-in speaker).
 *
 * Signaling: local SDP offer POSTed as `application/sdp` to
 * https://api.openai.com/v1/realtime/calls with the ephemeral client secret
 * as Bearer; the response body is the SDP answer (no trickle ICE — we wait
 * for gathering, bounded, before POSTing).
 *
 * Events: the `oai-events` DataChannel carries JSON server events, parsed by
 * [RealtimeEventParser] and republished on [events]. Barge-in is handled
 * here (Android §4.3): on `input_audio_buffer.speech_started` while the
 * assistant is speaking -> `response.cancel`, ~40 ms remote-track fade, then
 * `output_audio_buffer.clear` to flush the server-side jitter/playout buffer.
 */
@Singleton
class WebRtcTransport @Inject constructor(
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

    private var factory: PeerConnectionFactory? = null
    private var peerConnection: PeerConnection? = null
    private var audioSource: AudioSource? = null
    private var localAudioTrack: AudioTrack? = null

    @Volatile
    private var remoteAudioTrack: AudioTrack? = null
    private var dataChannel: DataChannel? = null

    private var iceGatheringComplete: CompletableDeferred<Unit>? = null
    private var peerConnected: CompletableDeferred<Unit>? = null

    @Volatile
    private var assistantSpeaking = false

    @Volatile
    private var userMuted = false
    private var fadeJob: Job? = null

    private var previousAudioMode = AudioManager.MODE_NORMAL
    private var previousSpeakerphone = false

    override suspend fun connect(ephemeralToken: String, callsUrl: String) {
        connectMutex.withLock {
            check(_state.value != TransportState.CONNECTING && _state.value != TransportState.CONNECTED) {
                "transport already ${_state.value}"
            }
            _state.value = TransportState.CONNECTING
            try {
                withContext(Dispatchers.Default) { doConnect(ephemeralToken, callsUrl) }
                _state.value = TransportState.CONNECTED
            } catch (t: Throwable) {
                releaseSession()
                _state.value = TransportState.FAILED
                throw t
            }
        }
    }

    private suspend fun doConnect(ephemeralToken: String, callsUrl: String) {
        ensureFactory()
        configureAudioForCall()

        val gathering = CompletableDeferred<Unit>()
        val connected = CompletableDeferred<Unit>()
        iceGatheringComplete = gathering
        peerConnected = connected

        val rtcConfig = PeerConnection.RTCConfiguration(emptyList()).apply {
            sdpSemantics = PeerConnection.SdpSemantics.UNIFIED_PLAN
        }
        val pc = requireNotNull(factory).createPeerConnection(rtcConfig, PcObserver())
            ?: throw IOException("createPeerConnection returned null")
        peerConnection = pc

        // Mic capture with software AEC/NS/AGC constraints layered on top of
        // the hardware effects the audio device module enables.
        val audioConstraints = MediaConstraints().apply {
            mandatory.add(MediaConstraints.KeyValuePair("googEchoCancellation", "true"))
            mandatory.add(MediaConstraints.KeyValuePair("googNoiseSuppression", "true"))
            mandatory.add(MediaConstraints.KeyValuePair("googAutoGainControl", "true"))
            mandatory.add(MediaConstraints.KeyValuePair("googHighpassFilter", "true"))
        }
        val source = requireNotNull(factory).createAudioSource(audioConstraints)
        audioSource = source
        val micTrack = requireNotNull(factory).createAudioTrack("liveninja-mic", source)
        localAudioTrack = micTrack
        pc.addTrack(micTrack, listOf("liveninja"))

        // Client-created events channel; OpenAI attaches to the same label.
        val dc = pc.createDataChannel("oai-events", DataChannel.Init())
            ?: throw IOException("createDataChannel returned null")
        dataChannel = dc
        dc.registerObserver(DcObserver(dc))

        val offerConstraints = MediaConstraints().apply {
            mandatory.add(MediaConstraints.KeyValuePair("OfferToReceiveAudio", "true"))
            mandatory.add(MediaConstraints.KeyValuePair("OfferToReceiveVideo", "false"))
        }
        val offer = awaitCreateOffer(pc, offerConstraints)
        awaitSetDescription { observer -> pc.setLocalDescription(observer, offer) }

        // No trickle over the HTTP signaling exchange: give ICE a bounded
        // window to gather host/srflx candidates into the local SDP.
        try {
            withTimeout(ICE_GATHERING_TIMEOUT_MS) { gathering.await() }
        } catch (_: TimeoutCancellationException) {
            Log.w(TAG, "ICE gathering incomplete after ${ICE_GATHERING_TIMEOUT_MS}ms; sending offer as-is")
        }

        val localSdp = pc.localDescription?.description
            ?: throw IOException("local description missing after ICE gathering")
        val answerSdp = postSdpOffer(callsUrl, ephemeralToken, localSdp)
        awaitSetDescription { observer ->
            pc.setRemoteDescription(observer, SessionDescription(SessionDescription.Type.ANSWER, answerSdp))
        }

        try {
            withTimeout(CONNECT_TIMEOUT_MS) { connected.await() }
        } catch (_: TimeoutCancellationException) {
            throw IOException("peer connection did not reach CONNECTED within ${CONNECT_TIMEOUT_MS}ms")
        }
    }

    private suspend fun postSdpOffer(callsUrl: String, token: String, sdp: String): String =
        withContext(Dispatchers.IO) {
            val request = Request.Builder()
                .url(callsUrl)
                .header("Authorization", "Bearer $token")
                .post(sdp.toRequestBody("application/sdp".toMediaType()))
                .build()
            httpClient.newCall(request).execute().use { response ->
                val body = response.body?.string().orEmpty()
                if (!response.isSuccessful) {
                    throw IOException("realtime calls SDP exchange failed: HTTP ${response.code} ${body.take(300)}")
                }
                if (!body.contains("v=0")) {
                    throw IOException("realtime calls response is not SDP: ${body.take(300)}")
                }
                body
            }
        }

    override fun sendEvent(event: JSONObject) {
        val dc = dataChannel ?: return
        runCatching {
            if (dc.state() == DataChannel.State.OPEN) {
                val bytes = event.toString().toByteArray(StandardCharsets.UTF_8)
                dc.send(DataChannel.Buffer(ByteBuffer.wrap(bytes), false))
            }
        }.onFailure { Log.w(TAG, "sendEvent failed", it) }
    }

    override fun setMicMuted(muted: Boolean) {
        userMuted = muted
        updateMicEnabled()
    }

    override fun stopPlayback() {
        interruptPlayback(manual = true)
    }

    override suspend fun disconnect() {
        connectMutex.withLock {
            releaseSession()
            if (_state.value != TransportState.IDLE) {
                _state.value = TransportState.CLOSED
            }
        }
    }

    // ---- barge-in ----

    /**
     * `response.cancel` immediately, fade the remote track to silence over
     * ~[FADE_MS] (avoids the audible click of a hard cut), then
     * `output_audio_buffer.clear` to flush audio still queued server-side.
     */
    private fun interruptPlayback(manual: Boolean) {
        if (!assistantSpeaking && !manual) return
        assistantSpeaking = false
        sendEvent(JSONObject().put("type", "response.cancel"))
        fadeJob?.cancel()
        fadeJob = scope.launch {
            val track = remoteAudioTrack
            if (track != null) {
                val steps = FADE_STEPS
                for (i in 1..steps) {
                    runCatching { track.setVolume(NOMINAL_VOLUME * (steps - i) / steps) }
                    delay(FADE_MS / steps)
                }
            }
            sendEvent(JSONObject().put("type", "output_audio_buffer.clear"))
            updateMicEnabled()
        }
    }

    private fun restoreVolume() {
        fadeJob?.cancel()
        runCatching { remoteAudioTrack?.setVolume(NOMINAL_VOLUME) }
    }

    /** Mic is live only when not user-muted and not half-duplex-gated. */
    private fun updateMicEnabled() {
        val enabled = !userMuted && !(halfDuplex && assistantSpeaking)
        runCatching { localAudioTrack?.setEnabled(enabled) }
    }

    // ---- server event routing ----

    private fun handleServerEvent(raw: String) {
        val event = RealtimeEventParser.parse(raw) ?: return
        when (event) {
            is RealtimeEvent.SpeechStarted -> interruptPlayback(manual = false)

            is RealtimeEvent.AssistantAudioStarted -> {
                assistantSpeaking = true
                restoreVolume()
                updateMicEnabled()
            }

            is RealtimeEvent.AssistantAudioStopped,
            is RealtimeEvent.ResponseDone,
            -> {
                assistantSpeaking = false
                updateMicEnabled()
                restoreVolume()
            }

            else -> Unit
        }
        if (!_events.tryEmit(event)) {
            Log.w(TAG, "event buffer full; dropped ${event::class.simpleName}")
        }
    }

    // ---- lifecycle plumbing ----

    private fun ensureFactory() {
        if (factory != null) return
        PeerConnectionFactory.initialize(
            PeerConnectionFactory.InitializationOptions.builder(context)
                .createInitializationOptions(),
        )
        val adm = JavaAudioDeviceModule.builder(context)
            .setUseHardwareAcousticEchoCanceler(true)
            .setUseHardwareNoiseSuppressor(true)
            .createAudioDeviceModule()
        factory = PeerConnectionFactory.builder()
            .setAudioDeviceModule(adm)
            .createPeerConnectionFactory()
        adm.release()
    }

    private fun configureAudioForCall() {
        val am = context.getSystemService(AudioManager::class.java) ?: return
        previousAudioMode = am.mode
        am.mode = AudioManager.MODE_IN_COMMUNICATION
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            // Prefer BT SCO → wired → speaker instead of forcing the built-in
            // speaker (02-voice §B3): route to the best available headset.
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

    private fun releaseSession() {
        fadeJob?.cancel()
        fadeJob = null
        assistantSpeaking = false
        userMuted = false
        iceGatheringComplete = null
        peerConnected = null

        dataChannel?.let { dc ->
            runCatching { dc.unregisterObserver() }
            runCatching { dc.close() }
            runCatching { dc.dispose() }
        }
        dataChannel = null

        remoteAudioTrack = null
        peerConnection?.let { pc ->
            runCatching { pc.close() }
            runCatching { pc.dispose() }
        }
        peerConnection = null

        localAudioTrack?.let { runCatching { it.dispose() } }
        localAudioTrack = null
        audioSource?.let { runCatching { it.dispose() } }
        audioSource = null

        restoreAudioMode()
    }

    // ---- webrtc observers ----

    private inner class PcObserver : PeerConnection.Observer {
        override fun onIceGatheringChange(state: PeerConnection.IceGatheringState?) {
            if (state == PeerConnection.IceGatheringState.COMPLETE) {
                iceGatheringComplete?.complete(Unit)
            }
        }

        override fun onConnectionChange(newState: PeerConnection.PeerConnectionState?) {
            when (newState) {
                PeerConnection.PeerConnectionState.CONNECTED -> peerConnected?.complete(Unit)
                PeerConnection.PeerConnectionState.FAILED -> {
                    peerConnected?.completeExceptionally(IOException("peer connection failed"))
                    if (_state.value == TransportState.CONNECTED) {
                        _state.value = TransportState.FAILED
                    }
                }
                PeerConnection.PeerConnectionState.CLOSED -> {
                    if (_state.value == TransportState.CONNECTED) {
                        _state.value = TransportState.CLOSED
                    }
                }
                else -> Unit
            }
        }

        override fun onTrack(transceiver: RtpTransceiver?) {
            val track = transceiver?.receiver?.track()
            if (track is AudioTrack) {
                remoteAudioTrack = track
                runCatching { track.setVolume(NOMINAL_VOLUME) }
            }
        }

        override fun onAddTrack(receiver: RtpReceiver?, streams: Array<out MediaStream>?) {
            val track = receiver?.track()
            if (track is AudioTrack) {
                remoteAudioTrack = track
                runCatching { track.setVolume(NOMINAL_VOLUME) }
            }
        }

        override fun onDataChannel(channel: DataChannel?) {
            // We create `oai-events` ourselves; a remotely-announced channel
            // (if any) is observed with the same handler for completeness.
            channel?.registerObserver(DcObserver(channel))
        }

        override fun onSignalingChange(state: PeerConnection.SignalingState?) {}
        override fun onIceConnectionChange(state: PeerConnection.IceConnectionState?) {}
        override fun onIceConnectionReceivingChange(receiving: Boolean) {}
        override fun onIceCandidate(candidate: IceCandidate?) {}
        override fun onIceCandidatesRemoved(candidates: Array<out IceCandidate>?) {}
        override fun onAddStream(stream: MediaStream?) {}
        override fun onRemoveStream(stream: MediaStream?) {}
        override fun onRenegotiationNeeded() {}
    }

    private inner class DcObserver(private val channel: DataChannel) : DataChannel.Observer {
        override fun onMessage(buffer: DataChannel.Buffer?) {
            val data = buffer?.data ?: return
            val bytes = ByteArray(data.remaining())
            data.get(bytes)
            if (buffer.binary) return
            handleServerEvent(String(bytes, StandardCharsets.UTF_8))
        }

        override fun onStateChange() {
            Log.d(TAG, "oai-events channel state: ${runCatching { channel.state() }.getOrNull()}")
        }

        override fun onBufferedAmountChange(previousAmount: Long) {}
    }

    // ---- sdp helpers ----

    private suspend fun awaitCreateOffer(
        pc: PeerConnection,
        constraints: MediaConstraints,
    ): SessionDescription = suspendCancellableCoroutine { cont ->
        pc.createOffer(
            object : SdpObserver {
                override fun onCreateSuccess(sdp: SessionDescription?) {
                    if (sdp == null) {
                        cont.resumeWithException(IOException("createOffer produced null SDP"))
                    } else {
                        cont.resume(sdp)
                    }
                }

                override fun onCreateFailure(error: String?) {
                    cont.resumeWithException(IOException("createOffer failed: $error"))
                }

                override fun onSetSuccess() {}
                override fun onSetFailure(error: String?) {}
            },
            constraints,
        )
    }

    private suspend fun awaitSetDescription(
        apply: (SdpObserver) -> Unit,
    ): Unit = suspendCancellableCoroutine { cont ->
        apply(
            object : SdpObserver {
                override fun onSetSuccess() {
                    cont.resume(Unit)
                }

                override fun onSetFailure(error: String?) {
                    cont.resumeWithException(IOException("setDescription failed: $error"))
                }

                override fun onCreateSuccess(sdp: SessionDescription?) {}
                override fun onCreateFailure(error: String?) {}
            },
        )
    }

    private companion object {
        const val TAG = "WebRtcTransport"
        const val ICE_GATHERING_TIMEOUT_MS = 2_000L
        const val CONNECT_TIMEOUT_MS = 15_000L

        /** Barge-in fade duration (plan.md Android §4.3: 30–50 ms). */
        const val FADE_MS = 40L
        const val FADE_STEPS = 5

        /** libwebrtc AudioTrack volume scale is 0..10; 1.0 is nominal. */
        const val NOMINAL_VOLUME = 1.0

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
