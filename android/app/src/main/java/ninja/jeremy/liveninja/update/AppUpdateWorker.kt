package ninja.jeremy.liveninja.update

import android.content.Context
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
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/**
 * Background self-update check, every [INTERVAL_HOURS] h on any connected
 * network. Shares [AppUpdateCoordinator]'s throttle with the foreground check,
 * so a run within an hour of an app start is a no-op. Failures are logged and
 * reported as success: the next period is the retry, never a tight loop.
 */
@HiltWorker
class AppUpdateWorker @AssistedInject constructor(
    @Assisted context: Context,
    @Assisted params: WorkerParameters,
    private val coordinator: AppUpdateCoordinator,
) : CoroutineWorker(context, params) {

    override suspend fun doWork(): Result {
        runCatching { coordinator.check(UpdateTrigger.BACKGROUND) }
            .onFailure { LNLog.w(LogCategory.GENERAL, TAG, "background update check crashed", it) }
        return Result.success()
    }

    companion object {
        private const val TAG = "AppUpdateWorker"
        private const val UNIQUE_WORK_NAME = "app-update-check"
        const val INTERVAL_HOURS = 6L

        /** Idempotent: KEEP leaves an existing schedule's cadence alone. */
        fun enqueue(context: Context) {
            val request = PeriodicWorkRequestBuilder<AppUpdateWorker>(INTERVAL_HOURS, TimeUnit.HOURS)
                .setConstraints(
                    Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build(),
                )
                .build()
            WorkManager.getInstance(context).enqueueUniquePeriodicWork(
                UNIQUE_WORK_NAME,
                ExistingPeriodicWorkPolicy.KEEP,
                request,
            )
        }
    }
}
