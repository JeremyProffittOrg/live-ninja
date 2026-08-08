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
        composeTestRule.waitForIdle()

        // CONNECTING is a TRANSIENT state, and the frame that shows it must be pinned down
        // before asserting on it. startSession() sets MicUiState.CONNECTING synchronously in the
        // click handler, then launches a coroutine that fetches a realtime session. In CI that
        // fetch always fails, and its failure overwrites the state with ERROR.
        //
        // The old assertion was `waitForIdle()` then assertExists, which is a race against that
        // network round trip — and it lost whenever the HTTP stack was already warm. That is the
        // order-dependence bisected to OnboardingToSignInGateTest: it is the only other class
        // that launches the real MainActivity, so it warms DNS/TLS/the OkHttp pool and the
        // failure comes back fast enough to win. Run alone the round trip is cold and slow, the
        // assertion wins, and the test passes — by luck, not by construction. Diagnosed by
        // dumping the semantics tree at the point of failure, which showed the screen already
        // rendering "Couldn't start the conversation" / "Forbidden" rather than nothing at all.
        //
        // Stopping the clock removes the race instead of hiding it: no recomposition can happen
        // until this test asks for one, so exactly the frame produced by the tap is rendered and
        // asserted. Whatever the network does afterwards cannot reach the screen first.
        composeTestRule.mainClock.autoAdvance = false

        composeTestRule
            .onNodeWithContentDescription(
                composeTestRule.activity.getString(R.string.conversation_mic_button_cd),
            )
            .performClick()

        composeTestRule.mainClock.advanceTimeByFrame()

        val connectingLabel = composeTestRule.activity.getString(R.string.conversation_state_connecting)
        composeTestRule.onNodeWithText(connectingLabel).assertExists()
    }
}
