package ninja.jeremy.liveninja.ui.jobs

import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.ExperimentalCoroutinesApi
import org.junit.Assert.*
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class JobHistoryTest {
    private val scopeA = JobHistoryScope("account-session-a", "job-a")
    private fun entry(n: Int, text: String = "Message $n") = JobHistoryEntry("entry-$n", n.toLong(), "user", text, "2030-01-01T00:00:00Z")

    @Test fun olderAndLatestKeepEveryLoadedEntryAndReadingAnchor() = runTest {
        val seen = mutableListOf<Pair<String?, HistoryDirection>>()
        val pager = JobHistoryPager(JobHistorySource { scope, cursor, direction ->
            seen += cursor to direction
            when (cursor) {
                null -> JobHistoryPage(scope, listOf(entry(60), entry(61)), "older-60", "newer-61", HistoryRetention.PARTIAL, "Since upgrade")
                "older-60" -> JobHistoryPage(scope, (1..60).map { entry(it) }, null, "unused", HistoryRetention.PARTIAL, "Since upgrade")
                else -> JobHistoryPage(scope, listOf(entry(61), entry(62)), null, "newer-62", HistoryRetention.PARTIAL, "Since upgrade")
            }
        }, backgroundScope)
        pager.open(scopeA); runCurrent(); pager.rememberPosition("entry-60", 81)
        pager.older(); runCurrent(); pager.latest(); runCurrent()
        assertEquals(62, pager.state.value.entries.size)
        assertEquals(JobHistoryAnchor("entry-60", 81), pager.state.value.anchor)
        assertNull(pager.state.value.olderCursor)
        assertEquals("newer-62", pager.state.value.latestCursor)
        assertEquals("newer-61" to HistoryDirection.LATEST, seen.last())
        assertEquals(HistoryRetention.PARTIAL, pager.state.value.retention)
    }

    @Test fun lateAccountResponseCannotEnterAnotherJobOrTenant() = runTest {
        val gate = CompletableDeferred<JobHistoryPage>()
        val scopeB = JobHistoryScope("account-session-b", "job-b")
        val pager = JobHistoryPager(JobHistorySource { scope, _, _ -> if (scope == scopeA) gate.await() else JobHistoryPage(scope, listOf(entry(2))) }, backgroundScope)
        pager.open(scopeA); runCurrent(); pager.open(scopeB); runCurrent()
        gate.complete(JobHistoryPage(scopeA, listOf(entry(1)))); runCurrent()
        assertEquals(scopeB, pager.state.value.scope)
        assertEquals(listOf("entry-2"), pager.state.value.entries.map { it.id })
        assertFalse(pager.state.value.loading)
    }

    @Test fun wrongScopeIsRejectedAndAnUnavailableHistoryIsHonest() = runTest {
        var wrong = true
        val pager = JobHistoryPager(JobHistorySource { scope, _, _ -> if (wrong) JobHistoryPage(scope.copy(jobId = "other"), listOf(entry(1))) else JobHistoryPage(scope, retention = HistoryRetention.UNAVAILABLE, retentionMessage = "Provider has no retained transcript") }, backgroundScope)
        pager.open(scopeA); runCurrent(); assertTrue(pager.state.value.failed); assertTrue(pager.state.value.entries.isEmpty())
        wrong = false; pager.latest(); runCurrent()
        assertEquals(HistoryRetention.UNAVAILABLE, pager.state.value.retention)
        assertEquals("Provider has no retained transcript", pager.state.value.retentionMessage)
    }

    @Test fun duplicateClicksCannotForkCursorReadsAndFailuresKeepLoadedText() = runTest {
        val gate = CompletableDeferred<JobHistoryPage>(); var calls = 0
        val pager = JobHistoryPager(JobHistorySource { scope, cursor, _ -> calls++; if (cursor == null) JobHistoryPage(scope, listOf(entry(1)), "older", "newer") else gate.await() }, backgroundScope)
        pager.open(scopeA); runCurrent(); pager.older(); pager.older(); pager.latest(); runCurrent()
        assertEquals(2, calls)
        gate.completeExceptionally(IllegalStateException("offline")); runCurrent()
        assertTrue(pager.state.value.failed); assertEquals("Message 1", pager.state.value.entries.single().text)
    }

    @Test fun veryLargeUnicodeOutputIsPreservedExactlyWithoutSplittingSurrogates() {
        val original = "Long output 🚀\n".repeat(40000)
        val chunks = historyTextChunks(original, 101)
        assertTrue(chunks.size > 1000)
        assertEquals(original, chunks.joinToString(""))
        assertTrue(chunks.none { it.first().isLowSurrogate() || it.last().isHighSurrogate() })
    }

    @Test fun reconnectCatchesUpTwentyPagesThenOffersExplicitRemainingBacklog() = runTest {
        var calls = 0
        val pager = JobHistoryPager(JobHistorySource { scope, cursor, _ ->
            calls++
            val next = cursor?.toInt()?.plus(1) ?: 0
            JobHistoryPage(scope, listOf(entry(next)), latestCursor = next.toString(), hasMoreNewer = cursor != null && next < 21)
        }, backgroundScope)
        pager.open(scopeA); runCurrent(); pager.latest(); runCurrent()
        assertEquals(21, calls); assertEquals(21, pager.state.value.entries.size)
        assertTrue(pager.state.value.hasMoreNewer)
        pager.latest(); runCurrent()
        assertEquals(22, pager.state.value.entries.size); assertFalse(pager.state.value.hasMoreNewer)
    }

    @Test fun changedDuplicateEntryIsRejectedWithoutReplacingRetainedContent() = runTest {
        val pager = JobHistoryPager(JobHistorySource { scope, cursor, _ ->
            JobHistoryPage(scope, listOf(entry(1, if (cursor == null) "Original" else "Changed")), latestCursor = "next")
        }, backgroundScope)
        pager.open(scopeA); runCurrent(); pager.latest(); runCurrent()
        assertTrue(pager.state.value.failed)
        assertEquals("Original", pager.state.value.entries.single().text)
    }
}
