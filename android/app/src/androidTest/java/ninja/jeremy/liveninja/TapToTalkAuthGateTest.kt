package ninja.jeremy.liveninja

import android.Manifest
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
 * The isolated test app has no signed-in account. Even when a screen is hosted directly,
 * bypassing the navigation gate, a real tap must fail at the coordinator's auth guard.
 * RealtimeSessionCoordinatorTest separately verifies zero bootstrap/transport calls and
 * signed-in starts using account-bound fake providers. No real provider is needed here.
 *
 * Connecting is transient and is not promised to a signed-out caller. Keep the Compose clock
 * running and assert the stable error, rather than freezing a frame and racing auth rejection.
 */
@RunWith(AndroidJUnit4::class)
class TapToTalkAuthGateTest {
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
    fun signedOutTapShowsAuthenticationErrorInsteadOfStartingVoice() {
        composeTestRule.setContent {
            LiveNinjaTheme { ConversationScreen() }
        }
        composeTestRule.waitForIdle()
        composeTestRule.onNodeWithContentDescription(
            composeTestRule.activity.getString(R.string.conversation_mic_button_cd),
        ).performClick()
        composeTestRule.waitForIdle()

        composeTestRule.onNodeWithText("Sign in before starting a conversation.").assertExists()
        composeTestRule.onNodeWithText(
            composeTestRule.activity.getString(R.string.conversation_state_listening),
        ).assertDoesNotExist()
        composeTestRule.onNodeWithText(
            composeTestRule.activity.getString(R.string.conversation_state_connecting),
        ).assertDoesNotExist()
    }
}
