package ninja.jeremy.liveninja.update

import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.*
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class ForegroundUpdateLoopTest {
    private val hour = UpdateCheckThrottle.FOREGROUND_INTERVAL_MS

    @Test fun checksHourlyWhileVisibleAndStopsWhenBackgrounded() = runTest {
        var checks = 0
        val loop = ForegroundUpdateLoop(backgroundScope) { checks++; hour }
        loop.start(); loop.start(); runCurrent()
        assertEquals(1, checks)
        advanceTimeBy(hour - 1); runCurrent(); assertEquals(1, checks)
        advanceTimeBy(1); runCurrent(); assertEquals(2, checks)
        loop.stop(); advanceTimeBy(3 * hour); runCurrent(); assertEquals(2, checks)
        loop.start(); runCurrent(); assertEquals(3, checks)
        loop.stop()
    }

    @Test fun checksNeverOverlapWhileOneIsWaiting() = runTest {
        var checks = 0
        val completion = CompletableDeferred<Unit>()
        val loop = ForegroundUpdateLoop(backgroundScope) { checks++; completion.await(); hour }
        loop.start(); runCurrent(); advanceTimeBy(2 * hour); runCurrent()
        assertEquals(1, checks)
        completion.complete(Unit); runCurrent()
        advanceTimeBy(hour); runCurrent(); assertEquals(2, checks)
        loop.stop()
    }

    @Test fun networkRetriesAreBoundedAndPersistedTimingSurvivesResume() {
        assertEquals(60_000L, UpdateRetrySchedule.intervalMs(1))
        assertEquals(300_000L, UpdateRetrySchedule.intervalMs(2))
        assertEquals(900_000L, UpdateRetrySchedule.intervalMs(3))
        for (failures in listOf(0, 4, 5, 100)) assertEquals(hour, UpdateRetrySchedule.intervalMs(failures))
        assertEquals(59_999L, UpdateRetrySchedule.remainingMs(1_001, 1_000, 1))
        assertEquals(0L, UpdateRetrySchedule.remainingMs(61_000, 1_000, 1))
        assertEquals(0L, UpdateRetrySchedule.remainingMs(999, 1_000, 1))
    }
}
