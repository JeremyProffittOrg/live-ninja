package ninja.jeremy.liveninja.update

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

/** One timer while the activity is visible. Start/stop must be called on the main thread. */
internal class ForegroundUpdateLoop(
    private val scope: CoroutineScope,
    private val checkAndNextDelay: suspend () -> Long,
) {
    private var job: Job? = null

    fun start() {
        if (job?.isActive == true) return
        job = scope.launch {
            while (isActive) {
                delay(checkAndNextDelay().coerceAtLeast(1_000L))
            }
        }
    }

    fun stop() {
        job?.cancel()
        job = null
    }
}

/** Persisted failure counts prevent foreground/background churn from resetting retries. */
internal object UpdateRetrySchedule {
    fun intervalMs(failures: Int): Long = when (failures) {
        1 -> 60_000L
        2 -> 5 * 60_000L
        3 -> 15 * 60_000L
        else -> UpdateCheckThrottle.FOREGROUND_INTERVAL_MS
    }

    fun remainingMs(now: Long, lastAttempt: Long, failures: Int): Long {
        if (lastAttempt <= 0 || now < lastAttempt) return 0
        return (intervalMs(failures) - (now - lastAttempt)).coerceAtLeast(0)
    }
}
