package ninja.jeremy.liveninja

import android.Manifest
// NB: assertExists() is a member of SemanticsNodeInteraction, not a top-level extension,
// so it must NOT be imported — importing it is an unresolved reference.
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onNodeWithContentDescription
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import ninja.jeremy.liveninja.ui.screens.ConversationScreen
import ninja.jeremy.liveninja.ui.theme.LiveNinjaTheme
import org.junit.Before
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

/**
 * WS-5 M24.2 instrumented test for "tap-to-talk -> live session".
 *
 * Boundary: reaching a genuinely LISTENING session needs a live OpenAI/Nova/Gemini realtime
 * backend plus a signed-in account — neither exists in CI, and this test must not depend on
 * either succeeding or the run would be flaky against real network conditions. What IS real
 * and offline-verifiable is that tapping the tap-to-talk affordance reaches
 * [ninja.jeremy.liveninja.ui.conversation.ConversationViewModel.startSession] and the screen
 * synchronously reflects `MicUiState.CONNECTING` — proving the gesture is wired to the real
 * view model and mic-state machine, not a compose function that renders but never reacts to a
 * tap. The test stops there; it never asserts LISTENING.
 *
 * Hosted on [TestHarnessActivity] (not [MainActivity]) because ConversationScreen normally
 * sits behind [ninja.jeremy.liveninja.ui.LiveNinjaRoot]'s onboarding/sign-in gate, and there
 * is no signed-in account to pass that gate with in CI.
 */
@RunWith(AndroidJUnit4::class)
class TapToTalkConnectingStateTest {

    @get:Rule
    val composeTestRule = createAndroidComposeRule<TestHarnessActivity>()

    @Before
    fun grantMicPermission() {
        val instrumentation = InstrumentationRegistry.getInstrumentation()
        instrumentation.uiAutomation.grantRuntimePermission(
            instrumentation.targetContext.packageName,
            Manifest.permission.RECORD_AUDIO,
        )
    }

    @Test
    fun tappingTheOrbMovesMicStateToConnecting() {
        composeTestRule.setContent {
            LiveNinjaTheme { ConversationScreen() }
        }

        composeTestRule
            .onNodeWithContentDescription("Tap to talk. Starts a live voice conversation.")
            .performClick()

        composeTestRule.waitForIdle()

        val connectingLabel = composeTestRule.activity.getString(R.string.conversation_state_connecting)
        composeTestRule.onNodeWithText(connectingLabel).assertExists()
    }
}
