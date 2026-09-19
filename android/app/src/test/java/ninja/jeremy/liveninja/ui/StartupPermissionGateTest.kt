package ninja.jeremy.liveninja.ui

import android.Manifest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class StartupPermissionGateTest {
    @Test
    fun api29AsksMicAndCameraOnly() {
        val names = runtimePermissionNames(29)
        assertEquals(
            listOf(Manifest.permission.RECORD_AUDIO, Manifest.permission.CAMERA),
            names,
        )
    }

    @Test
    fun api33AlsoAsksNotifications() {
        val names = runtimePermissionNames(33)
        assertTrue(names.contains(Manifest.permission.POST_NOTIFICATIONS))
        assertFalse(runtimePermissionNames(32).contains(Manifest.permission.POST_NOTIFICATIONS))
    }
}
