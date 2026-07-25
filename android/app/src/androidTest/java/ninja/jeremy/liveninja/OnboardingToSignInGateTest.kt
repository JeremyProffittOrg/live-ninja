package ninja.jeremy.liveninja

import android.content.Context
// NB: assertExists() is a member of SemanticsNodeInteraction, not a top-level extension,
// so it must NOT be imported — importing it is an unresolved reference.
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onNodeWithText
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import ninja.jeremy.liveninja.ui.state.OnboardingStore
import org.junit.Assert.assertTrue
import org.junit.BeforeClass
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

/**
 * WS-5 M24.2: the sign-in gate in [ninja.jeremy.liveninja.ui.LiveNinjaRoot].
 *
 * The invariant under test is the routing decision, which is the part that could regress
 * dangerously: **once onboarding is complete and no session exists, the app must show
 * LoginScreen — never the signed-in home scaffold.** A regression there would expose the home
 * UI to a signed-out user.
 *
 * Deliberately asserted from persisted state rather than by driving the seven wizard screens.
 * An earlier version of this test walked the wizard clicking each decline button in a fixed
 * order, and it failed twice for reasons that were not defects: the permission steps (mic,
 * notifications, battery) are skipped when the permission is already held, so the step order
 * legitimately differs between `adb install -g`, a plain Gradle install, and the CI emulator —
 * CI died looking for "Skip for now" while the local emulator got two steps further and died
 * looking for "Not now". Worse, declining the mic step can raise a real system permission
 * dialog, which is a different window that Compose's semantics tree cannot see or dismiss, so
 * every later interaction silently targets the window behind it. A test that reports defects
 * that do not exist is worse than no test, and driving system dialogs is what
 * [WakeServiceLifecycleTest]-style instrumentation is for, not this gate.
 *
 * Onboarding prefs are written in `@BeforeClass`, not `@Before`: `createAndroidComposeRule`'s
 * `ActivityScenarioRule` launches `MainActivity` (which reads that file) while the `@Rule` is
 * being applied, and that happens before any `@Before` method runs.
 */
@RunWith(AndroidJUnit4::class)
class OnboardingToSignInGateTest {

    @get:Rule
    val composeTestRule = createAndroidComposeRule<MainActivity>()

    companion object {
        @BeforeClass
        @JvmStatic
        fun markOnboardingCompleteAndSignedOut() {
            val context: Context = InstrumentationRegistry.getInstrumentation().targetContext
            // Both the file name and the key come from OnboardingStore's own constants, so a
            // rename breaks the build rather than silently making this test vacuous.
            context.getSharedPreferences(OnboardingStore.PREFS_NAME, Context.MODE_PRIVATE)
                .edit()
                .putBoolean(OnboardingStore.KEY_COMPLETED, true)
                .commit()
        }
    }

    @Test
    fun onboardingCompleteButSignedOutShowsLoginNotHome() {
        val strings = composeTestRule.activity.resources
        composeTestRule.waitForIdle()

        composeTestRule
            .onNodeWithText(strings.getString(R.string.login_tagline))
            .assertExists()

        // And explicitly NOT the home scaffold: its bottom-nav destinations must be absent.
        // Asserting the positive alone would still pass if both were somehow rendered.
        for (destination in listOf(
            R.string.destination_conversation,
            R.string.destination_history,
            R.string.destination_settings,
        )) {
            val label = strings.getString(destination)
            assertTrue(
                "signed-out user must not see the home destination \"$label\"",
                composeTestRule.onAllNodesWithText(label).fetchSemanticsNodes().isEmpty(),
            )
        }
    }
}
