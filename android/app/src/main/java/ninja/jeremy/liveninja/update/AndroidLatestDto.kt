package ninja.jeremy.liveninja.update

import kotlinx.serialization.Serializable

/** Wire shape of public `GET /v1/app/android/latest`. */
@Serializable
data class AndroidLatestDto(
    val schemaVersion: Int,
    val packageName: String,
    val versionName: String,
    val versionCode: Long,
    val url: String,
    val sha256: String,
    val sizeBytes: Long,
    val certificateSha256: String = "",
    val publishedAt: String = "",
    val gitSha: String = "",
)
