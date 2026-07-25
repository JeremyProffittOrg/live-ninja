package ninja.jeremy.liveninja.realtime

import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.mockk
import java.io.File
import kotlinx.coroutines.runBlocking
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.MediaUploadCompleteResponse
import ninja.jeremy.liveninja.net.MediaUploadIntentResponse
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Protocol
import okhttp3.Request
import okhttp3.Response
import okhttp3.ResponseBody.Companion.toResponseBody
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class DeviceCameraToolsTest {
    private class FakeGateway(
        private val permission: Boolean = true,
        private val result: UploadedCameraMedia = UploadedCameraMedia(
            deliverableId = "deliv-1",
            name = "photo-20260725.jpg",
            contentType = "image/jpeg",
            sizeBytes = 1234,
        ),
    ) : DeviceCameraGateway {
        val commands = mutableListOf<CameraCaptureCommand>()

        override fun hasCameraPermission(): Boolean = permission

        override suspend fun capture(command: CameraCaptureCommand): UploadedCameraMedia {
            commands += command
            return result
        }
    }

    @Test
    fun photoDefaultsToBackAndReturnsVerifiedFilesResult() = runBlocking {
        val gateway = FakeGateway()
        val output = JSONObject(
            DeviceCameraToolHandler(gateway).execute(
                TAKE_PHOTO_TOOL_NAME,
                "photo-call",
                "{}",
            ),
        )

        assertTrue(output.getBoolean("ok"))
        assertEquals(TAKE_PHOTO_TOOL_NAME, output.getString("tool"))
        val command = gateway.commands.single()
        assertEquals(CameraLens.BACK, command.lens)
        assertEquals(CameraCaptureKind.PHOTO, command.kind)
        val result = output.getJSONObject("output")
        assertEquals("deliv-1", result.getString("deliverableId"))
        assertEquals("Files", result.getString("location"))
        assertEquals("private S3", result.getString("storage"))
        assertEquals(180, result.getInt("retentionDays"))
    }

    @Test
    fun videoHonorsFrontLensAndExplicitDuration() = runBlocking {
        val gateway = FakeGateway(
            result = UploadedCameraMedia(
                deliverableId = "deliv-video",
                name = "video.mp4",
                contentType = "video/mp4",
                sizeBytes = 9876,
            ),
        )
        val output = JSONObject(
            DeviceCameraToolHandler(gateway).execute(
                RECORD_VIDEO_TOOL_NAME,
                "video-call",
                """{"camera":"front","durationSeconds":30}""",
            ),
        )

        assertTrue(output.getBoolean("ok"))
        val command = gateway.commands.single()
        assertEquals(CameraLens.FRONT, command.lens)
        assertEquals(30, command.durationSeconds)
        assertEquals(30, output.getJSONObject("output").getInt("durationSeconds"))
        assertFalse(output.getJSONObject("output").getBoolean("hasAudio"))
    }

    @Test
    fun videoDefaultsToSixtySecondsAndRejectsUnsafeArguments() = runBlocking {
        val validGateway = FakeGateway()
        DeviceCameraToolHandler(validGateway).execute(
            RECORD_VIDEO_TOOL_NAME,
            "default-duration",
            "{}",
        )
        assertEquals(DEFAULT_VIDEO_DURATION_SECONDS, validGateway.commands.single().durationSeconds)

        for (args in listOf(
            """{"durationSeconds":0}""",
            """{"durationSeconds":301}""",
            """{"durationSeconds":1.5}""",
            """{"camera":"external"}""",
            """{"flash":true}""",
        )) {
            val gateway = FakeGateway()
            val output = JSONObject(
                DeviceCameraToolHandler(gateway).execute(
                    RECORD_VIDEO_TOOL_NAME,
                    "bad",
                    args,
                ),
            )
            assertFalse(output.getBoolean("ok"))
            assertEquals("invalid_args", output.getJSONObject("error").getString("code"))
            assertTrue(gateway.commands.isEmpty())
        }
    }

    @Test
    fun missingPermissionFailsBeforeCapture() = runBlocking {
        val gateway = FakeGateway(permission = false)
        val output = JSONObject(
            DeviceCameraToolHandler(gateway).execute(
                TAKE_PHOTO_TOOL_NAME,
                "permission",
                "{}",
            ),
        )

        assertFalse(output.getBoolean("ok"))
        assertEquals("permission_required", output.getJSONObject("error").getString("code"))
        assertTrue(gateway.commands.isEmpty())
    }

    @Test
    fun uploaderUsesSignedHeadersWithoutLiveNinjaAuthorization() = runBlocking {
        val api = mockk<LiveNinjaApi>()
        val file = File.createTempFile("camera-upload-", ".jpg").apply {
            writeBytes(byteArrayOf(1, 2, 3, 4))
        }
        var uploadedRequest: Request? = null
        val unsignedClient = OkHttpClient.Builder()
            .addInterceptor { chain ->
                uploadedRequest = chain.request()
                Response.Builder()
                    .request(chain.request())
                    .protocol(Protocol.HTTP_1_1)
                    .code(200)
                    .message("OK")
                    .body("".toResponseBody("text/plain".toMediaType()))
                    .build()
            }
            .build()
        coEvery { api.createMediaUploadIntent(any()) } returns MediaUploadIntentResponse(
            deliverableId = "d-1",
            name = file.name,
            status = "pending",
            contentType = "image/jpeg",
            sizeBytes = 4,
            uploadUrl = "https://s3.example.invalid/signed",
            headers = mapOf(
                "Content-Type" to "image/jpeg",
                "Content-Length" to "4",
            ),
        )
        coEvery { api.completeMediaUpload("d-1") } returns MediaUploadCompleteResponse(
            deliverableId = "d-1",
            name = file.name,
            status = "ready",
            contentType = "image/jpeg",
            sizeBytes = 4,
        )

        try {
            val uploaded = CameraMediaUploader(api, unsignedClient).upload(
                CapturedCameraMedia(file, "image/jpeg"),
            )
            assertEquals("d-1", uploaded.deliverableId)
            val request = requireNotNull(uploadedRequest)
            assertNull(request.header("Authorization"))
            assertEquals("image/jpeg", request.header("Content-Type"))
            assertEquals("4", request.header("Content-Length"))
            coVerify(exactly = 1) { api.completeMediaUpload("d-1") }
        } finally {
            file.delete()
        }
    }

    @Test
    fun uploaderDeletesPendingIntentAfterS3Failure() = runBlocking {
        val api = mockk<LiveNinjaApi>()
        val file = File.createTempFile("camera-upload-failure-", ".jpg").apply {
            writeBytes(byteArrayOf(1, 2, 3, 4))
        }
        val unsignedClient = OkHttpClient.Builder()
            .addInterceptor { chain ->
                Response.Builder()
                    .request(chain.request())
                    .protocol(Protocol.HTTP_1_1)
                    .code(503)
                    .message("Unavailable")
                    .body("".toResponseBody("text/plain".toMediaType()))
                    .build()
            }
            .build()
        coEvery { api.createMediaUploadIntent(any()) } returns MediaUploadIntentResponse(
            deliverableId = "d-failed",
            name = file.name,
            status = "pending",
            contentType = "image/jpeg",
            sizeBytes = 4,
            uploadUrl = "https://s3.example.invalid/signed",
            headers = mapOf(
                "Content-Type" to "image/jpeg",
                "Content-Length" to "4",
            ),
        )
        coEvery { api.deleteDeliverable("d-failed") } returns Unit

        try {
            var error: CameraToolException? = null
            try {
                CameraMediaUploader(api, unsignedClient).upload(
                    CapturedCameraMedia(file, "image/jpeg"),
                )
            } catch (caught: CameraToolException) {
                error = caught
            }

            assertEquals("upload_failed", requireNotNull(error).code)
            coVerify(exactly = 1) { api.deleteDeliverable("d-failed") }
            coVerify(exactly = 0) { api.completeMediaUpload(any()) }
        } finally {
            file.delete()
        }
    }
}
