package ninja.jeremy.liveninja.ui.settings

import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class LatestSaveQueueTest {

    @Test
    fun rapidEditsCoalesceAndLatestValueWins() = runTest {
        val saved = mutableListOf<Int>()
        val queue = LatestSaveQueue<String, Int>(
            scope = this,
            debounceMillis = 100,
            merge = { _, newer -> newer },
            save = { _, value ->
                saved += value
                true
            },
        )

        queue.submit("wake/current", 1)
        queue.submit("wake/current", 2)
        queue.submit("wake/current", 3)

        advanceTimeBy(99)
        assertTrue(saved.isEmpty())
        advanceUntilIdle()

        assertEquals(listOf(3), saved)
    }

    @Test
    fun newerPendingEditSurvivesAnInFlightFailureAndRetriesInOrder() = runTest {
        val firstStarted = CompletableDeferred<Unit>()
        val releaseFirst = CompletableDeferred<Unit>()
        var shouldFail = true
        var attempts = 0
        val saved = mutableListOf<List<Int>>()
        val queue = LatestSaveQueue<String, List<Int>>(
            scope = this,
            debounceMillis = 100,
            merge = { older, newer -> older + newer },
            save = { _, value ->
                if (attempts++ == 0) {
                    firstStarted.complete(Unit)
                    releaseFirst.await()
                }
                saved += value
                !shouldFail
            },
        )

        queue.submit("privacy/current", listOf(1))
        advanceTimeBy(100)
        firstStarted.await()
        queue.submit("privacy/current", listOf(2))
        releaseFirst.complete(Unit)
        advanceUntilIdle()

        assertTrue(queue.hasPending("privacy/current"))
        shouldFail = false
        queue.retry("privacy/current")
        advanceUntilIdle()

        assertEquals(listOf(listOf(1), listOf(1, 2)), saved)
    }
}
