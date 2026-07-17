package ninja.jeremy.liveninja.wake

import android.app.ForegroundServiceStartNotAllowedException
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.os.Build
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.core.content.ContextCompat
import dagger.hilt.android.AndroidEntryPoint
import javax.inject.Inject
import ninja.jeremy.liveninja.MainActivity
import ninja.jeremy.liveninja.R

/**
 * Restarts [WakeWordService] after reboot / app update when the user had it enabled
 * (plan.md M4 "BOOT_COMPLETED restart").
 *
 * Android 15 (targetSdk 35) forbids launching a `microphone`-type FGS directly from
 * BOOT_COMPLETED — the start throws [ForegroundServiceStartNotAllowedException]. In that case
 * (and any other background-start denial) we post a normal tap-to-resume notification whose
 * content intent opens [MainActivity] with [EXTRA_START_WAKE_SERVICE]; once the app is in the
 * foreground the FGS start is permitted again.
 */
@AndroidEntryPoint
class WakeBootReceiver : BroadcastReceiver() {

    @Inject lateinit var prefs: WakePreferences

    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED &&
            intent.action != Intent.ACTION_MY_PACKAGE_REPLACED
        ) {
            return
        }
        if (!prefs.serviceEnabled) return

        try {
            WakeWordService.start(context)
            Log.i(TAG, "wake service restarted after ${intent.action}")
        } catch (e: Exception) {
            val expected = Build.VERSION.SDK_INT >= Build.VERSION_CODES.S &&
                e is ForegroundServiceStartNotAllowedException
            if (!expected) Log.w(TAG, "boot restart failed", e)
            postResumeNotification(context)
        }
    }

    private fun postResumeNotification(context: Context) {
        // Channel is idempotently (re)created here because the service may not have run yet
        // this boot.
        val manager = context.getSystemService(NotificationManager::class.java)
        manager.createNotificationChannel(
            android.app.NotificationChannel(
                WakeWordService.CHANNEL_ID,
                context.getString(R.string.wake_notification_channel),
                NotificationManager.IMPORTANCE_LOW,
            ),
        )
        val contentIntent = PendingIntent.getActivity(
            context,
            0,
            Intent(context, MainActivity::class.java)
                .putExtra(EXTRA_START_WAKE_SERVICE, true)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notification = NotificationCompat.Builder(context, WakeWordService.CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle(context.getString(R.string.wake_notification_resume_title))
            .setContentText(context.getString(R.string.wake_notification_resume_body))
            .setContentIntent(contentIntent)
            .setAutoCancel(true)
            .setSilent(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .build()
        ContextCompat.getSystemService(context, NotificationManager::class.java)
            ?.notify(WakeWordService.NOTIFICATION_ID + 1, notification)
        Log.i(TAG, "posted tap-to-resume notification (mic FGS not allowed from boot)")
    }

    companion object {
        private const val TAG = "WakeBootReceiver"

        /** MainActivity checks this extra and calls [WakeWordService.start] once foregrounded. */
        const val EXTRA_START_WAKE_SERVICE = "ninja.jeremy.liveninja.wake.START_SERVICE"
    }
}
