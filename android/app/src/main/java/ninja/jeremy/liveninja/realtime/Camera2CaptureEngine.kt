package ninja.jeremy.liveninja.realtime

import android.Manifest
import android.annotation.SuppressLint
import android.content.Context
import android.content.pm.PackageManager
import android.graphics.ImageFormat
import android.hardware.camera2.CameraCaptureSession
import android.hardware.camera2.CameraCharacteristics
import android.hardware.camera2.CameraDevice
import android.hardware.camera2.CameraManager
import android.hardware.camera2.CaptureRequest
import android.hardware.display.DisplayManager
import android.media.ImageReader
import android.media.MediaRecorder
import android.os.Build
import android.os.Handler
import android.os.HandlerThread
import android.util.Size
import android.view.Display
import android.view.Surface
import androidx.core.content.ContextCompat
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.File
import java.io.FileOutputStream
import java.time.Instant
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter
import java.util.UUID
import javax.inject.Inject
import javax.inject.Singleton
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout

/**
 * Camera2 capture with no preview surface, suitable for a voice-triggered
 * operation while the UI is not visible. Videos deliberately omit an audio
 * track: the realtime conversation owns RECORD_AUDIO, and stealing that input
 * would terminate or corrupt the user's live voice session.
 */
@Singleton
class Camera2CaptureEngine @Inject constructor(
    @ApplicationContext context: Context,
) {
    private val appContext = context.applicationContext
    private val cameraManager =
        requireNotNull(appContext.getSystemService(CameraManager::class.java)) {
            "CameraManager is unavailable"
        }
    private val displayManager =
        requireNotNull(appContext.getSystemService(DisplayManager::class.java)) {
            "DisplayManager is unavailable"
        }
    // CameraDevice.close() is asynchronous: OEM Camera2 implementations post
    // the terminal onClosed callback after close() returns. Keep one callback
    // looper alive for this process-lifetime singleton so teardown callbacks
    // never race a per-capture HandlerThread.quitSafely().
    private val callbackThread by lazy(LazyThreadSafetyMode.SYNCHRONIZED) {
        HandlerThread("live-ninja-camera").apply { start() }
    }
    private val callbackHandler by lazy(LazyThreadSafetyMode.SYNCHRONIZED) {
        Handler(callbackThread.looper)
    }

    internal suspend fun capture(command: CameraCaptureCommand): CapturedCameraMedia {
        ensurePermission()
        return when (command.kind) {
            CameraCaptureKind.PHOTO -> takePhoto(command.lens)
            CameraCaptureKind.VIDEO ->
                recordVideo(command.lens, requireNotNull(command.durationSeconds))
        }
    }

    private fun ensurePermission() {
        if (ContextCompat.checkSelfPermission(appContext, Manifest.permission.CAMERA) !=
            PackageManager.PERMISSION_GRANTED
        ) {
            throw CameraToolException(
                "permission_required",
                "Android camera permission is not granted.",
            )
        }
    }

    private suspend fun takePhoto(lens: CameraLens): CapturedCameraMedia {
        val target = resolveCamera(lens)
        val size = choosePhotoSize(target.characteristics)
        val reader = ImageReader.newInstance(
            size.width,
            size.height,
            ImageFormat.JPEG,
            2,
        )
        var device: CameraDevice? = null
        var session: CameraCaptureSession? = null
        val output = newOutputFile(CameraCaptureKind.PHOTO)

        try {
            val bytes = CompletableDeferred<ByteArray>()
            reader.setOnImageAvailableListener({ source ->
                val image = source.acquireLatestImage() ?: return@setOnImageAvailableListener
                image.use {
                    val buffer = it.planes[0].buffer
                    val data = ByteArray(buffer.remaining())
                    buffer.get(data)
                    bytes.complete(data)
                }
            }, callbackHandler)

            device = openCamera(target.id, callbackHandler)
            session = createSession(device, listOf(reader.surface), callbackHandler)
            val request = device.createCaptureRequest(CameraDevice.TEMPLATE_STILL_CAPTURE).apply {
                addTarget(reader.surface)
                set(CaptureRequest.CONTROL_MODE, CaptureRequest.CONTROL_MODE_AUTO)
                setAutoFocusIfSupported(
                    target.characteristics,
                    CaptureRequest.CONTROL_AF_MODE_CONTINUOUS_PICTURE,
                )
                set(
                    CaptureRequest.JPEG_ORIENTATION,
                    captureOrientationDegrees(target.characteristics, lens),
                )
            }.build()
            session.capture(request, null, callbackHandler)
            val jpeg = withTimeout(PHOTO_TIMEOUT_MS) { bytes.await() }
            withContext(Dispatchers.IO) {
                FileOutputStream(output).use { it.write(jpeg) }
            }
            if (output.length() < 1) {
                throw CameraToolException("capture_failed", "The camera returned an empty photo.")
            }
            return CapturedCameraMedia(output, CameraCaptureKind.PHOTO.contentType)
        } catch (e: CameraToolException) {
            output.delete()
            throw e
        } catch (e: Exception) {
            output.delete()
            throw CameraToolException(
                "capture_failed",
                "Android could not capture the photo.",
                e,
            )
        } finally {
            reader.setOnImageAvailableListener(null, null)
            session?.close()
            device?.close()
            reader.close()
        }
    }

    private suspend fun recordVideo(
        lens: CameraLens,
        durationSeconds: Int,
    ): CapturedCameraMedia {
        val target = resolveCamera(lens)
        val size = chooseVideoSize(target.characteristics)
        val output = newOutputFile(CameraCaptureKind.VIDEO)
        var recorder: MediaRecorder? = null
        var device: CameraDevice? = null
        var session: CameraCaptureSession? = null
        var recorderStarted = false

        try {
            val activeRecorder = createRecorder(
                output,
                size,
                captureOrientationDegrees(target.characteristics, lens),
            )
            recorder = activeRecorder
            device = openCamera(target.id, callbackHandler)
            session = createSession(device, listOf(activeRecorder.surface), callbackHandler)
            val request = device.createCaptureRequest(CameraDevice.TEMPLATE_RECORD).apply {
                addTarget(activeRecorder.surface)
                set(CaptureRequest.CONTROL_MODE, CaptureRequest.CONTROL_MODE_AUTO)
                setAutoFocusIfSupported(
                    target.characteristics,
                    CaptureRequest.CONTROL_AF_MODE_CONTINUOUS_VIDEO,
                )
            }.build()
            session.setRepeatingRequest(request, null, callbackHandler)
            activeRecorder.start()
            recorderStarted = true
            delay(durationSeconds * 1_000L)
            activeRecorder.stop()
            recorderStarted = false
            session.stopRepeating()
            if (output.length() < 1) {
                throw CameraToolException("capture_failed", "The camera returned an empty video.")
            }
            return CapturedCameraMedia(output, CameraCaptureKind.VIDEO.contentType)
        } catch (e: CameraToolException) {
            output.delete()
            throw e
        } catch (e: Exception) {
            output.delete()
            throw CameraToolException(
                "capture_failed",
                "Android could not record the video.",
                e,
            )
        } finally {
            if (recorderStarted) {
                runCatching { recorder?.stop() }
            }
            runCatching { session?.stopRepeating() }
            session?.close()
            device?.close()
            runCatching { recorder?.reset() }
            runCatching { recorder?.release() }
        }
    }

    private data class CameraTarget(
        val id: String,
        val characteristics: CameraCharacteristics,
    )

    private fun resolveCamera(lens: CameraLens): CameraTarget {
        val wanted = when (lens) {
            CameraLens.BACK -> CameraCharacteristics.LENS_FACING_BACK
            CameraLens.FRONT -> CameraCharacteristics.LENS_FACING_FRONT
        }
        for (id in cameraManager.cameraIdList) {
            val characteristics = cameraManager.getCameraCharacteristics(id)
            if (characteristics.get(CameraCharacteristics.LENS_FACING) == wanted) {
                return CameraTarget(id, characteristics)
            }
        }
        throw CameraToolException(
            "camera_unavailable",
            "This device does not have an available ${lens.wireName} camera.",
        )
    }

    private fun choosePhotoSize(characteristics: CameraCharacteristics): Size {
        val sizes = characteristics
            .get(CameraCharacteristics.SCALER_STREAM_CONFIGURATION_MAP)
            ?.getOutputSizes(ImageFormat.JPEG)
            ?.toList()
            .orEmpty()
        if (sizes.isEmpty()) {
            throw CameraToolException("camera_unavailable", "The camera cannot produce JPEG photos.")
        }
        // Avoid pathological 50–200 MP output while retaining a full-quality
        // phone photo. If every mode exceeds the cap, use the smallest.
        val underCap = sizes.filter { area(it) <= MAX_PHOTO_PIXELS }
        return (underCap.ifEmpty { sizes }).maxByOrNull(::area) ?: sizes.first()
    }

    private fun chooseVideoSize(characteristics: CameraCharacteristics): Size {
        val sizes = characteristics
            .get(CameraCharacteristics.SCALER_STREAM_CONFIGURATION_MAP)
            ?.getOutputSizes(MediaRecorder::class.java)
            ?.toList()
            .orEmpty()
        if (sizes.isEmpty()) {
            throw CameraToolException("camera_unavailable", "The camera cannot record MP4 video.")
        }
        val atMost1080p = sizes.filter {
            maxOf(it.width, it.height) <= 1920 && minOf(it.width, it.height) <= 1080
        }
        return atMost1080p.maxByOrNull(::area) ?: sizes.minBy(::area)
    }

    @Suppress("DEPRECATION")
    private fun createRecorder(
        output: File,
        size: Size,
        orientationDegrees: Int,
    ): MediaRecorder {
        val recorder = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            MediaRecorder(appContext)
        } else {
            MediaRecorder()
        }
        try {
            recorder.apply {
                setVideoSource(MediaRecorder.VideoSource.SURFACE)
                setOutputFormat(MediaRecorder.OutputFormat.MPEG_4)
                setOutputFile(output.absolutePath)
                setVideoEncoder(MediaRecorder.VideoEncoder.H264)
                setVideoSize(size.width, size.height)
                setVideoFrameRate(VIDEO_FRAME_RATE)
                setVideoEncodingBitRate(
                    (area(size) * 4L).coerceIn(MIN_VIDEO_BITRATE, MAX_VIDEO_BITRATE).toInt(),
                )
                setOrientationHint(orientationDegrees)
                prepare()
            }
        } catch (e: Exception) {
            runCatching { recorder.release() }
            throw e
        }
        return recorder
    }

    @SuppressLint("MissingPermission")
    private suspend fun openCamera(id: String, handler: Handler): CameraDevice =
        suspendCancellableCoroutine { continuation ->
            var opened: CameraDevice? = null
            cameraManager.openCamera(
                id,
                object : CameraDevice.StateCallback() {
                    override fun onOpened(camera: CameraDevice) {
                        opened = camera
                        if (continuation.isActive) continuation.resume(camera)
                        else camera.close()
                    }

                    override fun onDisconnected(camera: CameraDevice) {
                        camera.close()
                        if (continuation.isActive) {
                            continuation.resumeWithException(
                                CameraToolException(
                                    "camera_unavailable",
                                    "The camera disconnected before capture.",
                                ),
                            )
                        }
                    }

                    override fun onError(camera: CameraDevice, error: Int) {
                        camera.close()
                        if (continuation.isActive) {
                            continuation.resumeWithException(
                                CameraToolException(
                                    "camera_unavailable",
                                    "Android camera error $error prevented capture.",
                                ),
                            )
                        }
                    }
                },
                handler,
            )
            continuation.invokeOnCancellation { opened?.close() }
        }

    @Suppress("DEPRECATION")
    private suspend fun createSession(
        camera: CameraDevice,
        surfaces: List<android.view.Surface>,
        handler: Handler,
    ): CameraCaptureSession = suspendCancellableCoroutine { continuation ->
        var opened: CameraCaptureSession? = null
        camera.createCaptureSession(
            surfaces,
            object : CameraCaptureSession.StateCallback() {
                override fun onConfigured(session: CameraCaptureSession) {
                    opened = session
                    if (continuation.isActive) continuation.resume(session)
                    else session.close()
                }

                override fun onConfigureFailed(session: CameraCaptureSession) {
                    session.close()
                    if (continuation.isActive) {
                        continuation.resumeWithException(
                            CameraToolException(
                                "camera_unavailable",
                                "Android could not configure the camera capture.",
                            ),
                        )
                    }
                }
            },
            handler,
        )
        continuation.invokeOnCancellation { opened?.close() }
    }

    private fun newOutputFile(kind: CameraCaptureKind): File {
        val directory = File(appContext.cacheDir, "camera-captures").apply { mkdirs() }
        val stamp = FILE_STAMP.format(Instant.now())
        val suffix = UUID.randomUUID().toString().take(8)
        val prefix = if (kind == CameraCaptureKind.PHOTO) "photo" else "video"
        return File(directory, "$prefix-$stamp-$suffix.${kind.extension}")
    }

    private fun area(size: Size): Long = size.width.toLong() * size.height.toLong()

    private fun captureOrientationDegrees(
        characteristics: CameraCharacteristics,
        lens: CameraLens,
    ): Int = CameraCaptureOrientation.outputRotationDegrees(
        sensorOrientationDegrees =
            characteristics.get(CameraCharacteristics.SENSOR_ORIENTATION) ?: 0,
        displayRotationDegrees = currentDisplayRotationDegrees(),
        lens = lens,
    )

    @Suppress("DEPRECATION")
    private fun currentDisplayRotationDegrees(): Int =
        when (displayManager.getDisplay(Display.DEFAULT_DISPLAY)?.rotation) {
            Surface.ROTATION_90 -> 90
            Surface.ROTATION_180 -> 180
            Surface.ROTATION_270 -> 270
            else -> 0
        }

    private fun CaptureRequest.Builder.setAutoFocusIfSupported(
        characteristics: CameraCharacteristics,
        preferredMode: Int,
    ) {
        val supported =
            characteristics.get(CameraCharacteristics.CONTROL_AF_AVAILABLE_MODES) ?: intArrayOf()
        when {
            preferredMode in supported ->
                set(CaptureRequest.CONTROL_AF_MODE, preferredMode)
            CaptureRequest.CONTROL_AF_MODE_AUTO in supported ->
                set(CaptureRequest.CONTROL_AF_MODE, CaptureRequest.CONTROL_AF_MODE_AUTO)
            // Fixed-focus cameras expose OFF only; leaving the key unset is the
            // portable choice and avoids an invalid capture request.
        }
    }

    private companion object {
        const val PHOTO_TIMEOUT_MS = 30_000L
        const val MAX_PHOTO_PIXELS = 12_000_000L
        const val VIDEO_FRAME_RATE = 30
        const val MIN_VIDEO_BITRATE = 4_000_000L
        // 8 Mbps keeps the five-minute maximum below the backend's 300 MiB
        // signed-upload ceiling even after MP4 container overhead.
        const val MAX_VIDEO_BITRATE = 8_000_000L
        val FILE_STAMP: DateTimeFormatter =
            DateTimeFormatter.ofPattern("yyyyMMdd-HHmmss").withZone(ZoneOffset.UTC)
    }
}
