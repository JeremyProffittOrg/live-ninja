package ninja.jeremy.liveninja.realtime

import java.io.IOException
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.config.BackendConfig
import ninja.jeremy.liveninja.net.AuthorizedClient
import okhttp3.OkHttpClient
import okhttp3.Request
import org.json.JSONException
import org.json.JSONObject

/**
 * The minted Gemini Live ephemeral credential (gemini-direct mode only, M13;
 * gemini-plan.md §3.4 `accessToken` object).
 */
data class GeminiAccessToken(
    /** Token name (`auth_tokens/…`) — goes in the WSS `?access_token=` query param, URL-encoded. */
    val value: String,
    /**
     * RFC3339 end of the token's message window (~30 min). Past it the client
     * re-fetches the session bootstrap (fresh token) and resumes via handle.
     */
    val expiresAt: String?,
    /** RFC3339 end of the first-connect window (~2 min). */
    val newSessionExpiresAt: String?,
)

/**
 * Result of `GET /api/v1/realtime/session` (backend mirrors the broker's
 * session-mint shape — internal/webapp/api_routes.go handleRealtimeSession).
 */
data class RealtimeSession(
    /**
     * Which transport to use (FR-VE-03), resolved server-side from the
     * device's `voiceEngine` pin. `"openai-direct"` (default / pre-M12 shape)
     * → client-direct WebRTC to OpenAI; `"nova-bridge"` → WebSocket + PCM to
     * the backend Nova Sonic bridge ([NovaBridgeTransport]);
     * `"gemini-direct"` → client-direct WebSocket + base64 PCM to Gemini Live
     * ([GeminiLiveTransport], M13).
     */
    val mode: String = MODE_OPENAI_DIRECT,
    /** Short-lived OpenAI ephemeral client secret (Bearer for the SDP POST). Empty in nova-bridge mode. */
    val clientSecret: String,
    /** RFC3339 expiry of the client secret (~60s TTL). */
    val expiresAt: String?,
    val model: String?,
    val voice: String?,
    val sessionId: String?,
    /** `X-LN-Quota-Warning` header when the user is near their daily cap. */
    val quotaWarning: String?,
    /** Where to POST the SDP offer (openai-direct only). */
    val callsUrl: String = BackendConfig.OPENAI_REALTIME_CALLS_URL,
    /** Nova bridge WebSocket URL (`wss://nova.live.jeremy.ninja/session?...`), nova-bridge only. */
    val wsUrl: String? = null,
    /** Single-use bridge token scoped to this sessionId (nova-bridge only); may already be in [wsUrl]. */
    val bridgeToken: String? = null,
    /**
     * Gemini Live WSS endpoint (gemini-direct only). Deliberately NOT named
     * anything in the wsUrl/bridgeUrl family — legacy clients detect Nova by
     * field presence (gemini-plan.md §3.4).
     */
    val geminiEndpoint: String? = null,
    /** Single-use Gemini ephemeral token (gemini-direct only). */
    val accessToken: GeminiAccessToken? = null,
    /**
     * The exact raw `setup` frame BODY the client must send on the Gemini
     * socket open (gemini-direct only; the same config is locked into the
     * token via liveConnectConstraints server-side).
     */
    val sessionConfig: JSONObject? = null,
) {
    companion object {
        const val MODE_OPENAI_DIRECT = "openai-direct"
        const val MODE_NOVA_BRIDGE = "nova-bridge"
        const val MODE_GEMINI_DIRECT = "gemini-direct"
    }
}

/**
 * Session bootstrap failure with the backend's error taxonomy
 * (contracts/metering.md: `quota_exceeded` 402, `rate_limited` 429; plus
 * `not_authenticated`, `broker_unavailable`, `invalid_response`).
 */
class RealtimeSessionException(
    val kind: String,
    message: String,
    val httpCode: Int,
    val retryAfterSeconds: Int? = null,
) : IOException(message)

/**
 * Fetches the realtime session bootstrap from the Fiber backend. Uses the
 * [AuthorizedClient] OkHttp client, so the session JWT header and the
 * 401 refresh-and-retry are handled by the net layer; a 401 surfacing here
 * means the refresh itself failed (signed out).
 */
@Singleton
class RealtimeSessionApi @Inject constructor(
    @AuthorizedClient private val httpClient: OkHttpClient,
) {

    /**
     * `GET /api/v1/realtime/session`. Throws [RealtimeSessionException] on
     * auth/quota/shape failures and IOException on transport failures.
     */
    suspend fun fetchSession(): RealtimeSession = withContext(Dispatchers.IO) {
        val request = Request.Builder()
            .url(BackendConfig.REALTIME_SESSION_URL)
            .get()
            .build()

        httpClient.newCall(request).execute().use { response ->
            val body = response.body?.string().orEmpty()
            if (!response.isSuccessful) throw errorFrom(response.code, body)

            val json = try {
                JSONObject(body)
            } catch (e: JSONException) {
                throw RealtimeSessionException(
                    kind = "invalid_response",
                    message = "Realtime session response was not JSON.",
                    httpCode = response.code,
                )
            }
            parseSession(json, response.code, response.header("X-LN-Quota-Warning"))
        }
    }

    private fun errorFrom(code: Int, body: String): RealtimeSessionException {
        val json = try {
            JSONObject(body)
        } catch (_: JSONException) {
            null
        }
        val kind = json?.optString("error").orEmpty().ifEmpty {
            if (code == 401) "not_authenticated" else "http_$code"
        }
        val message = json?.optString("message").orEmpty().ifEmpty {
            when (code) {
                401 -> "Your session has expired. Sign in again."
                402 -> "Daily voice budget reached. It resets at midnight UTC."
                429 -> "Too many session starts. Wait a moment and retry."
                else -> "Realtime session request failed (HTTP $code)."
            }
        }
        val retryAfter = json?.optInt("retryAfterSeconds", 0)?.takeIf { it > 0 }
        return RealtimeSessionException(kind, message, code, retryAfter)
    }

    companion object {
        /**
         * Maps a success body onto [RealtimeSession], enforcing the per-mode
         * required fields (FR-VE-03 + M13). Pure org.json so it is
         * unit-testable without a mock server. Three valid success shapes; a
         * missing `mode` is the pre-M12 openai-direct shape.
         */
        fun parseSession(json: JSONObject, httpCode: Int, quotaWarning: String?): RealtimeSession {
            val mode = json.optString("mode").ifEmpty { RealtimeSession.MODE_OPENAI_DIRECT }
            val secret = json.optJSONObject("clientSecret")
            val value = secret?.optString("value").orEmpty()
            val wsUrl = json.optString("wsUrl").ifEmpty { null }
            val geminiEndpoint = json.optString("geminiEndpoint").ifEmpty { null }
            val accessToken = json.optJSONObject("accessToken")?.let { tok ->
                GeminiAccessToken(
                    value = tok.optString("value"),
                    expiresAt = tok.optString("expiresAt").ifEmpty { null },
                    newSessionExpiresAt = tok.optString("newSessionExpiresAt").ifEmpty { null },
                )
            }
            val sessionConfig = json.optJSONObject("sessionConfig")

            when (mode) {
                RealtimeSession.MODE_NOVA_BRIDGE -> {
                    if (wsUrl == null) {
                        throw RealtimeSessionException(
                            kind = "invalid_response",
                            message = "Nova bridge session response is missing wsUrl.",
                            httpCode = httpCode,
                        )
                    }
                }

                RealtimeSession.MODE_GEMINI_DIRECT -> {
                    if (geminiEndpoint == null || accessToken == null ||
                        accessToken.value.isEmpty() || sessionConfig == null
                    ) {
                        throw RealtimeSessionException(
                            kind = "invalid_response",
                            message = "Gemini session response is missing " +
                                "geminiEndpoint/accessToken.value/sessionConfig.",
                            httpCode = httpCode,
                        )
                    }
                }

                else -> if (value.isEmpty()) {
                    throw RealtimeSessionException(
                        kind = "invalid_response",
                        message = "Realtime session response is missing clientSecret.value.",
                        httpCode = httpCode,
                    )
                }
            }
            return RealtimeSession(
                mode = mode,
                clientSecret = value,
                expiresAt = secret?.optString("expiresAt")?.ifEmpty { null },
                model = json.optString("model").ifEmpty { null },
                voice = json.optString("voice").ifEmpty { null },
                sessionId = json.optString("sessionId").ifEmpty { null },
                quotaWarning = quotaWarning,
                wsUrl = wsUrl,
                bridgeToken = json.optString("token").ifEmpty { null },
                geminiEndpoint = geminiEndpoint,
                accessToken = accessToken,
                sessionConfig = sessionConfig,
            )
        }
    }
}
