package ninja.jeremy.liveninja.auth

import android.content.Context
import android.os.Build
import android.provider.Settings
import dagger.hilt.android.qualifiers.ApplicationContext
import java.util.UUID
import javax.inject.Inject
import javax.inject.Singleton
import ninja.jeremy.liveninja.BuildConfig
import ninja.jeremy.liveninja.net.DeviceMetadataRequest
import ninja.jeremy.liveninja.net.DeviceRegistrationRequest

/**
 * Stable, app-scoped identity for this Android installation.
 *
 * The identifier is deliberately random. It is not derived from ANDROID_ID,
 * a serial number, MAC address, IMEI, or any other hardware identifier. The
 * manifest disables Android backup, so restoring an app backup onto another
 * physical device cannot clone the identifier.
 */
@Singleton
class DeviceIdentityStore @Inject constructor(
    @ApplicationContext private val context: Context,
) {
    private val prefs = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)

    val deviceId: String
        get() = synchronized(this) {
            prefs.getString(KEY_DEVICE_ID, null)?.takeIf(::isUuid)
                ?: persistNewDeviceId()
        }

    var settingsMigrationComplete: Boolean
        @Synchronized get() = prefs.getBoolean(KEY_SETTINGS_MIGRATED, false)
        @Synchronized set(value) {
            prefs.edit().putBoolean(KEY_SETTINGS_MIGRATED, value).commit()
        }

    val pendingSettingsSections: Set<String>
        @Synchronized get() =
            prefs.getStringSet(KEY_PENDING_SETTINGS_SECTIONS, emptySet()).orEmpty().toSet()

    @Synchronized
    fun markSettingsSectionPending(sectionId: String) {
        val pending = pendingSettingsSections + sectionId
        prefs.edit().putStringSet(KEY_PENDING_SETTINGS_SECTIONS, pending).commit()
    }

    @Synchronized
    fun clearSettingsSectionPending(sectionId: String) {
        val pending = pendingSettingsSections - sectionId
        prefs.edit().putStringSet(KEY_PENDING_SETTINGS_SECTIONS, pending).apply()
    }

    @Synchronized
    fun clearPendingSettingsSections() {
        prefs.edit().putStringSet(KEY_PENDING_SETTINGS_SECTIONS, emptySet()).apply()
    }

    /**
     * Rotate only if the caller still owns the id it attempted. This makes two
     * concurrent registration retries converge on one new app-instance id.
     */
    @Synchronized
    fun rotateDeviceId(attemptedDeviceId: String): String {
        val current = deviceId
        return if (current == attemptedDeviceId) {
            persistRotatedDeviceId()
        } else {
            current
        }
    }

    fun registrationRequest(): DeviceRegistrationRequest {
        val manufacturer = cleanBuildValue(Build.MANUFACTURER)
        val model = cleanBuildValue(Build.MODEL)
        val camera = context.packageManager.hasSystemFeature("android.hardware.camera.any")
        return DeviceRegistrationRequest(
            suggestedName = inferSuggestedDeviceName(
                systemDeviceName = readSystemDeviceName(),
                manufacturer = manufacturer,
                model = model,
            ),
            metadata = DeviceMetadataRequest(
                manufacturer = manufacturer,
                model = model,
                product = cleanBuildValue(Build.PRODUCT),
                androidSdk = Build.VERSION.SDK_INT,
                appVersion = BuildConfig.VERSION_NAME,
                osVersion = cleanBuildValue(Build.VERSION.RELEASE),
            ),
            capabilities = buildList {
                add("aboutYou")
                add("wakeWord")
                add("persona")
                add("voiceEngine")
                add("turnDetection")
                add("appearance")
                add("microphone")
                add("privacy")
                if (camera) add("camera")
            },
        )
    }

    private fun readSystemDeviceName(): String? =
        runCatching {
            Settings.Global.getString(context.contentResolver, Settings.Global.DEVICE_NAME)
        }.getOrNull()

    companion object {
        private const val PREFS_NAME = "liveninja_device_identity"
        private const val KEY_DEVICE_ID = "app_instance_id"
        private const val KEY_SETTINGS_MIGRATED = "settings_v2_migrated"
        private const val KEY_PENDING_SETTINGS_SECTIONS = "settings_v2_pending_sections"
        private const val MAX_NAME_LENGTH = 80

        internal fun inferSuggestedDeviceName(
            systemDeviceName: String?,
            manufacturer: String?,
            model: String?,
        ): String {
            cleanName(systemDeviceName)?.let { return it }
            val maker = cleanName(manufacturer)
            val deviceModel = cleanName(model)
            return when {
                maker != null && deviceModel != null &&
                    !deviceModel.startsWith(maker, ignoreCase = true) -> "$maker $deviceModel"
                deviceModel != null -> deviceModel
                maker != null -> "$maker Android device"
                else -> "Android device"
            }.take(MAX_NAME_LENGTH)
        }

        private fun cleanName(value: String?): String? =
            value
                ?.replace(Regex("""[\p{Cc}\p{Cf}]"""), " ")
                ?.replace(Regex("\\s+"), " ")
                ?.trim()
                ?.take(MAX_NAME_LENGTH)
                ?.takeIf {
                    it.isNotBlank() &&
                        !it.equals("unknown", ignoreCase = true) &&
                        !it.equals("null", ignoreCase = true)
                }

        private fun cleanBuildValue(value: String?): String? = cleanName(value)

        private fun isUuid(value: String): Boolean =
            runCatching { UUID.fromString(value) }.isSuccess
    }

    private fun persistNewDeviceId(): String =
        UUID.randomUUID().toString().also { generated ->
            prefs.edit().putString(KEY_DEVICE_ID, generated).commit()
        }

    /**
     * A rotated identity is fresh. Atomically suppress legacy upload so local
     * settings associated with a conflicting/revoked identity cannot be
     * copied into another account.
     */
    private fun persistRotatedDeviceId(): String =
        UUID.randomUUID().toString().also { generated ->
            prefs.edit()
                .putString(KEY_DEVICE_ID, generated)
                .putBoolean(KEY_SETTINGS_MIGRATED, true)
                .putStringSet(KEY_PENDING_SETTINGS_SECTIONS, emptySet())
                .commit()
        }
}
