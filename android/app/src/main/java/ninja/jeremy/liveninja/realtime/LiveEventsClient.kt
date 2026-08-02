package ninja.jeremy.liveninja.realtime

import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
import ninja.jeremy.liveninja.net.IotCredentials
import ninja.jeremy.liveninja.net.LiveNinjaApi
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.ByteString
import org.json.JSONObject

/**
 * Cross-device change notifications over MQTT (plan.md §6 WS-4).
 *
 * The Android half of what liveevents.mjs does on web: subscribe to this
 * user's event topic so a document, memory entity or plan changed on another
 * device is known here immediately.
 *
 * Built on OkHttp's WebSocket plus [MqttCodec] rather than the AWS IoT SDK —
 * see MqttCodec's own note on why another native dependency is the wrong trade
 * for this device family.
 *
 * Two rules this class exists to hold:
 *  - **It never reports the user's own edits.** The server stamps each event
 *    with the device that caused it, and the credential response tells this
 *    client what that value will be for itself.
 *  - **It is not always-on.** The connection is opened while the app is
 *    actually in use and closed otherwise. Samsung's One UI kills long-lived
 *    background sockets anyway, and the reconnect churn costs more battery
 *    than the notification latency is worth.
 */
@Singleton
class LiveEventsClient @Inject constructor(
    private val api: LiveNinjaApi,
    private val http: OkHttpClient,
) {
    /** One change another device made. */
    data class Change(
        val type: String,
        val id: String,
        val actorDeviceId: String,
        val actorPersona: String,
        val summary: String,
    )

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private var socket: WebSocket? = null
    private var job: Job? = null
    private var reader = MqttCodec.Reader()
    private var creds: IotCredentials? = null
    private var pingJob: Job? = null
    private var running = false

    private val _changes = MutableStateFlow<Change?>(null)

    /** Latest change from ANOTHER device; null until one arrives. */
    val changes: StateFlow<Change?> = _changes

    /** Idempotent: a second start() while connected does nothing. */
    fun start() {
        if (running) return
        running = true
        job = scope.launch { connect() }
    }

    /** Clears presence deliberately, so peers see a departure rather than a crash. */
    fun stop() {
        running = false
        pingJob?.cancel()
        job?.cancel()
        creds?.let { c ->
            runCatching {
                socket?.send(ByteString.of(*MqttCodec.encodePublish(c.presenceTopic, "", retain = true)))
                socket?.send(ByteString.of(*MqttCodec.encodeDisconnect()))
            }
        }
        socket?.close(1000, null)
        socket = null
        creds = null
    }

    private suspend fun connect() {
        val c = runCatching { api.iotCredentials() }.getOrElse {
            // Signed out, offline, or the feature is not configured. This is a
            // convenience layer: it goes quiet rather than retrying forever.
            LNLog.i(LogCategory.NET, TAG, "iot credentials unavailable; cross-device events off")
            running = false
            return
        }
        if (c.endpoint.isEmpty() || c.token.isEmpty()) {
            running = false
            return
        }
        creds = c
        reader = MqttCodec.Reader()

        // AWS IoT takes the authorizer name from the query string and the token
        // from the MQTT CONNECT user-name field.
        val url = "wss://${c.endpoint}/mqtt?x-amz-customauthorizer-name=${c.authorizerName}"
        val request = Request.Builder()
            .url(url)
            // The subprotocol AWS IoT requires on the handshake.
            .addHeader("Sec-WebSocket-Protocol", "mqtt")
            .build()

        socket = http.newWebSocket(request, object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                webSocket.send(
                    ByteString.of(
                        *MqttCodec.encodeConnect(
                            clientId = c.clientId,
                            username = c.token,
                            will = MqttCodec.Will(c.presenceTopic),
                        ),
                    ),
                )
            }

            override fun onMessage(webSocket: WebSocket, bytes: ByteString) {
                for (pkt in reader.push(bytes)) onPacket(webSocket, pkt, c)
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                LNLog.w(LogCategory.NET, TAG, "live events socket failed", t)
                scheduleReconnect()
            }

            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                scheduleReconnect()
            }
        })
    }

    private fun onPacket(ws: WebSocket, pkt: MqttCodec.Packet, c: IotCredentials) {
        when (pkt.type) {
            MqttCodec.CONNACK -> {
                val code = pkt.body.getOrNull(1)?.toInt() ?: -1
                if (code != 0) {
                    LNLog.w(LogCategory.NET, TAG, "live events connection refused (code $code)")
                    running = false
                    ws.close(1000, null)
                    return
                }
                ws.send(ByteString.of(*MqttCodec.encodeSubscribe(1, listOf(c.topicFilter))))
                ws.send(
                    ByteString.of(
                        *MqttCodec.encodePublish(
                            c.presenceTopic,
                            JSONObject().put("deviceId", c.actorDeviceId).toString(),
                            retain = true,
                        ),
                    ),
                )
                startPing(ws)
                scheduleRefresh(c)
            }

            MqttCodec.PUBLISH -> {
                val pub = MqttCodec.decodePublish(pkt.body)
                if (pub.topic.contains("/presence/")) return // presence is peers, not changes
                val json = runCatching { JSONObject(pub.payload) }.getOrNull() ?: return
                val actor = json.optString("actorDeviceId")
                // The comparison that stops this device announcing its own edit.
                if (actor.isNotEmpty() && actor == c.actorDeviceId) return
                _changes.value = Change(
                    type = json.optString("type"),
                    id = json.optString("id"),
                    actorDeviceId = actor,
                    actorPersona = json.optString("actorPersona"),
                    summary = json.optString("summary"),
                )
            }
        }
    }

    private fun startPing(ws: WebSocket) {
        pingJob?.cancel()
        pingJob = scope.launch {
            // Half the 60s keep-alive: the broker disconnects at 1.5x, so this
            // leaves room for one lost ping without losing the session.
            while (running) {
                delay(30_000)
                runCatching { ws.send(ByteString.of(*MqttCodec.encodePingreq())) }
            }
        }
    }

    private fun scheduleRefresh(c: IotCredentials) {
        scope.launch {
            // Reconnect BEFORE the token expires. Closing routes through the
            // normal reconnect path, so there is one reconnect implementation
            // rather than two.
            delay(maxOf(30_000L, c.expiresInSeconds * 1000L - 60_000L))
            if (running) socket?.close(1000, "token refresh")
        }
    }

    private fun scheduleReconnect() {
        if (!running) return
        scope.launch {
            delay(2_000)
            if (running) connect()
        }
    }

    private companion object {
        const val TAG = "LiveEventsClient"
    }
}
