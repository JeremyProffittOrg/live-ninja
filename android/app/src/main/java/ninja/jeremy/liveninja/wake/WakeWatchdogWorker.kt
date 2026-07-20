package ninja.jeremy.liveninja.wake

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import androidx.core.app.NotificationCompat
import androidx.core.content.ContextCompat
import androidx.hilt.work.HiltWorker
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.NetworkType
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import dagger.assisted.Assisted
import dagger.assisted.AssistedInject
import java.util.concurrent.TimeUnit
import ninja.jeremy.liveninja.MainActivity
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/**
 * Reliability watchdog (M8.4, 02-voice §D reliability polish): a 15-minute periodic
 * background check that the always-listening [WakeWordService] hasn't silently died
 * while the user still has it enabled (OEM task-killers, an unhandled crash, Doze
 * edge cases the in-process supervision loop can't see because the process is gone).
 *
 * **This worker must NEVER attempt to start the mic foreground service itself** —
 * `startForegroundService()` from a WorkManager background execution context throws
 * `ForegroundServiceStartNotAllowedException` on Android 12+ (background-start
 * restriction; WorkManager's own execution window doesn't carry a start-FGS
 * exemption). Instead it degrades exactly like [WakeBootReceiver]'s boot-restart
 * failure path: post a tap-to-resume notification whose content intent opens
 * [MainActivity], which is a foreground context allowed to start the FGS.
 *
 * Channel/notification are deliberately *replicated* here (not called into
 * [WakeWordService] instance methods, which need a running Service) reusing the same
 * [WakeWordService.CHANNEL_ID] and `NOTIFICATION_ID + 1` slot [WakeBootReceiver]
 * already uses for its own tap-to-resume notification, so the two paths coalesce
 * into one notification instead of stacking duplicates.
 */
@HiltWorker
class WakeWatchdogWorker @AssistedInject constructor(
    @Assisted context: Context,
    @Assisted params: WorkerParameters,
    private val prefs: WakePreferences,
) : CoroutineWorker(context, params) {

    override suspend fun doWork(): Result {
        if (!prefs.serviceEnabled) {
            // User (or a settings sync) turned listening off since this run was
            // scheduled — nothing to watch. The enqueue/cancel hooks in
            // WakeWordService keep the periodic work itself in sync with this
            // flag; this is just a defensive belt-and-suspenders check.
            return Result.success()
        }
        if (WakeWordService.isRunning) {
            return Result.success()
        }
        LNLog.w(
            LogCategory.WAKE,
            TAG,
            "watchdog: listening enabled but WakeWordService is not running — posting tap-to-resume",
        )
        postResumeNotification(applicationContext)
        return Result.success()
    }

    private fun postResumeNotification(context: Context) {
        // Idempotent: createNotificationChannel() is a no-op if the channel (same
        // id/importance) already exists, which is the common case once the
        // service has run at least once.
        val manager = context.getSystemService(NotificationManager::class.java)
        manager.createNotificationChannel(
            NotificationChannel(
                WakeWordService.CHANNEL_ID,
                context.getString(R.string.wake_notification_channel),
                NotificationManager.IMPORTANCE_LOW,
            ).apply {
                description = context.getString(R.string.wake_notification_channel_description)
                setShowBadge(false)
            },
        )
        val contentIntent = PendingIntent.getActivity(
            context,
            0,
            Intent(context, MainActivity::class.java)
                .putExtra(WakeBootReceiver.EXTRA_START_WAKE_SERVICE, true)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notification = NotificationCompat.Builder(context, WakeWordService.CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle(context.getString(R.string.wake_notification_watchdog_title))
            .setContentText(context.getString(R.string.wake_notification_watchdog_body))
            .setContentIntent(contentIntent)
            .setAutoCancel(true)
            .setSilent(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .build()
        ContextCompat.getSystemService(context, NotificationManager::class.java)
            ?.notify(WakeWordService.NOTIFICATION_ID + 1, notification)
    }

    companion object {
        private const val TAG = "WakeWatchdogWorker"
        private const val UNIQUE_WORK_NAME = "wake-watchdog"

        /** WorkManager's floor for periodic work is 15 minutes (M8.4 spec value). */
        private const val INTERVAL_MINUTES = 15L

        /**
         * Enqueue the unique periodic watchdog. Called from [WakeWordService]'s
         * `onStartCommand` whenever `serviceEnabled` flips to true (both the
         * user-initiated start and the boot-restart path funnel through there).
         * `UPDATE` re-applies this request's policy/constraints on every enable
         * without resetting the run cadence if one is already scheduled.
         */
        fun enqueue(context: Context) {
            val request = PeriodicWorkRequestBuilder<WakeWatchdogWorker>(INTERVAL_MINUTES, TimeUnit.MINUTES)
                .setConstraints(
                    // No network/charging requirement — this is a pure local
                    // liveness check + local notification, must run under any
                    // condition the wake service itself is expected to run under.
                    Constraints.Builder().setRequiredNetworkType(NetworkType.NOT_REQUIRED).build(),
                )
                .build()
            WorkManager.getInstance(context).enqueueUniquePeriodicWork(
                UNIQUE_WORK_NAME,
                ExistingPeriodicWorkPolicy.UPDATE,
                request,
            )
        }

        /** Cancel the watchdog. Called when `serviceEnabled` flips to false (ACTION_STOP). */
        fun cancel(context: Context) {
            WorkManager.getInstance(context).cancelUniqueWork(UNIQUE_WORK_NAME)
        }
    }
}
