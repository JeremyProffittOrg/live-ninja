package ninja.jeremy.liveninja.net

import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class JobsModelsTest {
    private val json = Json { ignoreUnknownKeys = true; explicitNulls = false }

    @Test
    fun backendShapeKeepsReceiptAndUnavailableProviderTruth() {
        val page = json.decodeFromString<JobsListResponse>("""
            {"jobs":[{"id":"job_a","title":"Review quote","instructions":"Check total","kind":"review","status":"active","version":5,
              "schedule":{"kind":"weekly","time":"09:30","timezone":"America/New_York","weekday":0},
              "nextRunAt":"2026-11-01T14:30:00Z","lastRun":{"id":"run_a","jobId":"job_a","kind":"review","status":"waiting_approval",
              "progress":"Waiting for acknowledgment","attempt":1,"generation":2,"approvalExpiresAt":"2026-10-04T12:00:00Z"}}],
             "capabilities":{"reminder":true,"review":true,"scheduling":false,"historyLimit":50,
             "providers":[{"id":"coding","label":"Coding agents","available":false,"reason":"Not connected"}]},"nextCursor":"cursor-2","futureField":"ignored"}
        """.trimIndent())
        val job = page.jobs!!.single()
        assertEquals(5L, job.version)
        assertEquals(0, job.schedule.weekday)
        assertEquals("waiting_approval", job.lastRun!!.status)
        assertNull(job.lastRun!!.result)
        assertEquals(2L, job.lastRun!!.generation)
        assertEquals(false, page.capabilities!!.scheduling)
        assertEquals(false, page.capabilities!!.providers!!.single().available)
        assertEquals("cursor-2", page.nextCursor)
    }

    @Test
    fun absentOrNullHistoryAndCapabilitiesNeverBecomeEnabled() {
        val empty = json.decodeFromString<JobsListResponse>("{}")
        val nullable = json.decodeFromString<JobsListResponse>("""{"jobs":null,"capabilities":null,"nextCursor":null}""")
        val history = json.decodeFromString<JobRunsResponse>("""{"runs":null}""")
        assertNull(empty.capabilities)
        assertNull(nullable.jobs)
        assertNull(nullable.capabilities)
        assertNull(history.runs)
        val partial = json.decodeFromString<JobsCapabilitiesDto>("""{"review":true}""")
        assertNull(partial.scheduling)
        assertNull(partial.reminder)
        assertNull(partial.providers)
    }

    @Test
    fun writeBodyIsFlatAndContainsOnlyEditableFieldsAndRequestFence() {
        val request = JobSaveRequest("Title", "Exact instructions", "review", JobScheduleDto(), "stable-request-1", 9)
        val body = json.parseToJsonElement(json.encodeToString(request)).jsonObject
        assertEquals(setOf("title", "instructions", "kind", "schedule", "requestId", "expectedVersion"), body.keys)
        assertEquals("stable-request-1", body.getValue("requestId").jsonPrimitive.content)
        assertEquals("9", body.getValue("expectedVersion").jsonPrimitive.content)
        assertFalse(body.containsKey("input"))
        assertFalse(body.containsKey("userId"))
        assertFalse(body.containsKey("confirm"))
        val action = json.parseToJsonElement(json.encodeToString(JobActionRequest(9, "stable-request-1"))).jsonObject
        assertTrue(action.keys == setOf("expectedVersion", "requestId"))
    }
}
