package ninja.jeremy.liveninja.update

/**
 * Pure, Android-free decisions behind the self-updater, split out so the JVM
 * unit tests can pin them (throttle, trust, version, consent).
 */

/** Where an update check came from; decides throttling and consent handling. */
enum class UpdateTrigger {
    /** MainActivity.onStart — throttled to once per [UpdateCheckThrottle.FOREGROUND_INTERVAL_MS]. */
    FOREGROUND,

    /** [AppUpdateWorker] periodic run — same throttle, so a fresh foreground check suppresses it. */
    BACKGROUND,

    /** The user tapped Update / Retry in the update dialog — never throttled, ignores a decline. */
    MANUAL,
}

object UpdateCheckThrottle {
    /** Foreground starts check at most once an hour. */
    const val FOREGROUND_INTERVAL_MS: Long = 60L * 60 * 1000

    /**
     * True when a check is due. [lastCheckAtMs] <= 0 means "never checked".
     * A clock that went backwards (last > now) counts as due, so a bad wall
     * clock can never wedge the updater shut.
     */
    fun isDue(
        trigger: UpdateTrigger,
        nowMs: Long,
        lastCheckAtMs: Long,
        intervalMs: Long = FOREGROUND_INTERVAL_MS,
    ): Boolean {
        if (trigger == UpdateTrigger.MANUAL) return true
        if (lastCheckAtMs <= 0L) return true
        if (lastCheckAtMs > nowMs) return true
        return nowMs - lastCheckAtMs >= intervalMs
    }
}

/** Outcome of comparing the published pointer with this install. */
sealed class UpdateDecision {
    /** Installed build is current (or newer than the pointer). */
    data object UpToDate : UpdateDecision()

    /** The pointer failed a trust rule; never download or install it. */
    data class Rejected(val reason: String) : UpdateDecision()

    /**
     * The user cancelled the system confirm for this exact version. Automatic
     * triggers only offer it (the "Update available" dialog); they do not
     * re-launch the confirm screen on every start.
     */
    data class Offer(val release: AndroidLatestDto) : UpdateDecision()

    /** "Install unknown apps" is off for this package; surface the Settings shortcut. */
    data class NeedsInstallPermission(val release: AndroidLatestDto) : UpdateDecision()

    /** Download with the verified path, then commit the PackageInstaller session. */
    data class Install(val release: AndroidLatestDto) : UpdateDecision()
}

object UpdateDecider {
    private val SHA256_HEX = Regex("^[0-9a-fA-F]{64}$")

    fun decide(
        latest: AndroidLatestDto,
        installedVersionCode: Int,
        canInstallPackages: Boolean,
        declinedVersionCode: Long,
        trigger: UpdateTrigger,
    ): UpdateDecision {
        if (latest.packageName != AndroidReleasePolicy.PACKAGE_NAME) {
            return UpdateDecision.Rejected("package ${latest.packageName} is not this app")
        }
        if (!AndroidReleasePolicy.isTrustedApkUrl(latest.url)) {
            return UpdateDecision.Rejected("untrusted APK URL")
        }
        if (!SHA256_HEX.matches(latest.sha256)) {
            return UpdateDecision.Rejected("malformed sha256")
        }
        if (!AndroidReleasePolicy.sha256MatchesUrl(latest.url, latest.sha256)) {
            return UpdateDecision.Rejected("APK URL is not content-addressed")
        }
        if (!AndroidReleasePolicy.isNewer(latest.versionCode, installedVersionCode)) {
            return UpdateDecision.UpToDate
        }
        if (!canInstallPackages) return UpdateDecision.NeedsInstallPermission(latest)
        if (trigger != UpdateTrigger.MANUAL && declinedVersionCode == latest.versionCode) {
            return UpdateDecision.Offer(latest)
        }
        return UpdateDecision.Install(latest)
    }
}
