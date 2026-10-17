package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull

@Serializable
data class PhoneRecordingDto(
    val callSid: String = "",
    val requestId: String = "",
    val target: String = "",
    val recordingStatus: String = "pending",
    val reason: String = "",
    val providerStatus: String = "",
    val updatedAt: Long = 0,
    val audioBytes: Long = 0,
) {
    val ready: Boolean get() = recordingStatus == "ready" && Regex("CA[0-9a-fA-F]{32}").matches(callSid)
    val id: String get() = requestId.ifBlank { callSid }
}

@Serializable
data class PhoneRecordingsResponse(
    val items: List<PhoneRecordingDto> = emptyList(),
    val source: String,
    val sources: List<String> = emptyList(),
    val nextCursor: String? = null,
)

@Serializable
data class PhoneRecordingMedia(val url: String, val expiresIn: Int)

/** Media fetches carry only the short-lived S3 signature, never backend credentials. */
internal fun verifiedRecordingUrl(raw: String): String {
    val url = raw.toHttpUrlOrNull() ?: error("Invalid recording URL")
    require(url.isHttps && url.port == 443 && url.username.isEmpty() && url.password.isEmpty())
    require(url.host in setOf("live-ninja-deliverables-759775734231.s3.amazonaws.com", "live-ninja-deliverables-759775734231.s3.us-east-1.amazonaws.com"))
    require(url.encodedPath.startsWith("/phone-trial/") && !url.query.isNullOrBlank() && url.fragment == null)
    return url.toString()
}
