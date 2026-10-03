package ninja.jeremy.liveninja.ui

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import ninja.jeremy.liveninja.ui.permissions.PermissionAction
import ninja.jeremy.liveninja.ui.permissions.PermissionCoordinator
import ninja.jeremy.liveninja.ui.permissions.PermissionFacts
import ninja.jeremy.liveninja.ui.permissions.PermissionFeature
import ninja.jeremy.liveninja.ui.permissions.PermissionHistory

class StartupPermissionGateTest {
    private class SavedHistory : PermissionHistory {
        override var guideSeen = false
        val requests = mutableSetOf<PermissionFeature>()
        override fun wasRequested(feature: PermissionFeature) = feature in requests
        override fun markRequested(feature: PermissionFeature) { requests.add(feature) }
    }

    @Test
    fun guideDoesNotRepeatAfterNewCoordinatorOrAppProcess() {
        val history = SavedHistory()
        assertTrue(PermissionCoordinator(history).claimStartupGuide())
        assertFalse(PermissionCoordinator(history).claimStartupGuide())
        assertTrue(history.requests.isEmpty())
    }

    @Test
    fun seeingOrSkippingGuideDoesNotRecordAnySystemPermissionRequest() {
        val history = SavedHistory()
        val coordinator = PermissionCoordinator(history)
        coordinator.claimStartupGuide()
        PermissionFeature.entries.forEach { feature ->
            assertEquals(PermissionAction.REQUEST, coordinator.action(feature, PermissionFacts(granted = false)))
        }
        assertTrue(history.requests.isEmpty())
    }

    @Test
    fun grantedAccessNeverProducesAnotherRequest() {
        val coordinator = PermissionCoordinator(SavedHistory())
        PermissionFeature.entries.forEach { feature ->
            assertEquals(PermissionAction.NONE, coordinator.action(feature, PermissionFacts(granted = true)))
        }
    }

    @Test
    fun deniedAccessCanBeRetriedOnlyAfterExplicitFeatureTapWithRationale() {
        val history = SavedHistory()
        val coordinator = PermissionCoordinator(history)
        coordinator.requestStarted(PermissionFeature.MICROPHONE)
        assertEquals(PermissionAction.REQUEST, coordinator.action(
            PermissionFeature.MICROPHONE, PermissionFacts(granted = false, shouldShowRationale = true),
        ))
        assertEquals(setOf(PermissionFeature.MICROPHONE), history.requests)
        assertEquals(PermissionAction.REQUEST, coordinator.action(
            PermissionFeature.CAMERA, PermissionFacts(granted = false),
        ))
    }

    @Test
    fun denialWithoutAnotherAndroidPromptOffersSettingsAcrossRestart() {
        val history = SavedHistory()
        PermissionCoordinator(history).requestStarted(PermissionFeature.CAMERA)
        assertEquals(PermissionAction.OPEN_SETTINGS, PermissionCoordinator(history).action(
            PermissionFeature.CAMERA, PermissionFacts(granted = false, shouldShowRationale = false),
        ))
    }

    @Test
    fun revokedOrExpiredGrantIsReadFreshAndCanRecoverThroughSettings() {
        val coordinator = PermissionCoordinator(SavedHistory())
        coordinator.requestStarted(PermissionFeature.MICROPHONE)
        assertEquals(PermissionAction.NONE, coordinator.action(PermissionFeature.MICROPHONE, PermissionFacts(true)))
        assertEquals(PermissionAction.OPEN_SETTINGS, coordinator.action(PermissionFeature.MICROPHONE, PermissionFacts(false)))
    }

    @Test
    fun notificationsDisabledOnOlderAndroidOpenSettingsInsteadOfUnsupportedRuntimeRequest() {
        val coordinator = PermissionCoordinator(SavedHistory())
        assertEquals(PermissionAction.OPEN_SETTINGS, coordinator.action(
            PermissionFeature.NOTIFICATIONS, PermissionFacts(false, runtimeRequestAvailable = false),
        ))
    }

    @Test
    fun globallyDisabledNotificationsWithRuntimeGrantAlsoOpenSettings() {
        val coordinator = PermissionCoordinator(SavedHistory())
        assertEquals(PermissionAction.OPEN_SETTINGS, coordinator.action(
            PermissionFeature.NOTIFICATIONS, PermissionFacts(false, runtimeRequestAvailable = false),
        ))
        assertEquals(PermissionAction.NONE, coordinator.action(
            PermissionFeature.NOTIFICATIONS, PermissionFacts(true, runtimeRequestAvailable = false),
        ))
    }

    @Test
    fun cameraAbsentDoesNotOfferUnusablePermissionRequest() {
        assertEquals(PermissionAction.NONE, PermissionCoordinator(SavedHistory()).action(
            PermissionFeature.CAMERA, PermissionFacts(false, hardwareAvailable = false),
        ))
    }
}
