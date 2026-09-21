package ninja.jeremy.liveninja.realtime

import android.content.Context
import android.net.Uri
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.IOException
import java.util.concurrent.TimeUnit
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.withTimeout
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import org.json.JSONObject

/**
 * Azure Voice Live transport (azure-voice-plan.md E3): WebRTC media with WSS
 * signaling. Same peer connection as [WebRtcTransport]; the SDP offer is sent
 * as `rtc.call.sdp.create` on a control socket instead of HTTP POST.
 */
@Singleton
class VoiceLiveTransport @Inject constructor(
    @ApplicationContext context: Context,
    httpClient: OkHttpClient,
    echoGuard: EchoGuardPolicy,
) : WebRtcTransport(context, httpClient, echoGuard) {

    private var controlSocket: WebSocket? = null
    private var sessionConfig: JSONObject? = null

    override val eventsChannelLabel: String = "voice-live-events"

    override fun prime(session: RealtimeSession) {
        sessionConfig = session.sessionConfig
    }

    override suspend fun postSdpOffer(callsUrl: String, token: String, sdp: String): String {
        val answer = CompletableDeferred<String>()
        val url = Uri.parse(callsUrl).buildUpon()
            .appendQueryParameter("Authorization", "Bearer $token")
            .build()
            .toString()
        val request = Request.Builder().url(url).build()
        val listener = object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                val payload = JSONObject()
                    .put("type", "rtc.call.sdp.create")
                    .put("sdp_offer", sdp)
                    .put("session", sessionConfig ?: JSONObject())
                webSocket.send(payload.toString())
            }

            override fun onMessage(webSocket: WebSocket, text: String) {
                val json = runCatching { JSONObject(text) }.getOrNull() ?: return
                val type = json.optString("type")
                val sdpAnswer = json.optString("sdp_answer").ifEmpty {
                    json.optString("sdp")
                }
                if (type == "error") {
                    answer.completeExceptionally(
                        IOException(json.optString("message").ifEmpty { "Voice Live rejected the session" }),
                    )
                    return
                }
                if (type == "rtc.call.sdp.created" || sdpAnswer.contains("v=0")) {
                    if (!answer.isCompleted) answer.complete(sdpAnswer)
                }
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                if (!answer.isCompleted) {
                    answer.completeExceptionally(IOException("Voice Live socket failed", t))
                }
            }

            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                if (!answer.isCompleted) {
                    answer.completeExceptionally(IOException("Voice Live closed before SDP answer"))
                }
            }
        }
        controlSocket = httpClient.newWebSocket(request, listener)
        return try {
            withTimeout(15_000) { answer.await() }
        } catch (t: Throwable) {
            controlSocket?.cancel()
            controlSocket = null
            throw IOException("Voice Live SDP exchange failed", t)
        }
    }

    override fun releaseSession() {
        controlSocket?.cancel()
        controlSocket = null
        sessionConfig = null
        super.releaseSession()
    }

    companion object {
        @Suppress("unused")
        private const val TAG = "VoiceLiveTransport"
    }
}
