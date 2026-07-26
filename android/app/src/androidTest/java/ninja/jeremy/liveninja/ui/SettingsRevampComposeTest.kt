package ninja.jeremy.liveninja.ui

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.SemanticsProperties
import androidx.compose.ui.test.SemanticsMatcher
import androidx.compose.ui.test.assert
import androidx.compose.ui.test.assertIsNotEnabled
import androidx.compose.ui.test.getUnclippedBoundsInRoot
import androidx.compose.ui.test.hasClickAction
import androidx.compose.ui.test.hasText
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onNodeWithContentDescription
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.test.espresso.Espresso.pressBack
import androidx.test.ext.junit.runners.AndroidJUnit4
import ninja.jeremy.liveninja.TestHarnessActivity
import ninja.jeremy.liveninja.ui.settings.DEFAULT_EXPANDED_SETTINGS_SECTION
import ninja.jeremy.liveninja.ui.settings.SettingsAccordionCard
import ninja.jeremy.liveninja.ui.settings.SettingsSection
import ninja.jeremy.liveninja.ui.settings.SettingsHostUi
import ninja.jeremy.liveninja.ui.settings.SettingsSectionScopeUi
import ninja.jeremy.liveninja.ui.screens.SettingsDeviceScopeControl
import ninja.jeremy.liveninja.ui.settings.toggledSettingsSection
import ninja.jeremy.liveninja.ui.theme.LiveNinjaTheme
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class SettingsRevampComposeTest {

    @get:Rule
    val composeTestRule = createAndroidComposeRule<TestHarnessActivity>()

    @Test
    fun accordionComposesOnlyOnePanelAndOpenHeaderCanCollapse() {
        composeTestRule.setContent {
            LiveNinjaTheme {
                var expanded by remember {
                    mutableStateOf<SettingsSection?>(DEFAULT_EXPANDED_SETTINGS_SECTION)
                }
                val firstFocus = remember { FocusRequester() }
                val secondFocus = remember { FocusRequester() }
                Column {
                    SettingsAccordionCard(
                        title = "About you",
                        description = "Personal context",
                        expanded = expanded == SettingsSection.ABOUT_YOU,
                        expandedStateDescription = "Expanded",
                        collapsedStateDescription = "Collapsed",
                        expandActionLabel = "Expand section",
                        collapseActionLabel = "Collapse section",
                        focusRequester = firstFocus,
                        onToggle = {
                            expanded = toggledSettingsSection(
                                expanded,
                                SettingsSection.ABOUT_YOU,
                            )
                        },
                        onHeaderKeyEvent = { false },
                    ) {
                        androidx.compose.material3.Text("About panel body")
                    }
                    SettingsAccordionCard(
                        title = "Wake word",
                        description = "Conversation start",
                        expanded = expanded == SettingsSection.WAKE_WORD,
                        expandedStateDescription = "Expanded",
                        collapsedStateDescription = "Collapsed",
                        expandActionLabel = "Expand section",
                        collapseActionLabel = "Collapse section",
                        focusRequester = secondFocus,
                        onToggle = {
                            expanded = toggledSettingsSection(
                                expanded,
                                SettingsSection.WAKE_WORD,
                            )
                        },
                        onHeaderKeyEvent = { false },
                    ) {
                        androidx.compose.material3.Text("Wake panel body")
                    }
                }
            }
        }

        val expanded = SemanticsMatcher.expectValue(
            SemanticsProperties.StateDescription,
            "Expanded",
        )
        val collapsed = SemanticsMatcher.expectValue(
            SemanticsProperties.StateDescription,
            "Collapsed",
        )

        composeTestRule.onNodeWithText("About you").assert(expanded)
        composeTestRule.onNodeWithText("About panel body").assertExists()
        composeTestRule.onNodeWithText("Wake word").assert(collapsed)
        composeTestRule.onNodeWithText("Wake panel body").assertDoesNotExist()

        composeTestRule.onNodeWithText("Wake word").performClick()

        composeTestRule.onNodeWithText("About panel body").assertDoesNotExist()
        composeTestRule.onNodeWithText("Wake panel body").assertExists()
        composeTestRule.onNodeWithText("Wake word").assert(expanded).performClick()
        composeTestRule.onNodeWithText("Wake panel body").assertDoesNotExist()
        composeTestRule.onNodeWithText("Wake word").assert(collapsed)
    }

    @Test
    fun matchingEdgeBarsAreFortyPercentOfTheViewport() {
        composeTestRule.setContent {
            LiveNinjaTheme {
                Box(
                    Modifier
                        .fillMaxSize()
                        .testTag("settings-test-viewport"),
                ) {
                    SettingsEdgeBar(
                        edge = SettingsEdge.OPEN,
                        onClick = {},
                        focusRequester = remember { FocusRequester() },
                    )
                    SettingsEdgeBar(
                        edge = SettingsEdge.CLOSE,
                        onClick = {},
                        focusRequester = remember { FocusRequester() },
                    )
                }
            }
        }

        composeTestRule.onNodeWithText("Settings", useUnmergedTree = true).assertExists()
        composeTestRule.onNodeWithText("Close", useUnmergedTree = true).assertExists()
        composeTestRule.onNodeWithContentDescription("Open settings").assertExists()
        composeTestRule.onNodeWithContentDescription("Close settings").assertExists()

        val viewportBounds = composeTestRule
            .onNodeWithTag("settings-test-viewport")
            .getUnclippedBoundsInRoot()
        val openBounds = composeTestRule
            .onNodeWithTag(SETTINGS_OPEN_BAR_TAG)
            .getUnclippedBoundsInRoot()
        val closeBounds = composeTestRule
            .onNodeWithTag(SETTINGS_CLOSE_BAR_TAG)
            .getUnclippedBoundsInRoot()

        val viewportHeight = (viewportBounds.bottom - viewportBounds.top).value
        val openHeight = (openBounds.bottom - openBounds.top).value
        val closeHeight = (closeBounds.bottom - closeBounds.top).value
        val openWidth = (openBounds.right - openBounds.left).value
        val closeWidth = (closeBounds.right - closeBounds.left).value

        assertEquals(viewportHeight * 0.4f, openHeight, 0.6f)
        assertEquals(openHeight, closeHeight, 0.1f)
        assertEquals(48f, openWidth, 0.1f)
        assertEquals(openWidth, closeWidth, 0.1f)
    }

    @Test
    fun hostScopeShowsValuesAndDisablesInvalidAllHostsInheritance() {
        composeTestRule.setContent {
            LiveNinjaTheme {
                SettingsDeviceScopeControl(
                    section = SettingsSection.ABOUT_YOU,
                    scope = SettingsSectionScopeUi(
                        version = 3,
                        viewedDeviceId = "current",
                        hosts = listOf(
                            SettingsHostUi(
                                id = "current",
                                name = "Kitchen tablet",
                                isCurrent = true,
                                inherited = false,
                                settings = JSONObject().put(
                                    "profile",
                                    JSONObject()
                                        .put("displayName", "Jeremy")
                                        .put("units", "metric"),
                                ),
                            ),
                        ),
                    ),
                    onViewDevice = {},
                    onApply = { _, _ -> },
                )
            }
        }

        composeTestRule.onNodeWithText("Jeremy · metric units").assertExists()
        composeTestRule
            .onNodeWithContentDescription("Apply to… Kitchen tablet")
            .performClick()
        composeTestRule.onNodeWithText("All hosts").performClick()
        composeTestRule.onNodeWithText(
            "Applying to all hosts updates the account default and clears this section’s " +
                "per-host customizations.",
        ).assertExists()
        composeTestRule.onNodeWithText("Use inherited defaults").assertIsNotEnabled()
    }

    @Test
    fun hostPickerKeepsUnsupportedHostVisibleAndExplicitlyDisabled() {
        composeTestRule.setContent {
            LiveNinjaTheme {
                SettingsDeviceScopeControl(
                    section = SettingsSection.APPEARANCE,
                    scope = SettingsSectionScopeUi(
                        version = 3,
                        viewedDeviceId = "current",
                        hosts = listOf(
                            SettingsHostUi(
                                id = "current",
                                name = "Kitchen tablet",
                                isCurrent = true,
                            ),
                            SettingsHostUi(
                                id = "speaker",
                                name = "Bedroom display",
                                capabilities = setOf("privacy"),
                            ),
                        ),
                    ),
                    onViewDevice = {},
                    onApply = { _, _ -> },
                )
            }
        }

        composeTestRule
            .onNode(hasText("Kitchen tablet · This device") and hasClickAction())
            .performClick()
        composeTestRule.onNodeWithText("Bedroom display").assertExists()
        composeTestRule.onNodeWithText("Not supported for this section").assertExists()
        composeTestRule
            .onNode(
                hasText("Bedroom display") and
                    hasText("Not supported for this section"),
            )
            .assertIsNotEnabled()

        pressBack()
        composeTestRule
            .onNodeWithContentDescription("Apply to… Kitchen tablet")
            .performClick()
        composeTestRule
            .onNode(hasText("All hosts") and hasClickAction())
            .assertIsNotEnabled()
    }
}
