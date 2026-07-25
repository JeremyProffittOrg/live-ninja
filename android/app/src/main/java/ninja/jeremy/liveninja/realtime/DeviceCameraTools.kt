package ninja.jeremy.liveninja.realtime

import android.Manifest
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import androidx.core.content.ContextCompat
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.File
import java.io.IOException
import java.util.UUID
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.TimeUnit
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.MediaUploadIntentRequest
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.asRequestBody
import org.json.JSONException
import org.json.JSONObject

const val TAKE_PHOTO_TOOL_NAME = "take_photo"
const val RECORD_VIDEO_TOOL_NAME = "record_video"
const val DEFAULT_VIDEO_DURATION_SECONDS = 60
const val MAX_VIDEO_DURATION_SECONDS = 300

internal enum class CameraLens(val wireName: String) {
    BACK("back"),
    FRONT("front"),
    ;

    companion object {
        fun fromWireName(value: String): CameraLens? = entries.firstOrNull { it.wireName == value }
    }
}

internal enum class CameraCaptureKind(
    val toolName: String,
    val contentType: String,
    val extension: String,
) {
    PHOTO(TAKE_PHOTO_TOOL_NAME, "image/jpeg", "jpg"),
    VIDEO(RECORD_VIDEO_TOOL_NAME, "video/mp4", "mp4"),
    ;

    companion object {
        fun forTool(name: String): CameraCaptureKind? = entries.firstOrNull { it.toolName == name }
    }
}

internal data class CameraCaptureCommand(
    val kind: CameraCaptureKind,
    val lens: CameraLens,
    val durationSeconds: Int? = null,
)

internal data class CapturedCameraMedia(
    val file: File,
    val contentType: String,
)

internal data class UploadedCameraMedia(
    val deliverableId: String,
    val name: String,
    val contentType: String,
    val sizeBytes: Long,
)

internal class CameraToolException(
    val code: String,
    message: String,
    cause: Throwable? = null,
) : Exception(message, cause)

/** Framework/storage seam kept narrow so command semantics are local-JVM testable. */
internal interface DeviceCameraGateway {
    fun hasCameraPermission(): Boolean
    suspend fun capture(command: CameraCaptureCommand): UploadedCameraMedia
}

@Singleton
class DeviceCameraToolExecutor @Inject constructor(
    @ApplicationContext context: Context,
) {
    private val handler = DeviceCameraToolHandler(ForegroundCameraGateway(context))

    suspend fun execute(toolName: String, callId: String, argumentsJson: String): String =
        handler.execute(toolName, callId, argumentsJson)
}

/**
 * Starts a camera-typed foreground service before Camera2 is opened. Voice
 * interaction is an Android background-start exemption, and the existing live
 * session normally already keeps this process foreground; refusals still become
 * truthful tool errors instead of a crash or fabricated success.
 */
private class ForegroundCameraGateway(
    context: Context,
) : DeviceCameraGateway {
    private val appContext = context.applicationContext

    override fun hasCameraPermission(): Boolean =
        ContextCompat.checkSelfPermission(appContext, Manifest.permission.CAMERA) ==
            PackageManager.PERMISSION_GRANTED

    override suspend fun capture(command: CameraCaptureCommand): UploadedCameraMedia =
        CameraCaptureBroker.capture(appContext, command)
}

internal class DeviceCameraToolHandler(
    private val gateway: DeviceCameraGateway,
) {
    suspend fun execute(toolName: String, callId: String, argumentsJson: String): String {
        val command = try {
            parseCommand(toolName, argumentsJson)
        } catch (e: CameraToolException) {
            return errorOutput(toolName, callId, e.code, e.message.orEmpty())
        }

        if (!gateway.hasCameraPermission()) {
            return errorOutput(
                toolName,
                callId,
                "permission_required",
                "Camera access is off. Open Live Ninja Settings > Privacy and allow Camera, then try again.",
            )
        }

        return try {
            val uploaded = gateway.capture(command)
            successOutput(toolName, callId, command, uploaded)
        } catch (e: CameraToolException) {
            errorOutput(toolName, callId, e.code, e.message.orEmpty())
        } catch (e: SecurityException) {
            errorOutput(
                toolName,
                callId,
                "permission_required",
                "Android denied camera access. Allow Camera in Live Ninja Settings, then try again.",
            )
        } catch (e: Exception) {
            errorOutput(
                toolName,
                callId,
                "capture_failed",
                e.message?.takeIf { it.isNotBlank() } ?: "The camera capture failed.",
            )
        }
    }

    private fun parseCommand(toolName: String, argumentsJson: String): CameraCaptureCommand {
        val kind = CameraCaptureKind.forTool(toolName)
            ?: throw CameraToolException("invalid_args", "unknown camera tool \"$toolName\"")
        val json = try {
            JSONObject(argumentsJson)
        } catch (_: JSONException) {
            throw CameraToolException("invalid_args", "arguments must be a JSON object")
        }

        val allowed = when (kind) {
            CameraCaptureKind.PHOTO -> setOf("camera")
            CameraCaptureKind.VIDEO -> setOf("camera", "durationSeconds")
        }
        val keys = json.keys()
        while (keys.hasNext()) {
            val key = keys.next()
            if (key !in allowed) {
                throw CameraToolException(
                    "invalid_args",
                    "unexpected argument \"$key\" for tool $toolName",
                )
            }
        }

        val lens = if (!json.has("camera") || json.isNull("camera")) {
            CameraLens.BACK
        } else {
            val raw = json.opt("camera")
            if (raw !is String || raw.isEmpty()) {
                throw CameraToolException("invalid_args", "camera must be back or front")
            }
            CameraLens.fromWireName(raw)
                ?: throw CameraToolException("invalid_args", "camera must be back or front")
        }

        val duration = if (kind == CameraCaptureKind.VIDEO) {
            optionalWholeNumber(json, "durationSeconds") ?: DEFAULT_VIDEO_DURATION_SECONDS
        } else {
            null
        }
        if (duration != null && duration !in 1..MAX_VIDEO_DURATION_SECONDS) {
            throw CameraToolException(
                "invalid_args",
                "durationSeconds must be between 1 and $MAX_VIDEO_DURATION_SECONDS",
            )
        }
        return CameraCaptureCommand(kind, lens, duration)
    }

    private fun optionalWholeNumber(json: JSONObject, name: String): Int? {
        if (!json.has(name) || json.isNull(name)) return null
        val raw = json.opt(name) as? Number
            ?: throw CameraToolException("invalid_args", "$name must be a whole number")
        val value = raw.toDouble()
        if (!value.isFinite() || value != value.toInt().toDouble()) {
            throw CameraToolException("invalid_args", "$name must be a whole number")
        }
        return value.toInt()
    }

    private fun successOutput(
        toolName: String,
        callId: String,
        command: CameraCaptureCommand,
        uploaded: UploadedCameraMedia,
    ): String {
        val output = JSONObject()
            .put("deliverableId", uploaded.deliverableId)
            .put("name", uploaded.name)
            .put("contentType", uploaded.contentType)
            .put("sizeBytes", uploaded.sizeBytes)
            .put("camera", command.lens.wireName)
            .put("location", "Files")
            .put("storage", "private S3")
            .put("retentionDays", 180)
        if (command.kind == CameraCaptureKind.VIDEO) {
            output
                .put("durationSeconds", command.durationSeconds)
                // The live voice session already owns the microphone. Recording
                // video-only avoids stealing it and breaking the conversation.
                .put("hasAudio", false)
        }
        output.put(
            "instruction",
            "The ${if (command.kind == CameraCaptureKind.PHOTO) "photo" else "video"} was captured " +
                "and saved as ${uploaded.name} in Files. Confirm that briefly.",
        )
        return JSONObject()
            .put("tool", toolName)
            .put("callId", callId)
            .put("ok", true)
            .put("output", output)
            .toString()
    }

    private fun errorOutput(
        toolName: String,
        callId: String,
        code: String,
        message: String,
    ): String = JSONObject()
        .put("tool", toolName)
        .put("callId", callId)
        .put("ok", false)
        .put("error", JSONObject().put("code", code).put("message", message))
        .toString()
}

/**
 * Process-local request/result rendezvous between the realtime coordinator and
 * [CameraCaptureService]. Only one camera operation runs at once; the timeout
 * includes capture duration plus the uploader's slow-network allowance and
 * one final minute for camera startup/server verification.
 */
internal object CameraCaptureBroker {
    private data class Pending(
        val command: CameraCaptureCommand,
        val result: CompletableDeferred<UploadedCameraMedia>,
    )

    private val requests = ConcurrentHashMap<String, Pending>()
    private val cameraMutex = Mutex()

    suspend fun capture(context: Context, command: CameraCaptureCommand): UploadedCameraMedia =
        cameraMutex.withLock {
            val requestId = UUID.randomUUID().toString()
            val result = CompletableDeferred<UploadedCameraMedia>()
            requests[requestId] = Pending(command, result)
            try {
                ContextCompat.startForegroundService(
                    context,
                    Intent(context, CameraCaptureService::class.java)
                        .putExtra(CameraCaptureService.EXTRA_REQUEST_ID, requestId),
                )
            } catch (e: RuntimeException) {
                requests.remove(requestId)
                throw CameraToolException(
                    "camera_unavailable",
                    "Android would not start camera capture from the current app state.",
                    e,
                )
            }

            val captureSeconds = command.durationSeconds ?: 0
            try {
                // A five-minute 8 Mbps recording can approach 300 MB. Preserve
                // enough time for a real mobile uplink rather than timing out
                // immediately after the capture itself finishes.
                withTimeout((captureSeconds + 12L * 60L) * 1_000L) { result.await() }
            } finally {
                requests.remove(requestId)
            }
        }

    fun take(requestId: String): CameraCaptureCommand? = requests[requestId]?.command

    fun succeed(requestId: String, uploaded: UploadedCameraMedia) {
        requests[requestId]?.result?.complete(uploaded)
    }

    fun fail(requestId: String, error: Throwable) {
        requests[requestId]?.result?.completeExceptionally(error)
    }
}

/**
 * Direct-to-S3 transfer. Backend calls use [LiveNinjaApi] (authorized); the PUT
 * uses the unqualified, credential-free client and only the signed headers
 * returned by the intent, so no Live Ninja credential can leak to S3.
 */
@Singleton
class CameraMediaUploader @Inject constructor(
    private val api: LiveNinjaApi,
    unsignedHttpClient: OkHttpClient,
) {
    private val uploadHttpClient = unsignedHttpClient.newBuilder()
        .connectTimeout(30, TimeUnit.SECONDS)
        .writeTimeout(10, TimeUnit.MINUTES)
        .readTimeout(2, TimeUnit.MINUTES)
        .callTimeout(11, TimeUnit.MINUTES)
        .build()

    internal suspend fun upload(media: CapturedCameraMedia): UploadedCameraMedia =
        withContext(Dispatchers.IO) {
            val size = media.file.length()
            if (size < 1) {
                throw CameraToolException("capture_failed", "The camera produced an empty file.")
            }
            val intent = try {
                api.createMediaUploadIntent(
                    MediaUploadIntentRequest(
                        name = media.file.name,
                        contentType = media.contentType,
                        sizeBytes = size,
                    ),
                )
            } catch (e: Exception) {
                throw CameraToolException(
                    "upload_failed",
                    "The capture succeeded, but Live Ninja could not prepare Files storage.",
                    e,
                )
            }

            try {
                val request = Request.Builder()
                    .url(intent.uploadUrl)
                    .put(media.file.asRequestBody(media.contentType.toMediaType()))
                    .apply {
                        intent.headers.forEach { (name, value) -> header(name, value) }
                    }
                    .build()
                try {
                    uploadHttpClient.newCall(request).execute().use { response ->
                        if (!response.isSuccessful) {
                            throw CameraToolException(
                                "upload_failed",
                                "The capture succeeded, but S3 rejected the upload (HTTP ${response.code}).",
                            )
                        }
                    }
                } catch (e: CameraToolException) {
                    throw e
                } catch (e: IOException) {
                    throw CameraToolException(
                        "upload_failed",
                        "The capture succeeded, but the upload connection failed.",
                        e,
                    )
                }

                val complete = try {
                    api.completeMediaUpload(intent.deliverableId)
                } catch (e: Exception) {
                    throw CameraToolException(
                        "upload_failed",
                        "The media reached storage, but Live Ninja could not verify it.",
                        e,
                    )
                }
                if (complete.status != "ready") {
                    throw CameraToolException(
                        "upload_failed",
                        "The media upload was not marked ready in Files.",
                    )
                }
                UploadedCameraMedia(
                    deliverableId = complete.deliverableId,
                    name = complete.name,
                    contentType = complete.contentType,
                    sizeBytes = complete.sizeBytes,
                )
            } catch (e: Exception) {
                // DELETE is already the canonical owner-scoped cleanup path: it removes
                // any partial S3 object, pending DELIV row, and filename claim. Run it
                // non-cancellably so a stopped service/app coroutine does not strand the
                // normal failure case. Process death can still interrupt the request; the
                // pending row remains visible and manually deletable in Files.
                try {
                    withContext(NonCancellable) {
                        api.deleteDeliverable(intent.deliverableId)
                    }
                } catch (_: Exception) {
                    // Preserve the upload failure as the truthful tool result. Cleanup is
                    // best-effort and must never disguise the original cause.
                }
                throw e
            }
        }
}
