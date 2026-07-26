package ninja.jeremy.liveninja.auth

import javax.inject.Inject
import javax.inject.Singleton
import ninja.jeremy.liveninja.net.DeviceDto
import ninja.jeremy.liveninja.net.LiveNinjaApi
import retrofit2.HttpException

/**
 * Registers the current install and repairs stable identity rejections by
 * rotating the app-instance UUID and retrying exactly once. The second
 * attempt deliberately escapes the catch so a revoked-bound stale session
 * cannot spin through identities indefinitely.
 */
@Singleton
class DeviceRegistrationManager @Inject constructor(
    private val api: LiveNinjaApi,
    private val identity: DeviceIdentityStore,
) {
    suspend fun registerCurrentDevice(): DeviceDto {
        val attemptedId = identity.deviceId
        return try {
            api.registerCurrentDevice(identity.registrationRequest()).device
        } catch (error: HttpException) {
            if (!error.requiresFreshDeviceIdentity()) throw error
            identity.rotateDeviceId(attemptedId)
            api.registerCurrentDevice(identity.registrationRequest()).device
        }
    }

    private fun HttpException.requiresFreshDeviceIdentity(): Boolean {
        if (code() != 409) return false
        val body = runCatching { response()?.errorBody()?.string().orEmpty() }
            .getOrDefault("")
        val stableCode = ERROR_CODE_PATTERN.find(body)?.groupValues?.getOrNull(1)
        return stableCode in ROTATING_ERROR_CODES
    }

    private companion object {
        val ROTATING_ERROR_CODES = setOf("device_conflict", "device_revoked")
        val ERROR_CODE_PATTERN = Regex("""["']error["']\s*:\s*["']([^"']+)["']""")
    }
}
