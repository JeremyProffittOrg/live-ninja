package ninja.jeremy.liveninja.ui.permissions

import android.Manifest
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.SharedPreferences
import io.mockk.every
import io.mockk.mockk
import io.mockk.verify
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import ninja.jeremy.liveninja.ui.state.ConsentEvent
import ninja.jeremy.liveninja.ui.state.ConsentLog

class PermissionGuideTest {
    @Test
    fun androidHistorySharesSavedGuideAndActualAttemptsAcrossRecreation() {
        val stored = mutableMapOf<String, Boolean>()
        val preferences = mockk<SharedPreferences>()
        val editor = mockk<SharedPreferences.Editor>(relaxed = true)
        val context = mockk<Context>()
        every { context.applicationContext } returns context
        every { context.getSharedPreferences("permission_guide", Context.MODE_PRIVATE) } returns preferences
        every { preferences.getBoolean(any(), any()) } answers { stored[firstArg()] ?: secondArg() }
        every { preferences.edit() } returns editor
        every { editor.putBoolean(any(), any()) } answers {
            stored[firstArg()] = secondArg()
            editor
        }
        val first = AndroidPermissionHistory(context)
        assertFalse(first.guideSeen)
        first.guideSeen = true
        first.markRequested(PermissionFeature.MICROPHONE)
        val recreated = AndroidPermissionHistory(context)
        assertTrue(recreated.guideSeen)
        assertTrue(recreated.wasRequested(PermissionFeature.MICROPHONE))
        assertFalse(recreated.wasRequested(PermissionFeature.CAMERA))
        verify(exactly = 2) { editor.apply() }
    }

    @Test
    fun locationButtonRequestsApproximateOnlyWithoutFineOrBackgroundUpgrade() {
        assertEquals(Manifest.permission.ACCESS_COARSE_LOCATION, PermissionFeature.APPROXIMATE_LOCATION.runtimePermission())
        val permissions = PermissionFeature.entries.map { it.runtimePermission() }
        assertFalse(permissions.contains(Manifest.permission.ACCESS_FINE_LOCATION))
        assertFalse(permissions.contains(Manifest.permission.ACCESS_BACKGROUND_LOCATION))
        assertFalse(permissions.contains(Manifest.permission.REQUEST_INSTALL_PACKAGES))
        assertEquals(4, permissions.distinct().size)
    }

    @Test
    fun eachFeatureRequestsOnlyItsActualRuntimePermission() {
        assertEquals(Manifest.permission.RECORD_AUDIO, PermissionFeature.MICROPHONE.runtimePermission())
        assertEquals(Manifest.permission.CAMERA, PermissionFeature.CAMERA.runtimePermission())
        assertEquals(Manifest.permission.POST_NOTIFICATIONS, PermissionFeature.NOTIFICATIONS.runtimePermission())
    }

    @Test
    fun missingOemSettingsScreenReturnsRecoverableFailure() {
        val context = mockk<Context>()
        every { context.packageName } returns "ninja.jeremy.liveninja"
        every { context.startActivity(any()) } throws ActivityNotFoundException()
        assertFalse(openPermissionSettings(context, PermissionFeature.MICROPHONE))
    }

    @Test
    fun restrictedSettingsAccessReturnsRecoverableFailure() {
        val context = mockk<Context>()
        every { context.packageName } returns "ninja.jeremy.liveninja"
        every { context.startActivity(any()) } throws SecurityException()
        assertFalse(openPermissionSettings(context, PermissionFeature.MICROPHONE))
    }

    @Test
    fun guideRetainsMicrophoneCameraDisclosuresAndDenialAudit() {
        val log = mockk<ConsentLog>(relaxed = true)
        val model = PermissionGuideViewModel(log)
        model.onDisclosure(PermissionFeature.MICROPHONE)
        model.onResult(PermissionFeature.MICROPHONE, false)
        model.onDisclosure(PermissionFeature.CAMERA)
        model.onResult(PermissionFeature.CAMERA, true)
        model.onResult(PermissionFeature.NOTIFICATIONS, false)
        verify(exactly = 1) { log.record(ConsentEvent.MIC_DISCLOSURE_SHOWN, "permission_guide") }
        verify(exactly = 1) { log.record(ConsentEvent.MIC_PERMISSION_DENIED, "permission_guide") }
        verify(exactly = 1) { log.record(ConsentEvent.CAMERA_DISCLOSURE_SHOWN, "permission_guide") }
        verify(exactly = 1) { log.record(ConsentEvent.CAMERA_PERMISSION_GRANTED, "permission_guide") }
        verify(exactly = 1) { log.record(ConsentEvent.NOTIFICATIONS_DENIED, "permission_guide") }
        verify(exactly = 0) { log.record(ConsentEvent.MIC_PERMISSION_GRANTED, any()) }
    }
}
