package ninja.jeremy.liveninja.auth

import android.content.Context
import android.content.SharedPreferences
import io.mockk.every
import io.mockk.mockk
import java.util.UUID
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Test

class DeviceIdentityStoreTest {

    @Test
    fun systemDeviceNameWinsAndIsCleaned() {
        assertEquals(
            "Kitchen tablet",
            DeviceIdentityStore.inferSuggestedDeviceName(
                systemDeviceName = "  Kitchen   tablet ",
                manufacturer = "Samsung",
                model = "SM-X510",
            ),
        )
    }

    @Test
    fun inferredNamesStripUnicodeControlCharacters() {
        assertEquals(
            "Kitchen tablet",
            DeviceIdentityStore.inferSuggestedDeviceName(
                systemDeviceName = "Kitchen\u202E\u0000 tablet",
                manufacturer = "Samsung",
                model = "SM-X510",
            ),
        )
    }

    @Test
    fun manufacturerAndModelProvideSafeFallback() {
        assertEquals(
            "Google Pixel 9",
            DeviceIdentityStore.inferSuggestedDeviceName(
                systemDeviceName = "unknown",
                manufacturer = "Google",
                model = "Pixel 9",
            ),
        )
        assertEquals(
            "Samsung Galaxy Tab",
            DeviceIdentityStore.inferSuggestedDeviceName(
                systemDeviceName = null,
                manufacturer = "Samsung",
                model = "Samsung Galaxy Tab",
            ),
        )
        assertEquals(
            "Android device",
            DeviceIdentityStore.inferSuggestedDeviceName(null, null, null),
        )
    }

    @Test
    fun generatedInstallUuidPersistsAndExplicitRotationChangesIt() {
        var storedId: String? = null
        var migrationComplete = false
        val editor = mockk<SharedPreferences.Editor>()
        every { editor.putString(any(), any()) } answers {
            storedId = secondArg()
            editor
        }
        every { editor.putStringSet(any(), any()) } returns editor
        every { editor.putBoolean(any(), any()) } answers {
            migrationComplete = secondArg()
            editor
        }
        every { editor.commit() } returns true
        every { editor.apply() } returns Unit
        val prefs = mockk<SharedPreferences>()
        every { prefs.getString(any(), any()) } answers { storedId ?: secondArg() }
        every { prefs.getStringSet(any(), any()) } answers { secondArg() }
        every { prefs.getBoolean(any(), any()) } answers { migrationComplete }
        every { prefs.edit() } returns editor
        val context = mockk<Context>()
        every { context.getSharedPreferences(any(), any()) } returns prefs
        val identity = DeviceIdentityStore(context)

        val original = identity.deviceId
        val same = identity.deviceId
        val rotated = identity.rotateDeviceId(original)

        assertEquals(original, same)
        UUID.fromString(original)
        UUID.fromString(rotated)
        assertNotEquals(original, rotated)
        assertEquals(rotated, identity.deviceId)
        assertEquals(true, identity.settingsMigrationComplete)
    }
}
