package ninja.jeremy.liveninja.update

import android.Manifest
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.provider.Settings
import androidx.core.app.NotificationCompat
import androidx.core.content.ContextCompat
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/**
 * Notifications for the self-updater. Static (no Hilt) because the
 * PackageInstaller status broadcast arrives in [UpdateInstallReceiver].
 */
object AppUpdateNotifier {
    const val CHANNEL_ID = "app_updates"
    private const val ID_CONFIRM = 7101
    private const val ID_INSTALL_PERMISSION = 7102
    private const val TAG = "AppUpdateNotifier"

    /** Opens the system install-confirm screen the PackageInstaller handed back. */
    fun postConfirmInstall(context: Context, confirm: Intent) {
        val pending = PendingIntent.getActivity(
            context,
            ID_CONFIRM,
            Intent(confirm).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        notify(
            context,
            ID_CONFIRM,
            NotificationCompat.Builder(context, CHANNEL_ID)
                .setSmallIcon(R.drawable.ic_launcher_foreground)
                .setContentTitle(context.getString(R.string.update_confirm_notification_title))
                .setContentText(context.getString(R.string.update_confirm_notification_body))
                .setContentIntent(pending)
                .setAutoCancel(true)
                .setPriority(NotificationCompat.PRIORITY_HIGH)
                .build(),
        )
    }

    /** Opens Settings › Install unknown apps for this package. */
    fun postInstallPermissionNeeded(context: Context) {
        val pending = PendingIntent.getActivity(
            context,
            ID_INSTALL_PERMISSION,
            unknownSourcesIntent(context),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        notify(
            context,
            ID_INSTALL_PERMISSION,
            NotificationCompat.Builder(context, CHANNEL_ID)
                .setSmallIcon(R.drawable.ic_launcher_foreground)
                .setContentTitle(context.getString(R.string.update_permission_notification_title))
                .setContentText(context.getString(R.string.update_permission_notification_body))
                .setStyle(
                    NotificationCompat.BigTextStyle()
                        .bigText(context.getString(R.string.update_permission_notification_body)),
                )
                .setContentIntent(pending)
                .setAutoCancel(true)
                .setPriority(NotificationCompat.PRIORITY_DEFAULT)
                .build(),
        )
    }

    fun cancelAll(context: Context) {
        val nm = context.getSystemService(NotificationManager::class.java) ?: return
        nm.cancel(ID_CONFIRM)
        nm.cancel(ID_INSTALL_PERMISSION)
    }

    fun unknownSourcesIntent(context: Context): Intent =
        Intent(
            Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES,
            Uri.parse("package:${context.packageName}"),
        ).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)

    private fun notify(context: Context, id: Int, notification: android.app.Notification) {
        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) !=
            PackageManager.PERMISSION_GRANTED
        ) {
            LNLog.w(LogCategory.GENERAL, TAG, "notifications not granted; update notice $id not shown")
            return
        }
        val nm = context.getSystemService(NotificationManager::class.java) ?: return
        nm.createNotificationChannel(
            NotificationChannel(
                CHANNEL_ID,
                context.getString(R.string.update_notification_channel),
                NotificationManager.IMPORTANCE_HIGH,
            ).apply { setShowBadge(false) },
        )
        runCatching { nm.notify(id, notification) }
            .onFailure { LNLog.w(LogCategory.GENERAL, TAG, "update notification failed", it) }
    }
}
