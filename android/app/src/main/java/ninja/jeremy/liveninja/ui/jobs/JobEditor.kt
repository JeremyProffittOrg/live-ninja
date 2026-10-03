package ninja.jeremy.liveninja.ui.jobs

import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.net.JobDto
import ninja.jeremy.liveninja.net.JobInputDto
import ninja.jeremy.liveninja.net.JobScheduleDto
import java.time.Instant
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.LocalTime
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.time.format.FormatStyle

data class JobDraft(
    val id: String? = null,
    val version: Long = 0,
    val title: String = "",
    val instructions: String = "",
    val kind: String = "reminder",
    val scheduleKind: String = "manual",
    val timezone: String = ZoneId.systemDefault().id,
    val date: String = LocalDate.now().plusDays(1).toString(),
    val time: String = "09:00",
    val weekday: Int = 1,
    val originalSchedule: JobScheduleDto? = null,
) {
    companion object {
        fun from(job: JobDto): JobDraft {
            val schedule = job.schedule
            val zone = runCatching { ZoneId.of(schedule.timezone ?: "UTC") }.getOrDefault(ZoneId.of("UTC"))
            val once = schedule.at?.let { runCatching { Instant.parse(it).atZone(zone) }.getOrNull() }
            return JobDraft(
                id = job.id, version = job.version, title = job.title, instructions = job.instructions,
                kind = job.kind, scheduleKind = if (schedule.kind == "once" && schedule.at.isNullOrBlank()) "manual" else schedule.kind,
                timezone = zone.id, date = once?.toLocalDate()?.toString() ?: LocalDate.now(zone).plusDays(1).toString(),
                time = once?.toLocalTime()?.format(DateTimeFormatter.ofPattern("HH:mm")) ?: schedule.time ?: "09:00",
                weekday = schedule.weekday ?: if (schedule.kind == "weekly") 0 else 1,
                originalSchedule = schedule,
            )
        }
    }
}

class JobValidationException(val messageRes: Int) : IllegalArgumentException()

/** Civil-time conversion rejects clock gaps and overlaps instead of moving a commitment. */
fun JobDraft.toInput(now: Instant = Instant.now()): JobInputDto {
    val cleanTitle = title.trim()
    val cleanNotes = instructions.trim()
    if (cleanTitle.isBlank() || cleanTitle.toByteArray(Charsets.UTF_8).size > 160) throw JobValidationException(R.string.jobs_invalid_title)
    if (cleanNotes.isBlank() || cleanNotes.toByteArray(Charsets.UTF_8).size > 2000) throw JobValidationException(R.string.jobs_invalid_notes)
    if (kind !in listOf("reminder", "review")) throw JobValidationException(R.string.jobs_unavailable)
    val zone = runCatching { ZoneId.of(timezone) }.getOrElse { throw JobValidationException(R.string.jobs_invalid_timezone) }
    val schedule = when (scheduleKind) {
        "manual" -> JobScheduleDto(kind = "once", timezone = zone.id)
        "once" -> {
            val civil = runCatching { LocalDateTime.of(LocalDate.parse(date), LocalTime.parse(time)) }
                .getOrElse { throw JobValidationException(R.string.jobs_invalid_datetime) }
            val originalInstant = originalSchedule?.at?.let { runCatching { Instant.parse(it) }.getOrNull() }
            val unchanged = id != null && originalSchedule?.kind == "once" && originalSchedule.timezone == zone.id &&
                originalInstant?.atZone(zone)?.toLocalDateTime()?.withSecond(0)?.withNano(0) == civil
            if (unchanged) originalSchedule!! else {
                val offsets = zone.rules.getValidOffsets(civil)
                if (offsets.isEmpty()) throw JobValidationException(R.string.jobs_dst_gap)
                if (offsets.size > 1) throw JobValidationException(R.string.jobs_dst_overlap)
                val instant = civil.toInstant(offsets.single())
                if (!instant.isAfter(now)) throw JobValidationException(R.string.jobs_time_in_past)
                JobScheduleDto(kind = "once", timezone = zone.id, at = instant.toString())
            }
        }
        "daily", "weekdays", "weekly" -> {
            val parsed = runCatching { LocalTime.parse(time) }.getOrElse { throw JobValidationException(R.string.jobs_invalid_datetime) }
            if (weekday !in 0..6) throw JobValidationException(R.string.jobs_invalid_datetime)
            JobScheduleDto(kind = scheduleKind, timezone = zone.id, time = parsed.format(DateTimeFormatter.ofPattern("HH:mm")), weekday = if (scheduleKind == "weekly") weekday else null)
        }
        else -> throw JobValidationException(R.string.jobs_invalid_datetime)
    }
    return JobInputDto(title = cleanTitle, instructions = cleanNotes, kind = kind, schedule = schedule)
}

fun jobDateLabel(value: String?, timezone: String? = null): String = value?.let {
    runCatching {
        val zone = timezone?.let(ZoneId::of) ?: ZoneId.systemDefault()
        DateTimeFormatter.ofLocalizedDateTime(FormatStyle.MEDIUM, FormatStyle.SHORT).format(Instant.parse(it).atZone(zone))
    }.getOrNull()
} ?: "—"

fun jobStatusLabel(status: String): Int = when (status) {
    "active" -> R.string.jobs_active
    "paused" -> R.string.jobs_paused
    "cancelled" -> R.string.jobs_cancelled
    "queued" -> R.string.jobs_queued
    "running" -> R.string.jobs_running
    "waiting_approval" -> R.string.jobs_needs_review
    "succeeded" -> R.string.jobs_completed
    "failed" -> R.string.jobs_failed
    else -> R.string.jobs_unknown
}
