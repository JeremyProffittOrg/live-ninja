package ninja.jeremy.liveninja.realtime

import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import org.json.JSONObject

/** Connection lifecycle of a realtime voice session. */
enum class TransportState {
    IDLE,
    CONNECTING,
    CONNECTED,
    FAILED,
    CLOSED,
}

/**
 * Contract for the realtime media transport (plan.md M4, Android §4).
 *
 * Default implementation: [WebRtcTransport] via the prebuilt
 * io.github.webrtc-sdk:android artifact — SDP offer POSTed to
 * https://api.openai.com/v1/realtime/calls with the ephemeral token (minted by
 * GET /api/v1/realtime/session) as the Bearer, mic captured in
 * MODE_IN_COMMUNICATION with platform+WebRTC AEC/NS/AGC.
 * The interface keeps callers (wake service, conversation UI) independent of
 * the vendor library so an alternative transport (e.g. the M12 Nova Sonic
 * bridge WebSocket) can slot in behind it.
 */
interface RealtimeTransport {
    /** Observable connection state. */
    val state: StateFlow<TransportState>

    /**
     * Parsed server events from the `oai-events` DataChannel. Hot; emissions
     * are dropped when nobody collects fast enough (buffered 256 deep).
     */
    val events: SharedFlow<RealtimeEvent>

    /**
     * Half-duplex fallback (plan.md Android §4.3): when true the mic track is
     * disabled while the assistant is speaking, for devices whose AEC cannot
     * prevent self-triggered barge-in. Takes effect from the next speaking
     * transition; safe to flip mid-session.
     */
    var halfDuplex: Boolean

    /**
     * Open a session: negotiate SDP with [callsUrl] using [ephemeralToken] and
     * begin bidirectional audio. Returns when the peer connection is CONNECTED
     * or throws on negotiation failure.
     */
    suspend fun connect(ephemeralToken: String, callsUrl: String)

    /** Send a client event JSON on the `oai-events` DataChannel (no-op when not open). */
    fun sendEvent(event: JSONObject)

    /**
     * User-facing mute of the outgoing mic track. Independent of the
     * half-duplex gating (both must allow audio for the track to be live).
     */
    fun setMicMuted(muted: Boolean)

    /**
     * Interrupt assistant playback (barge-in / manual stop): sends
     * `response.cancel`, fades the remote track over ~40 ms, then flushes the
     * server's buffered audio with `output_audio_buffer.clear`.
     */
    fun stopPlayback()

    /** Close the session and release the peer connection/audio devices. */
    suspend fun disconnect()
}
