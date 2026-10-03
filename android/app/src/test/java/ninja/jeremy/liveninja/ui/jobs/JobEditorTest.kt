package ninja.jeremy.liveninja.ui.jobs

import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.net.JobDto
import ninja.jeremy.liveninja.net.JobScheduleDto
import org.junit.Assert.*
import org.junit.Test
import java.time.Instant

class JobEditorTest {
    private fun draft() = JobDraft(title = "Review priorities", instructions = "Choose the three priorities.", timezone = "UTC")
    @Test fun onceUsesSelectedTimezoneAndFractionalOffsets() {
        val schedule = draft().copy(scheduleKind = "once", timezone = "Asia/Kathmandu", date = "2030-07-04", time = "09:15").toInput(Instant.EPOCH).schedule
        assertEquals("2030-07-04T03:30:00Z", schedule.at)
        assertEquals("Asia/Kathmandu", schedule.timezone)
    }
    @Test fun oneTimeDstGapAndOverlapRequireAnExplicitAlternative() {
        val spring = draft().copy(scheduleKind = "once", timezone = "America/New_York", date = "2030-03-10", time = "02:30")
        assertEquals(R.string.jobs_dst_gap, assertThrows(JobValidationException::class.java) { spring.toInput(Instant.EPOCH) }.messageRes)
        val fall = spring.copy(date = "2030-11-03", time = "01:30")
        assertEquals(R.string.jobs_dst_overlap, assertThrows(JobValidationException::class.java) { fall.toInput(Instant.EPOCH) }.messageRes)
    }
    @Test fun pastOnceCanOnlyBeRetainedWhenTheStoredScheduleIsUnchanged() {
        val schedule = JobScheduleDto(kind = "once", timezone = "UTC", at = "2000-01-01T09:15:37Z")
        val original = JobDraft.from(JobDto(id = "job-1", title = "Old reminder", instructions = "Old notes", kind = "reminder", schedule = schedule, version = 2))
        assertEquals(schedule, original.copy(instructions = "Updated notes").toInput().schedule)
        assertEquals(R.string.jobs_time_in_past, assertThrows(JobValidationException::class.java) { original.copy(time = "09:16").toInput() }.messageRes)
    }
    @Test fun manualAndSundayRoundTripWithoutInventingAnOccurrence() {
        assertNull(draft().toInput().schedule.at)
        val sunday = draft().copy(scheduleKind = "weekly", time = "08:30", weekday = 0).toInput().schedule
        assertEquals(0, sunday.weekday)
        assertEquals("08:30", sunday.time)
        val decodedSunday = JobDraft.from(JobDto(schedule = JobScheduleDto(kind = "weekly", timezone = "UTC", time = "08:30")))
        assertEquals(0, decodedSunday.weekday)
    }
    @Test fun utf8LimitsAndMalformedInputsFailBeforeTheNetwork() {
        assertEquals(R.string.jobs_invalid_title, assertThrows(JobValidationException::class.java) { draft().copy(title = "é".repeat(81)).toInput() }.messageRes)
        assertEquals(R.string.jobs_invalid_notes, assertThrows(JobValidationException::class.java) { draft().copy(instructions = "界".repeat(667)).toInput() }.messageRes)
        assertThrows(JobValidationException::class.java) { draft().copy(timezone = "Mars/Olympus").toInput() }
        assertThrows(JobValidationException::class.java) { draft().copy(scheduleKind = "weekly", weekday = 7).toInput() }
    }
}
