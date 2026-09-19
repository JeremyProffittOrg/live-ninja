package ninja.jeremy.liveninja.update

import java.net.URI

/**
 * Trust rules for `GET /v1/app/android/latest`. Mirrors
 * internal/webapp/android_distribution_routes.go validateAndroidLatest
 * so a compromised or stale document cannot point the installer at a
 * foreign host.
 */
object AndroidReleasePolicy {
    const val PACKAGE_NAME = "ninja.jeremy.liveninja"
    const val HOST = "live.jeremy.ninja"
    const val PATH_PREFIX = "/static/models/downloads/"

    fun isNewer(remoteVersionCode: Long, installedVersionCode: Int): Boolean =
        remoteVersionCode > installedVersionCode.toLong()

    fun isTrustedApkUrl(url: String): Boolean {
        val uri = runCatching { URI(url) }.getOrNull() ?: return false
        if (uri.scheme != "https") return false
        if (uri.host != HOST) return false
        if (!uri.userInfo.isNullOrEmpty()) return false
        val path = uri.path ?: return false
        if (!path.startsWith(PATH_PREFIX) || !path.endsWith(".apk")) return false
        if (!uri.query.isNullOrEmpty() || !uri.fragment.isNullOrEmpty()) return false
        return true
    }

    fun sha256MatchesUrl(url: String, sha256: String): Boolean {
        val path = runCatching { URI(url).path }.getOrNull() ?: return false
        return path.endsWith("-${sha256.lowercase()}.apk")
    }
}
