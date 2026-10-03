package ninja.jeremy.liveninja.ui

import android.graphics.Bitmap
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.width
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.asAndroidBitmap
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.test.*
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.unit.Density
import androidx.compose.ui.unit.dp
import androidx.test.ext.junit.runners.AndroidJUnit4
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.TestHarnessActivity
import ninja.jeremy.liveninja.net.*
import ninja.jeremy.liveninja.ui.jobs.*
import ninja.jeremy.liveninja.ui.screens.GhostExplorerContent
import ninja.jeremy.liveninja.ui.theme.LiveNinjaTheme
import org.junit.Assert.*
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import java.io.File

/** Controlled fixtures: no provider account, archive download, or production network. */
@RunWith(AndroidJUnit4::class)
class GhostExplorerComposeTest {
    @get:Rule val rule = createAndroidComposeRule<TestHarnessActivity>()
    private fun text(id: Int) = rule.activity.getString(id)
    private fun show(state: GhostExplorerState, scale: Float = 1f, onEvent: (GhostExplorerEvent) -> Unit = {}) {
        rule.setContent { LiveNinjaTheme { CompositionLocalProvider(LocalDensity provides Density(LocalDensity.current.density, scale)) { Box(Modifier.width(360.dp)) { GhostExplorerContent(state, onEvent, {}) } } } }
    }
    private fun capture(name: String) { File(rule.activity.cacheDir, name).outputStream().use { rule.onNodeWithTag("ghost-explorer").captureToImage().asAndroidBitmap().compress(Bitmap.CompressFormat.PNG, 100, it) } }

    @Test fun providerInventoryDisclosesMetadataLimitAndNeverInfersAConversationLink() {
        var selected: GhostExplorerEvent? = null
        show(GhostExplorerState(nodes = listOf(GhostNodeDto("FIXTURE-OFFICEPC", state = "live")), jobs = listOf(GhostScheduledEventDto("fixture-schedule", node = "FIXTURE-OFFICEPC", repo = "Fixture repository", enabled = true, last_run_status = "succeeded")), loaded = true, runHistoryLimit = 10)) { selected = it }
        rule.onNodeWithText(text(R.string.ghost_owner_boundary)).assertIsDisplayed()
        rule.onNodeWithTag("ghost-content").performScrollToNode(hasText(rule.activity.getString(R.string.ghost_run_limit, 10)))
        rule.onNodeWithText(rule.activity.getString(R.string.ghost_run_limit, 10)).assertExists()
        rule.onNodeWithTag("ghost-content").performScrollToNode(hasText(text(R.string.ghost_inspect_job)))
        rule.onNodeWithText(text(R.string.ghost_inspect_job)).performClick()
        rule.onNodeWithTag("ghost-content").performScrollToNode(hasText(text(R.string.ghost_no_binding)))
        rule.onNodeWithText(text(R.string.ghost_no_binding)).assertExists()
        assertNull(selected)
        capture("ghost-native-inventory.png")
    }

    @Test fun actualSessionSelectionRemainsExplicitAtLargeFontScale() {
        var selected: GhostExplorerEvent? = null
        show(GhostExplorerState(nodeId = "FIXTURE-OFFICEPC", sessions = listOf(GhostSessionDto("fixture-provider-session", "2030-01-01")), sessionsCursor = "more"), scale = 2f) { selected = it }
        rule.onNodeWithTag("ghost-content").performScrollToNode(hasText("fixture-provider-session"))
        rule.onNodeWithText("fixture-provider-session").performClick()
        assertEquals(GhostExplorerEvent.Session("fixture-provider-session"), selected)
        rule.onNodeWithTag("ghost-content").performScrollToNode(hasText(text(R.string.ghost_more_sessions)))
        rule.onNodeWithText(text(R.string.ghost_more_sessions)).assertIsEnabled()
    }

    @Test fun archivePreservesFullTextAndMissingTimestampAndGapNotices() {
        val archive = GhostArchiveState(scope = GhostHistoryScope("fixture-account", "FIXTURE-OFFICEPC", "fixture-provider-session"), events = listOf(GhostHistoryEventDto("fixture-event", "001", kind = "tool_result", text = "Retained fixture output. ".repeat(700) + "FULL FIXTURE OUTPUT END", more = true)), gaps = listOf("Fixture missing object"), loaded = true, resumeCursor = "tail")
        show(GhostExplorerState(nodeId = "FIXTURE-OFFICEPC", archive = archive))
        rule.onNodeWithTag("ghost-content").performScrollToNode(hasText("provider retains session archives for 90 days", substring = true))
        rule.onNodeWithText("provider retains session archives for 90 days", substring = true).assertExists()
        rule.onNodeWithTag("ghost-content").performScrollToNode(hasText(text(R.string.ghost_time_unknown)))
        rule.onNodeWithText(text(R.string.ghost_time_unknown)).assertExists()
        capture("ghost-native-archive.png")
        rule.onNodeWithTag("ghost-content").performScrollToNode(hasText("FULL FIXTURE OUTPUT END", substring = true))
        rule.onNodeWithText("FULL FIXTURE OUTPUT END", substring = true).assertExists()
        rule.onNodeWithTag("ghost-content").performScrollToNode(hasText(rule.activity.getString(R.string.ghost_gap, "Fixture missing object")))
        rule.onNodeWithText(rule.activity.getString(R.string.ghost_gap, "Fixture missing object")).assertExists()
    }

    @Test fun deniedStateNeverDisplaysProviderContentAndDisablesRefresh() {
        show(GhostExplorerState(nodes = listOf(GhostNodeDto("private-node")), jobs = listOf(GhostScheduledEventDto("private-job")), denied = true, errorCode = 403))
        rule.onNodeWithText(text(R.string.ghost_access_denied)).assertIsDisplayed()
        rule.onNodeWithText("private-node").assertDoesNotExist(); rule.onNodeWithText("private-job").assertDoesNotExist()
        rule.onNodeWithText(text(R.string.jobs_refresh)).assertIsNotEnabled()
    }

    @Test fun expiredArchiveExplicitlyRequiresRescanAndNeverClaimsAnEnd() {
        show(GhostExplorerState(nodeId = "FIXTURE-OFFICEPC", archive = GhostArchiveState(scope = GhostHistoryScope("fixture-account", "FIXTURE-OFFICEPC", "fixture-provider-session"), loaded = true, errorCode = 410, rescanRequired = true)))
        rule.onNodeWithText(text(R.string.ghost_missing_archive)).assertExists()
        rule.onNodeWithTag("ghost-content").performScrollToNode(hasText(text(R.string.ghost_rescan)))
        rule.onNodeWithText(text(R.string.ghost_rescan)).assertIsEnabled()
        rule.onNodeWithText(text(R.string.ghost_loaded_end)).assertDoesNotExist()
        rule.onNodeWithText(text(R.string.ghost_empty_archive)).assertDoesNotExist()
    }
}
