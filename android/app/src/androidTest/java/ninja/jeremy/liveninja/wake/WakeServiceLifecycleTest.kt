package ninja.jeremy.liveninja.wake

import android.Manifest
import android.content.Context
import androidx.test.ext.junit.rules.ActivityScenarioRule
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import ninja.jeremy.liveninja.TestHarnessActivity
import org.junit.After
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

/**
 * WS-5 M21.0 + M21.4 instrumented regression test: the always-listening switch has to reach
 * an actually-running foreground service on real Android, not just a persisted intent. Only
 * a real device/emulator can prove this — [WakeWordService] is a framework [android.app.Service]
 * that a JVM unit test cannot start.
 *
 * M21.0 was exactly this call failing on a fresh install: the only two callers of
 * [WakeWordService.start] were both gated on [WakePreferences.serviceEnabled], a flag only
 * the service itself ever set true from inside its own `onStartCommand` — an unreachable
 * cycle. This test drives the identical call the Settings switch makes
 * ([WakeWordService.start]) from a state that explicitly reproduces a fresh install
 * (`serviceEnabled` cleared to false first) and asserts the service reports itself running
 * via [WakeWordService.runningFlow] — the WS-5 M21.4 seam the Settings switch reads.
 *
 * Started from [TestHarnessActivity] (genuinely resumed/visible, via [ActivityScenarioRule])
 * rather than the bare instrumentation context, so the call matches exactly how the real
 * Settings switch invokes it (`WakeWordService.start(LocalContext.current)` inside a
 * composable) and isn't relying on any instrumentation-process exemption from Android 12+'s
 * background foreground-service-start restrictions.
 */
@RunWith(AndroidJUnit4::class)
class WakeServiceLifecycleTest {

    @get:Rule
    val activityRule = ActivityScenarioRule(TestHarnessActivity::class.java)

    private val instrumentation = InstrumentationRegistry.getInstrumentation()
    private val context: Context = instrumentation.targetContext

    @Before
    fun resetWakePrefsAndGrantMic() {
        // Mirrors WakePreferences' own SharedPreferences file name — cleared so this test
        // proves the fresh-install case (serviceEnabled starts false) rather than riding on
        // whatever a previous test run left behind on the same emulator.
        context.getSharedPreferences("wake", Context.MODE_PRIVATE).edit().clear().commit()
        instrumentation.uiAutomation.grantRuntimePermission(context.packageName, Manifest.permission.RECORD_AUDIO)
    }

    @After
    fun stopService() {
        WakeWordService.stop(context)
        runBlocking {
            runCatching { withTimeout(5_000) { WakeWordService.runningFlow.first { !it } } }
        }
    }

    @Test
    fun startingTheServiceFromAFreshInstallMakesRunningFlowReportTrue() {
        activityRule.scenario.onActivity { activity -> WakeWordService.start(activity) }

        val becameRunning = runBlocking {
            withTimeout(10_000) { WakeWordService.runningFlow.first { it } }
        }
        assertTrue(becameRunning)
    }
}
