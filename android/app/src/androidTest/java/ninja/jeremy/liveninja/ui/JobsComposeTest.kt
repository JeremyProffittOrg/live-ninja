package ninja.jeremy.liveninja.ui

import android.graphics.Bitmap
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.width
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.asAndroidBitmap
import androidx.compose.ui.test.*
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.unit.dp
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
    private fun show(state: JobsUiState, onEvent: (JobsEvent) -> Unit = {}) { rule.setContent { LiveNinjaTheme { Box(Modifier.width(360.dp)) { JobsContent(state, onEvent) } } } }
    private fun text(resource: Int) = rule.activity.getString(resource)
    private fun capture(name: String, tag: String = "jobs-screen") {
        val target = File(rule.activity.cacheDir, name)
        target.outputStream().use { rule.onNodeWithTag(tag).captureToImage().asAndroidBitmap().compress(Bitmap.CompressFormat.PNG, 100, it) }
    }

    @Test fun emptyStateAndNativeNavigationExposeJobs() {
        var event: JobsEvent? = null
        show(JobsUiState(loaded = true, capabilities = capabilities)) { event = it }
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
}
