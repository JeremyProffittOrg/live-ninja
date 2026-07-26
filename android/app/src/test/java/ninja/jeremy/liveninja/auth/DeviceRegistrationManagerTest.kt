package ninja.jeremy.liveninja.auth

import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.every
import io.mockk.mockk
import io.mockk.verify
import kotlinx.coroutines.test.runTest
import ninja.jeremy.liveninja.net.DeviceDto
import ninja.jeremy.liveninja.net.DeviceMetadataRequest
import ninja.jeremy.liveninja.net.DeviceRegistrationRequest
import ninja.jeremy.liveninja.net.DeviceRegistrationResponse
import ninja.jeremy.liveninja.net.LiveNinjaApi
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import retrofit2.HttpException
import retrofit2.Response

class DeviceRegistrationManagerTest {

    @Test
    fun ownershipConflictRotatesInstallUuidAndRetriesOnce() = runTest {
        assertRotationAndRetry("device_conflict")
    }

    @Test
    fun revokedDeviceRotatesInstallUuidAndRetriesOnce() = runTest {
        assertRotationAndRetry("device_revoked")
    }

    @Test
    fun secondStableConflictEscapesWithoutAnotherRotation() = runTest {
        val oldId = "18c93d91-579a-4dc9-9f13-62e847d981dc"
        val newId = "77dc34de-e89c-4fc2-8ccf-2f59bf13d13b"
        val request = DeviceRegistrationRequest(
            suggestedName = "Kitchen tablet",
            metadata = DeviceMetadataRequest(androidSdk = 35, appVersion = "test"),
        )
        val identity = mockk<DeviceIdentityStore>()
        every { identity.deviceId } returns oldId
        every { identity.registrationRequest() } returns request
        every { identity.rotateDeviceId(oldId) } returns newId
        val api = mockk<LiveNinjaApi>()
        coEvery { api.registerCurrentDevice(request) } throws stableConflict("device_revoked")

        val failure = runCatching {
            DeviceRegistrationManager(api, identity).registerCurrentDevice()
        }.exceptionOrNull()

        assertTrue(failure is HttpException)
        coVerify(exactly = 2) { api.registerCurrentDevice(request) }
        verify(exactly = 1) { identity.rotateDeviceId(oldId) }
    }

    private suspend fun assertRotationAndRetry(errorCode: String) {
        val oldId = "18c93d91-579a-4dc9-9f13-62e847d981dc"
        val newId = "77dc34de-e89c-4fc2-8ccf-2f59bf13d13b"
        val request = DeviceRegistrationRequest(
            suggestedName = "Kitchen tablet",
            metadata = DeviceMetadataRequest(androidSdk = 35, appVersion = "test"),
        )
        val identity = mockk<DeviceIdentityStore>()
        every { identity.deviceId } returns oldId
        every { identity.registrationRequest() } returns request
        every { identity.rotateDeviceId(oldId) } returns newId
        val api = mockk<LiveNinjaApi>()
        var calls = 0
        coEvery { api.registerCurrentDevice(request) } answers {
            if (calls++ == 0) throw stableConflict(errorCode)
            DeviceRegistrationResponse(DeviceDto(deviceId = newId, name = "Kitchen tablet"))
        }

        val result = DeviceRegistrationManager(api, identity).registerCurrentDevice()

        assertEquals(newId, result.deviceId)
        coVerify(exactly = 2) { api.registerCurrentDevice(request) }
        verify(exactly = 1) { identity.rotateDeviceId(oldId) }
    }

    private fun stableConflict(errorCode: String): HttpException = HttpException(
        Response.error<DeviceRegistrationResponse>(
            409,
            """{"error":"$errorCode","message":"That device identity cannot be registered."}"""
                .toResponseBody("application/json".toMediaType()),
        ),
    )
}
