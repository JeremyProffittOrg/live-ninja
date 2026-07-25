package ninja.jeremy.liveninja.realtime

import android.Manifest
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.os.IBinder
import androidx.core.app.NotificationCompat
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat
import dagger.hilt.android.AndroidEntryPoint
import java.util.concurrent.ConcurrentHashMap
import javax.inject.Inject
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.MainActivity
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/**
 * Short-lived camera foreground service. It exists only for a tool capture and
 * is removed as soon as the S3 upload is server-verified.
 */
@AndroidEntryPoint
class CameraCaptureService : Service() {
    @Inject lateinit var captureEngine: Camera2CaptureEngine
    @Inject lateinit var uploader: CameraMediaUploader

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val activeRequestIds = ConcurrentHashMap.newKeySet<String>()

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        createChannel()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val requestId = intent?.getStringExtra(EXTRA_REQUEST_ID).orEmpty()
        val command = CameraCaptureBroker.take(requestId)
        if (requestId.isEmpty() || command == null) {
            stopSelf(startId)
            return START_NOT_STICKY
        }
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.CAMERA) !=
            PackageManager.PERMISSION_GRANTED
        ) {
            CameraCaptureBroker.fail(
                requestId,
                CameraToolException("permission_required", "Android camera permission is not granted."),
            )
            stopSelf(startId)
            return START_NOT_STICKY
        }

        try {
            ServiceCompat.startForeground(
                this,
                NOTIFICATION_ID,
                buildNotification(command),
                ServiceInfo.FOREGROUND_SERVICE_TYPE_CAMERA,
            )
        } catch (e: SecurityException) {
            CameraCaptureBroker.fail(
                requestId,
                CameraToolException(
                    "permission_required",
                    "Android did not allow camera access from the current app state.",
                    e,
                ),
            )
            stopSelf(startId)
            return START_NOT_STICKY
        } catch (e: IllegalStateException) {
            CameraCaptureBroker.fail(
                requestId,
                CameraToolException(
                    "camera_unavailable",
                    "Android would not start the camera foreground service.",
                    e,
                ),
            )
            stopSelf(startId)
            return START_NOT_STICKY
        }

        activeRequestIds += requestId
        scope.launch {
            var localFile: java.io.File? = null
            try {
                val captured = captureEngine.capture(command)
                localFile = captured.file
                val uploaded = uploader.upload(captured)
                CameraCaptureBroker.succeed(requestId, uploaded)
                LNLog.i(
                    LogCategory.GENERAL,
                    TAG,
                    "camera tool saved ${command.kind.toolName} as ${uploaded.name}",
                )
            } catch (e: CameraToolException) {
                CameraCaptureBroker.fail(requestId, e)
                LNLog.w(LogCategory.GENERAL, TAG, "${command.kind.toolName} failed: ${e.code}")
            } catch (e: Exception) {
                CameraCaptureBroker.fail(
                    requestId,
                    CameraToolException(
                        "capture_failed",
                        "The camera capture failed before it could be saved.",
                        e,
                    ),
                )
                LNLog.e(LogCategory.GENERAL, TAG, "${command.kind.toolName} failed", e)
            } finally {
                localFile?.delete()
                activeRequestIds -= requestId
                ServiceCompat.stopForeground(this@CameraCaptureService, ServiceCompat.STOP_FOREGROUND_REMOVE)
                stopSelf(startId)
            }
        }
        return START_NOT_STICKY
    }

    override fun onDestroy() {
        activeRequestIds.forEach { requestId ->
            CameraCaptureBroker.fail(
                requestId,
                CameraToolException("capture_failed", "Camera capture was interrupted by Android."),
            )
        }
        activeRequestIds.clear()
        scope.cancel()
        super.onDestroy()
    }

    private fun createChannel() {
        val manager = getSystemService(NotificationManager::class.java)
        manager.createNotificationChannel(
            NotificationChannel(
                CHANNEL_ID,
                getString(R.string.camera_notification_channel),
                NotificationManager.IMPORTANCE_LOW,
            ).apply {
                description = getString(R.string.camera_notification_channel_description)
            },
        )
    }

    private fun buildNotification(command: CameraCaptureCommand): android.app.Notification {
        val openIntent = Intent(this, MainActivity::class.java)
            .addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP)
        val pendingIntent = PendingIntent.getActivity(
            this,
            41,
            openIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val text = when (command.kind) {
            CameraCaptureKind.PHOTO ->
                getString(R.string.camera_notification_photo, command.lens.wireName)
            CameraCaptureKind.VIDEO ->
                getString(
                    R.string.camera_notification_video,
                    requireNotNull(command.durationSeconds),
                    command.lens.wireName,
                )
        }
        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle(getString(R.string.camera_notification_title))
            .setContentText(text)
            .setContentIntent(pendingIntent)
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .build()
    }

    companion object {
        const val EXTRA_REQUEST_ID = "camera_request_id"
        private const val TAG = "CameraCaptureService"
        private const val CHANNEL_ID = "live_ninja_camera_capture"
        private const val NOTIFICATION_ID = 4107
    }
}
