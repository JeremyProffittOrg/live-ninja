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
    const val LATEST_URL = "https://live.jeremy.ninja/v1/app/android/latest"
    const val MAX_APK_BYTES = 512L * 1024 * 1024
    private val SHA256 = Regex("^[0-9a-fA-F]{64}$")

    fun isNewer(remoteVersionCode: Long, installedVersionCode: Int): Boolean =
        remoteVersionCode > installedVersionCode.toLong()

    fun isTrustedApkUrl(url: String): Boolean {
        val uri = runCatching { URI(url) }.getOrNull() ?: return false
        if (uri.scheme != "https") return false
        if (uri.host != HOST) return false
        if (uri.port != -1 && uri.port != 443) return false
        if (!uri.userInfo.isNullOrEmpty()) return false
        val path = uri.path ?: return false
        if (!path.startsWith(PATH_PREFIX) || !path.endsWith(".apk")) return false
        if (uri.rawPath != path || uri.normalize().path != path) return false
        if (path.removePrefix(PATH_PREFIX).contains('/') || path.contains('\\')) return false
        if (uri.query != null || uri.fragment != null) return false
        return true
    }

    fun metadataError(release: AndroidLatestDto): String? = when {
        release.schemaVersion != 1 -> "unsupported release schema"
        release.packageName != PACKAGE_NAME -> "release package is not this app"
        release.versionCode <= 0 || release.versionName.isBlank() || release.versionName.length > 100 -> "invalid release version"
        release.sizeBytes !in 1..MAX_APK_BYTES -> "invalid APK size"
        !SHA256.matches(release.sha256) -> "malformed sha256"
        !SHA256.matches(release.certificateSha256) -> "missing or malformed signing certificate"
        !isTrustedApkUrl(release.url) -> "untrusted APK URL"
        !sha256MatchesUrl(release.url, release.sha256) -> "APK URL is not content-addressed"
        else -> null
    }

    /** Deliberately requires current signer equality; key rotation needs a separate migration. */
    fun archiveError(release: AndroidLatestDto, packageName: String, versionCode: Long,
                     apkSigners: Set<String>, installedSigners: Set<String>): String? = when {
        packageName != PACKAGE_NAME -> "APK package mismatch"
        versionCode != release.versionCode -> "APK version mismatch"
        apkSigners.size != 1 || installedSigners.size != 1 -> "unsupported or missing APK signer"
        apkSigners.single().lowercase() != release.certificateSha256.lowercase() -> "APK certificate mismatch"
        apkSigners.map(String::lowercase).toSet() != installedSigners.map(String::lowercase).toSet() -> "APK does not match the installed signing certificate"
        else -> null
    }

    fun sha256MatchesUrl(url: String, sha256: String): Boolean {
        val path = runCatching { URI(url).path }.getOrNull() ?: return false
        return path.endsWith("-${sha256.lowercase()}.apk")
    }
}
