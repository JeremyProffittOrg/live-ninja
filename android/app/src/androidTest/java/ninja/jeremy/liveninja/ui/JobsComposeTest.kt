package ninja.jeremy.liveninja.ui

import android.graphics.Bitmap
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.width
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.asAndroidBitmap
import androidx.compose.ui.test.*
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.Density
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.runtime.CompositionLocalProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.TestHarnessActivity
import ninja.jeremy.liveninja.net.*
import ninja.jeremy.liveninja.ui.jobs.*
import ninja.jeremy.liveninja.ui.screens.JobsContent
import ninja.jeremy.liveninja.ui.theme.LiveNinjaTheme
import org.junit.Assert.*
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import java.io.File

/** Controlled native UI states only. No production auth, network, tools or device services. */
@RunWith(AndroidJUnit4::class)
class JobsComposeTest {
    @get:Rule val rule = createAndroidComposeRule<TestHarnessActivity>()
    private val capabilities = JobsCapabilitiesDto(reminder = true, review = true, scheduling = true)
    private val job = JobDto(id = "job-test", title = "Review the weekly priorities", instructions = "Choose the three things that matter most this week.", kind = "review", status = "active", version = 2, schedule = JobScheduleDto(kind = "weekly", timezone = "America/New_York", time = "09:00", weekday = 1), nextRunAt = "2030-01-07T14:00:00Z")
    private val run = JobRunDto(id = "run-test", jobId = job.id, title = "Original checkpoint", instructions = "Review this exact saved text. Do not send any email.", kind = "review", status = "waiting_approval", attempt = 1, createdAt = "2030-01-01T10:00:00Z", progress = "Waiting for your acknowledgement. No external action will run.")
    private fun show(state: JobsUiState, fontScale: Float = 1f, onEvent: (JobsEvent) -> Unit = {}) { rule.setContent { LiveNinjaTheme { CompositionLocalProvider(LocalDensity provides Density(LocalDensity.current.density, fontScale)) { Box(Modifier.width(360.dp)) { JobsContent(state, onEvent) } } } } }
    private fun text(resource: Int) = rule.activity.getString(resource)
    private fun capture(name: String, tag: String = "jobs-screen") {
        val target = File(rule.activity.cacheDir, name)
        target.outputStream().use { rule.onNodeWithTag(tag).captureToImage().asAndroidBitmap().compress(Bitmap.CompressFormat.PNG, 100, it) }
    }

    @Test fun emptyStateAndNativeNavigationExposeJobs() {
        var event: JobsEvent? = null
        show(JobsUiState(loaded = true, capabilities = capabilities)) { event = it }
        rule.onNodeWithTag("jobs-list").performScrollToNode(hasText(text(R.string.jobs_empty)))
        rule.onNodeWithText(text(R.string.jobs_empty)).assertIsDisplayed()
        rule.onNodeWithText(text(R.string.jobs_new)).performClick()
        assertEquals(JobsEvent.New, event)
        assertEquals("jobs", TopLevelDestination.JOBS.route)
        assertEquals(R.string.destination_jobs, TopLevelDestination.JOBS.labelRes)
    }

    @Test fun reviewDialogUsesImmutableReceiptAndAnExplicitConfirmation() {
        var event: JobsEvent? = null
        show(JobsUiState(loaded = true, capabilities = capabilities, selected = job.copy(instructions = "New job text"), runs = listOf(run), confirmation = JobConfirmation(job, "approve", run))) { event = it }
        rule.onNodeWithText("Review this exact saved text. Do not send any email.").assertIsDisplayed()
        rule.onNodeWithText(text(R.string.jobs_confirm_review_body)).assertIsDisplayed()
        capture("jobs-native-review.png", "jobs-confirmation")
        rule.onNodeWithTag("jobs-confirm").performClick()
        assertEquals(JobsEvent.Confirm, event)
    }

    @Test fun pendingApprovalDisablesRepeatedConfirmAndDismiss() {
        show(JobsUiState(loaded = true, capabilities = capabilities, selected = job, busy = true, confirmation = JobConfirmation(job, "approve", run)))
        rule.onNodeWithTag("jobs-confirm").assertIsNotEnabled()
        rule.onNodeWithText(text(R.string.jobs_go_back)).assertIsNotEnabled()
    }

    @Test fun staleEditorKeepsInputAndRequiresExplicitReload() {
        show(JobsUiState(capabilities = capabilities, draft = JobDraft(title = "Keep my unsaved title", instructions = "Keep these notes"), draftConflict = true, draftError = R.string.jobs_conflict))
        rule.onNodeWithTag("job-title").assertTextContains("Keep my unsaved title")
        rule.onNodeWithText(text(R.string.jobs_save)).assertIsNotEnabled()
        rule.onNodeWithText(text(R.string.jobs_reload_draft)).performScrollTo().assertIsDisplayed()
        capture("jobs-native-editor.png", "job-editor")
    }

    @Test fun unavailableSchedulesCannotBeSelectedInTheEditor() {
        show(JobsUiState(capabilities = capabilities.copy(scheduling = false), draft = JobDraft(title = "Manual job", instructions = "Notes")))
        rule.onNodeWithText(text(R.string.jobs_manual)).performScrollTo().performClick()
        rule.onNodeWithText(text(R.string.jobs_daily)).assertIsNotEnabled()
    }

    @Test fun detailShowsTruthfulProgressAndRunControlsOnNarrowScreen() {
        var event: JobsEvent? = null
        show(JobsUiState(loaded = true, capabilities = capabilities, selected = job, runs = listOf(run), runsLoaded = true)) { event = it }
        rule.onNodeWithTag("job-detail").performScrollToNode(hasText(text(R.string.jobs_approve)))
        rule.onNodeWithText(run.progress).assertIsDisplayed()
        capture("jobs-native-detail.png")
        rule.onNodeWithText(text(R.string.jobs_approve)).performClick()
        assertEquals(JobsEvent.Action("approve", run), event)
    }

    @Test fun voiceReviewRemainsExplicitAndReadableAtLargeFontScale() {
        val proposal = JobsVoiceProposal("call-a", "sid-a", "job_command", 0, job = job, jobId = job.id, expectedVersion = 2, text = "Exact voice-proposed note")
        var event: JobsEvent? = null
        show(JobsUiState(voiceReview = proposal), fontScale = 2f) { event = it }
        assertNull(event)
        rule.onNodeWithText("Exact voice-proposed note").performScrollTo().assertIsDisplayed()
        rule.onNodeWithTag("jobs-voice-confirm").assertIsEnabled()
        capture("jobs-native-voice-large-font.png", "jobs-voice-review")
        rule.onNodeWithTag("jobs-voice-confirm").performClick()
        assertEquals(JobsEvent.ConfirmVoice, event)
    }

    @Test fun historyKeepsFullLargeOutputAndProvidesOlderAndLatestControls() {
        val fullText = "Retained output. ".repeat(500) + "END OF RETAINED OUTPUT"
        val history = JobHistoryState(scope = JobHistoryScope("sid-a", job.id), entries = listOf(JobHistoryEntry("h1", 1, "system", fullText, "2030-01-01T00:00:00Z")), olderCursor = "older", latestCursor = "latest", retention = HistoryRetention.PARTIAL, retentionMessage = "Recorded since this update")
        show(JobsUiState(loaded = true, selected = job, history = history))
        rule.onNodeWithTag("job-detail").performScrollToNode(hasText("END OF RETAINED OUTPUT", substring = true))
        rule.onNodeWithText("END OF RETAINED OUTPUT", substring = true).assertExists()
        rule.onNodeWithTag("job-detail").performScrollToNode(hasText(text(R.string.jobs_history_older)))
        rule.onNodeWithText(text(R.string.jobs_history_older)).assertIsEnabled()
        capture("jobs-native-retained-history.png")
    }

    @Test fun noteReviewShowsExactSubmittedTextWithoutExecutionClaims() {
        show(JobsUiState(selected = job, noteDraft = "Later changed draft", confirmation = JobConfirmation(job, "note", note = "Original note to save")))
        rule.onNodeWithText("Original note to save").assertIsDisplayed()
        rule.onNode(hasText(text(R.string.jobs_note_boundary)) and hasAnyAncestor(hasTestTag("jobs-confirmation"))).assertIsDisplayed()
        rule.onNodeWithTag("jobs-confirm").assertIsEnabled()
    }

    @Test fun createProposalShowsTheExactHumanReviewJobKind() {
        val proposal = JobsVoiceProposal("call-a", "sid-a", "job_create", 0, input = JobInputDto("Checkpoint", "Exact notes", "review"))
        show(JobsUiState(voiceReview = proposal))
        rule.onNodeWithText("${text(R.string.jobs_field_type)}: ${text(R.string.jobs_review)}").assertIsDisplayed()
        rule.onNodeWithText("Checkpoint").assertIsDisplayed()
        rule.onNodeWithTag("jobs-voice-confirm").assertIsEnabled()
    }
}
