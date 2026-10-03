package ninja.jeremy.liveninja.update

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class AutoUpdatePolicyTest {

    private val hour = UpdateCheckThrottle.FOREGROUND_INTERVAL_MS
    private val sha = "e460916d92d1556b26bf377fc32a90064144d1eea30be2b34dd6ddf4f2c58317"

    private fun release(
        versionCode: Long = 20,
        url: String = "https://live.jeremy.ninja/static/models/downloads/liveninja-0.3.14-20-$sha.apk",
        sha256: String = sha,
        packageName: String = AndroidReleasePolicy.PACKAGE_NAME,
    ) = AndroidLatestDto(
        schemaVersion = 1,
        packageName = packageName,
        versionName = "0.3.14",
        versionCode = versionCode,
        url = url,
        sha256 = sha256,
        sizeBytes = 1234,
        certificateSha256 = "a".repeat(64),
    )

    private fun decide(
        latest: AndroidLatestDto = release(),
        installed: Int = 19,
        canInstall: Boolean = true,
        declined: Long = 0,
        trigger: UpdateTrigger = UpdateTrigger.FOREGROUND,
    ) = UpdateDecider.decide(latest, installed, canInstall, declined, trigger)

    // ---- throttle ----

    @Test
    fun firstCheckIsAlwaysDue() {
        assertTrue(UpdateCheckThrottle.isDue(UpdateTrigger.FOREGROUND, nowMs = 1_000, lastCheckAtMs = 0))
    }

    @Test
    fun foregroundChecksAtMostOncePerHour() {
        val last = 10_000_000L
        assertFalse(UpdateCheckThrottle.isDue(UpdateTrigger.FOREGROUND, last + 1, last))
        assertFalse(UpdateCheckThrottle.isDue(UpdateTrigger.FOREGROUND, last + hour - 1, last))
        assertTrue(UpdateCheckThrottle.isDue(UpdateTrigger.FOREGROUND, last + hour, last))
    }

    @Test
    fun backgroundSharesTheThrottle() {
        val last = 10_000_000L
        assertFalse(UpdateCheckThrottle.isDue(UpdateTrigger.BACKGROUND, last + 60_000, last))
        assertTrue(UpdateCheckThrottle.isDue(UpdateTrigger.BACKGROUND, last + 6 * hour, last))
    }

    @Test
    fun manualIsNeverThrottled() {
        val last = 10_000_000L
        assertTrue(UpdateCheckThrottle.isDue(UpdateTrigger.MANUAL, last + 1, last))
    }

    @Test
    fun clockGoingBackwardsDoesNotWedgeTheUpdater() {
        assertTrue(UpdateCheckThrottle.isDue(UpdateTrigger.FOREGROUND, nowMs = 5_000, lastCheckAtMs = 9_000_000))
    }

    // ---- decision ----

    @Test
    fun newerTrustedReleaseInstalls() {
        val d = decide()
        assertTrue(d is UpdateDecision.Install)
        assertEquals(20L, (d as UpdateDecision.Install).release.versionCode)
    }

    @Test
    fun sameOrOlderVersionIsUpToDate() {
        assertEquals(UpdateDecision.UpToDate, decide(latest = release(versionCode = 19)))
        assertEquals(UpdateDecision.UpToDate, decide(latest = release(versionCode = 18)))
    }

    @Test
    fun foreignPackageIsRejected() {
        assertTrue(decide(latest = release(packageName = "com.evil")) is UpdateDecision.Rejected)
    }

    @Test
    fun untrustedHostIsRejectedEvenWhenNewer() {
        val d = decide(latest = release(url = "https://evil.example/static/models/downloads/x-$sha.apk"))
        assertTrue(d is UpdateDecision.Rejected)
    }

    @Test
    fun urlThatDoesNotCarryTheShaIsRejected() {
        val other = "a".repeat(64)
        val d = decide(latest = release(sha256 = other))
        assertTrue(d is UpdateDecision.Rejected)
    }

    @Test
    fun malformedShaIsRejected() {
        val d = decide(
            latest = release(
                sha256 = "deadbeef",
                url = "https://live.jeremy.ninja/static/models/downloads/liveninja-deadbeef.apk",
            ),
        )
        assertTrue(d is UpdateDecision.Rejected)
    }

    @Test
    fun rejectionWinsOverAMissingInstallPermission() {
        val d = decide(latest = release(packageName = "com.evil"), canInstall = false)
        assertTrue(d is UpdateDecision.Rejected)
    }

    @Test
    fun missingInstallPermissionIsSurfaced() {
        assertTrue(decide(canInstall = false) is UpdateDecision.NeedsInstallPermission)
    }

    @Test
    fun aDeclinedVersionIsOfferedNotReprompted() {
        assertTrue(decide(declined = 20) is UpdateDecision.Offer)
        assertTrue(decide(declined = 20, trigger = UpdateTrigger.BACKGROUND) is UpdateDecision.Offer)
    }

    @Test
    fun manualOverridesADecline() {
        assertTrue(decide(declined = 20, trigger = UpdateTrigger.MANUAL) is UpdateDecision.Install)
    }

    @Test
    fun aDeclineOfAnOlderVersionDoesNotBlockANewerOne() {
        assertTrue(decide(declined = 19) is UpdateDecision.Install)
    }
}
