package ninja.jeremy.liveninja.ui.jobs

import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import ninja.jeremy.liveninja.net.*
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.Assert.*
import org.junit.Test
import retrofit2.HttpException
import retrofit2.Response

@OptIn(ExperimentalCoroutinesApi::class)
class GhostHistoryPagerTest {
    private val selection = GhostHistoryScope("sid-a", "OFFICEPC", "provider-a")
    private fun event(id: String, sequence: String = id, text: String = "Text $id") = GhostHistoryEventDto(id, sequence, kind = "assistant", text = text)
    private fun page(events: List<GhostHistoryEventDto> = emptyList(), next: String? = null, resume: String = "tail") = GhostEventsResponse(1, selection.nodeId, selection.providerSessionId, events, next, resume, "retained_only")
    private fun failure(code: Int) = HttpException(Response.error<GhostEventsResponse>(code, "{}".toResponseBody()))

    @Test fun emptyPagesContinueAndStringSequenceAndEveryFragmentArePreserved() = runTest {
        val full = "Full retained output\n".repeat(9000)
        val cursors = mutableListOf<String?>()
        val pager = GhostHistoryPager(GhostHistorySource { _, cursor ->
            cursors += cursor
            when (cursor) {
                null -> page(next = "empty-next")
                "empty-next" -> page(listOf(event("b", "2", full).copy(part = 1)), "third")
                else -> page(listOf(event("a", "10").copy(more = true)))
            }
        }, backgroundScope)
        pager.open(selection); runCurrent()
        assertEquals(listOf(null, "empty-next", "third"), cursors)
        assertEquals(listOf("a", "b"), pager.state.value.events.map { it.id })
        assertEquals(full, pager.state.value.events.last().text)
        assertTrue(pager.state.value.events.first().more)
        assertNull(pager.state.value.events.first().timestamp)
        assertEquals("tail", pager.state.value.resumeCursor)
    }

    @Test fun resumePollAndFreshRescanRecoverLateUploadsAndPruneExpiredIdsOnlyWhenComplete() = runTest {
        var rescan = false
        val gate = CompletableDeferred<GhostEventsResponse>()
        val cursors = mutableListOf<String?>()
        val pager = GhostHistoryPager(GhostHistorySource { _, cursor ->
            cursors += cursor
            if (rescan) { if (cursor == null) page(listOf(event("late", "0")), "scan-rest") else gate.await() }
            else if (cursor == null) page(listOf(event("expired", "1"), event("kept", "2"))) else page(listOf(event("kept", "2"), event("new", "3")))
        }, backgroundScope)
        pager.open(selection); runCurrent(); pager.poll(); runCurrent()
        assertEquals("tail", cursors.last()); assertEquals(3, pager.state.value.events.size)
        rescan = true; pager.rescan(); runCurrent()
        assertTrue(pager.state.value.events.any { it.id == "expired" })
        gate.complete(page(listOf(event("kept", "2"), event("new", "3")))); runCurrent()
        assertEquals(listOf("late", "kept", "new"), pager.state.value.events.map { it.id })
    }

    @Test fun twentyPageBatchesExposeContinuationAndDuplicateClicksCannotForkRead() = runTest {
        var calls = 0
        val gate = CompletableDeferred<Unit>()
        val pager = GhostHistoryPager(GhostHistorySource { _, cursor ->
            calls++; if (cursor == null) gate.await()
            val n = cursor?.toInt() ?: 0
            page(listOf(event("event-$n", n.toString().padStart(3, '0'))), if (n < 24) (n + 1).toString() else null)
        }, backgroundScope)
        pager.open(selection); runCurrent(); pager.more(); pager.poll(); pager.rescan(); runCurrent()
        assertEquals(1, calls)
        gate.complete(Unit); runCurrent(); assertEquals(20, calls); assertEquals("20", pager.state.value.nextCursor)
        pager.more(); runCurrent(); assertEquals(25, calls); assertEquals(25, pager.state.value.events.size); assertNull(pager.state.value.nextCursor)
    }

    @Test fun changedOrExpiredObjectRequiresExplicitRescanAndNeverLooksComplete() = runTest {
        for (code in listOf(409, 410)) {
            var fail = false; var calls = 0
            val pager = GhostHistoryPager(GhostHistorySource { _, _ -> calls++; if (fail) throw failure(code) else page(listOf(event("a"))) }, backgroundScope)
            pager.open(selection); runCurrent(); fail = true; pager.poll(); runCurrent()
            assertEquals(code, pager.state.value.errorCode); assertTrue(pager.state.value.rescanRequired)
            assertEquals(1, pager.state.value.events.size)
            pager.poll(); pager.more(); runCurrent(); assertEquals(2, calls)
            fail = false; pager.rescan(); runCurrent(); assertNull(pager.state.value.errorCode); assertFalse(pager.state.value.rescanRequired)
        }
    }

    @Test fun denialClearsAllTextAndStopsFurtherPollingAndRescans() = runTest {
        for (code in listOf(401, 403)) {
            var calls = 0
            val pager = GhostHistoryPager(GhostHistorySource { _, _ -> calls++; if (calls > 1) throw failure(code) else page(listOf(event("private"))) }, backgroundScope)
            pager.open(selection); runCurrent(); pager.poll(); runCurrent()
            assertTrue(pager.state.value.denied); assertTrue(pager.state.value.events.isEmpty()); assertNull(pager.state.value.scope)
            pager.poll(); pager.more(); pager.rescan(); runCurrent(); assertEquals(2, calls)
        }
    }

    @Test fun lateOldAccountAndScopeResponsesCannotEnterNewSelection() = runTest {
        val gate = CompletableDeferred<GhostEventsResponse>()
        val other = GhostHistoryScope("sid-b", "LAPTOP", "provider-b")
        val pager = GhostHistoryPager(GhostHistorySource { selected, _ -> if (selected == selection) gate.await() else page(listOf(event("public-b"))).copy(node_id = other.nodeId, session_id = other.providerSessionId) }, backgroundScope)
        pager.open(selection); runCurrent(); pager.open(other); runCurrent()
        gate.complete(page(listOf(event("private-a")))); runCurrent()
        assertEquals(other, pager.state.value.scope); assertEquals(listOf("public-b"), pager.state.value.events.map { it.id })
    }

    @Test fun changedDuplicateInTheSameScanOrPollIsRejectedWithoutReplacingVisibleText() = runTest {
        val pager = GhostHistoryPager(GhostHistorySource { _, cursor -> page(listOf(event("a", text = if (cursor == null) "Original" else "Changed"))) }, backgroundScope)
        pager.open(selection); runCurrent(); pager.poll(); runCurrent()
        assertEquals(409, pager.state.value.errorCode); assertEquals("Original", pager.state.value.events.single().text)
    }

    @Test fun incompleteAndWrongScopePagesNeverBecomeEmptySuccess() = runTest {
        val pager = GhostHistoryPager(GhostHistorySource { _, _ -> page(listOf(event("a"))).copy(session_id = "other") }, backgroundScope)
        pager.open(selection); runCurrent(); assertEquals(0, pager.state.value.errorCode); assertFalse(pager.state.value.loaded); assertTrue(pager.state.value.events.isEmpty())
        val malformed = GhostHistoryPager(GhostHistorySource { _, _ -> throw failure(422) }, backgroundScope)
        malformed.open(selection); runCurrent(); assertEquals(422, malformed.state.value.errorCode); assertFalse(malformed.state.value.loaded)
    }
}
