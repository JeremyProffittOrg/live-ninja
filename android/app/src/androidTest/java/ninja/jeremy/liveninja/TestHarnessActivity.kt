package ninja.jeremy.liveninja

import android.os.Bundle
import androidx.activity.ComponentActivity
import dagger.hilt.android.AndroidEntryPoint

/**
 * WS-5 M24.2 instrumented-test harness. Hosts one screen composable directly (bypassing
 * [LiveNinjaRoot][ninja.jeremy.liveninja.ui.LiveNinjaRoot]'s onboarding/sign-in gate) so a
 * screen that lives behind real Login-with-Amazon auth — like ConversationScreen — can still
 * be exercised offline in CI, where no signed-in account exists.
 *
 * Annotated `@AndroidEntryPoint` (not a plain `ComponentActivity`) purely so `hiltViewModel()`
 * calls inside the hosted screen resolve against the real Hilt graph, exactly like
 * MainActivity's. Test-only: declared in `src/androidTest/AndroidManifest.xml`, so it never
 * ships in a release or debug APK. Content is supplied per test via
 * `composeTestRule.setContent { ... }`.
 */
@AndroidEntryPoint
class TestHarnessActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
    }
}
