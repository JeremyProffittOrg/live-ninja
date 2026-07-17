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
 * Result of `GET /api/v1/realtime/session` (backend mirrors the broker's
 * session-mint shape — internal/webapp/api_routes.go handleRealtimeSession).
 */
data class RealtimeSession(
    /** Short-lived OpenAI ephemeral client secret (Bearer for the SDP POST). */
    val clientSecret: String,
    /** RFC3339 expiry of the client secret (~60s TTL). */
    val expiresAt: String?,
    val model: String?,
    val voice: String?,
    val sessionId: String?,
    /** `X-LN-Quota-Warning` header when the user is near their daily cap. */
    val quotaWarning: String?,
    /** Where to POST the SDP offer. */
    val callsUrl: String = BackendConfig.OPENAI_REALTIME_CALLS_URL,
)

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
            val secret = json.optJSONObject("clientSecret")
            val value = secret?.optString("value").orEmpty()
            if (value.isEmpty()) {
                throw RealtimeSessionException(
                    kind = "invalid_response",
                    message = "Realtime session response is missing clientSecret.value.",
                    httpCode = response.code,
                )
            }
            RealtimeSession(
                clientSecret = value,
                expiresAt = secret.optString("expiresAt").ifEmpty { null },
                model = json.optString("model").ifEmpty { null },
                voice = json.optString("voice").ifEmpty { null },
                sessionId = json.optString("sessionId").ifEmpty { null },
                quotaWarning = response.header("X-LN-Quota-Warning"),
            )
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
}
