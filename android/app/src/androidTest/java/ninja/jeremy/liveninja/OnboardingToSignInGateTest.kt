package ninja.jeremy.liveninja

import android.content.Context
// NB: assertExists() is a member of SemanticsNodeInteraction, not a top-level extension,
// so it must NOT be imported — importing it is an unresolved reference.
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.junit.BeforeClass
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

/**
 * WS-5 M24.2 instrumented test for "onboarding -> signed-in home".
 *
 * Boundary: reaching an actually signed-in home screen needs a live Login-with-Amazon
 * round trip, which does not exist in CI. What this test verifies instead — the part of the
 * flow that is real and offline-verifiable — is that walking through every onboarding step
 * (skipping sign-in, permissions, and the assistant role, exactly as a user declining each
 * prompt would) reaches [ninja.jeremy.liveninja.ui.onboarding.LoginScreen], not a crash and
 * not (incorrectly) the signed-in home scaffold. That routing decision
 * (`OnboardingScreen -> LoginScreen -> home`) lives in
 * [ninja.jeremy.liveninja.ui.LiveNinjaRoot] and is exactly the kind of gate that a regression
 * could silently bypass (showing home to a signed-out user).
 *
 * The instrumentation process may already have `onboarding_completed_v1` set from a prior
 * run on the same emulator (no reinstall between local runs); the onboarding prefs file is
 * cleared in `@BeforeClass` — deliberately NOT `@Before` — so the test is reproducible
 * regardless of prior state. `createAndroidComposeRule`'s `ActivityScenarioRule` launches
 * `MainActivity` (reading that prefs file) as part of applying the `@Rule`, which happens
 * before any `@Before` method runs; only a class-level `@BeforeClass` is guaranteed to run
 * ahead of it.
 */
@RunWith(AndroidJUnit4::class)
class OnboardingToSignInGateTest {

    @get:Rule
    val composeTestRule = createAndroidComposeRule<MainActivity>()

    companion object {
        @BeforeClass
        @JvmStatic
        fun resetOnboardingState() {
            val context: Context = InstrumentationRegistry.getInstrumentation().targetContext
            // Mirrors ninja.jeremy.liveninja.ui.state.OnboardingStore's backing file/key.
            context.getSharedPreferences("liveninja_onboarding", Context.MODE_PRIVATE)
                .edit()
                .clear()
                .commit()
        }
    }

    @Test
    fun skippingEveryStepReachesTheSignInScreen() {
        val strings = composeTestRule.activity.resources

        // WELCOME
        composeTestRule.onNodeWithText(strings.getString(R.string.onboarding_get_started)).performClick()

        // SIGN_IN (declined — a fresh install has no Amazon session to reuse)
        composeTestRule.onNodeWithText(strings.getString(R.string.onboarding_skip_for_now)).performClick()

        // MIC_PERMISSION (declined)
        composeTestRule.onNodeWithText(strings.getString(R.string.onboarding_mic_not_now)).performClick()

        // NOTIFICATIONS (declined)
        composeTestRule.onNodeWithText(strings.getString(R.string.onboarding_skip_for_now)).performClick()

        // ASSISTANT_ROLE (declined)
        composeTestRule.onNodeWithText(strings.getString(R.string.onboarding_role_skip)).performClick()

        // BATTERY (declined)
        composeTestRule.onNodeWithText(strings.getString(R.string.onboarding_skip_for_now)).performClick()

        // WAKE_WORD -> Finish setup (default selection is fine; this screen never broke).
        composeTestRule.onNodeWithText(strings.getString(R.string.onboarding_finish)).performClick()

        // The wizard is done and the user is signed out: LoginScreen, never the home scaffold.
        composeTestRule
            .onNodeWithText(strings.getString(R.string.login_tagline))
            .assertExists()
    }
}
