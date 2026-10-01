package ninja.jeremy.liveninja.ui

import android.Manifest
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class StartupPermissionGateTest {

    private val mic = Manifest.permission.RECORD_AUDIO
    private val camera = Manifest.permission.CAMERA
    private val notifications = Manifest.permission.POST_NOTIFICATIONS
    private val coarse = Manifest.permission.ACCESS_COARSE_LOCATION
    private val fine = Manifest.permission.ACCESS_FINE_LOCATION

    private val nothingGranted = SpecialAccessStatus(
        batteryOptimizationIgnored = false,
        canInstallPackages = false,
        canUseFullScreenIntent = false,
        canDrawOverlays = false,
    )

    @After
    fun resetLedger() = StartupPermissionLedger.resetForTest()

    @Test
    fun api29AsksMicCameraAndLocation() {
        assertEquals(listOf(mic, camera, coarse, fine), runtimePermissionNames(29))
    }

    @Test
    fun api33AlsoAsksNotifications() {
        assertTrue(runtimePermissionNames(33).contains(notifications))
        assertFalse(runtimePermissionNames(32).contains(notifications))
    }

    @Test
    fun firstLaunchRequestsEverythingInOneSheet() {
        val ask = StartupPermissionPlan.runtimeToRequest(34, isGranted = { false }, alreadyAsked = emptySet())
        assertEquals(listOf(mic, camera, notifications, coarse, fine), ask)
    }

    @Test
    fun grantedPermissionsAreNotAskedAgain() {
        val granted = setOf(mic, camera, notifications, coarse, fine)
        assertEquals(
            emptyList<String>(),
            StartupPermissionPlan.runtimeToRequest(34, isGranted = { it in granted }, alreadyAsked = emptySet()),
        )
        assertEquals(
            listOf(coarse, fine),
            StartupPermissionPlan.runtimeToRequest(
                34,
                isGranted = { it in setOf(mic, camera, notifications) },
                alreadyAsked = emptySet(),
            ),
        )
    }

    @Test
    fun whatOnboardingAlreadyAskedIsSkipped() {
        val ask = StartupPermissionPlan.runtimeToRequest(
            34,
            isGranted = { false },
            alreadyAsked = setOf(mic, camera, notifications),
        )
        assertEquals(listOf(coarse, fine), ask)
    }

    @Test
    fun fineLocationAlwaysTravelsWithCoarse() {
        // Coarse was asked (and denied) earlier this process; fine still needs it in the same request.
        val ask = StartupPermissionPlan.runtimeToRequest(
            34,
            isGranted = { it == mic || it == camera || it == notifications },
            alreadyAsked = setOf(coarse),
        )
        assertEquals(listOf(coarse, fine), ask)
    }

    @Test
    fun approximateOnlyGrantAsksForFineAlone() {
        val ask = StartupPermissionPlan.runtimeToRequest(
            34,
            isGranted = { it != fine },
            alreadyAsked = emptySet(),
        )
        assertEquals(listOf(fine), ask)
    }

    @Test
    fun specialStepsFollowTheFixedOrder() {
        assertEquals(
            listOf(
                SpecialAccess.BATTERY,
                SpecialAccess.INSTALL_PACKAGES,
                SpecialAccess.FULL_SCREEN_INTENT,
                SpecialAccess.OVERLAY,
            ),
            StartupPermissionPlan.specialToShow(34, nothingGranted, alreadyAsked = emptySet()),
        )
    }

    @Test
    fun fullScreenIntentIsOnlyAskedOnApi34Plus() {
        val steps = StartupPermissionPlan.specialToShow(33, nothingGranted, alreadyAsked = emptySet())
        assertFalse(steps.contains(SpecialAccess.FULL_SCREEN_INTENT))
        assertEquals(
            listOf(SpecialAccess.BATTERY, SpecialAccess.INSTALL_PACKAGES, SpecialAccess.OVERLAY),
            StartupPermissionPlan.specialToShow(29, nothingGranted, alreadyAsked = emptySet()),
        )
    }

    @Test
    fun overlayIsSkippedWhenTheAppDoesNotUseIt() {
        val steps = StartupPermissionPlan.specialToShow(
            34,
            nothingGranted,
            alreadyAsked = emptySet(),
            appUsesOverlay = false,
        )
        assertFalse(steps.contains(SpecialAccess.OVERLAY))
    }

    @Test
    fun grantedOrAlreadyAskedSpecialsAreDropped() {
        val status = nothingGranted.copy(batteryOptimizationIgnored = true, canUseFullScreenIntent = true)
        assertEquals(
            listOf(SpecialAccess.INSTALL_PACKAGES),
            StartupPermissionPlan.specialToShow(34, status, alreadyAsked = setOf(SpecialAccess.OVERLAY)),
        )
    }

    @Test
    fun gateRunsAtMostOncePerProcess() {
        assertTrue(StartupPermissionLedger.claimGateRun())
        assertFalse(StartupPermissionLedger.claimGateRun())
    }

    @Test
    fun ledgerRecordsOnboardingAsks() {
        StartupPermissionLedger.markRuntimeAsked(listOf(mic))
        StartupPermissionLedger.markSpecialAsked(SpecialAccess.BATTERY)
        assertEquals(setOf(mic), StartupPermissionLedger.askedRuntime())
        assertEquals(setOf(SpecialAccess.BATTERY), StartupPermissionLedger.askedSpecial())
    }
}
