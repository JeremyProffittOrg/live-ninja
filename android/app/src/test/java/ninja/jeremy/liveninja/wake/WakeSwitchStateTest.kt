package ninja.jeremy.liveninja.wake

import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * WS-5 M21.0 + M21.4 regression tests for the Settings always-listening switch's pure
 * decision logic (see [wakeSwitchDisplay] / [decideWakeSwitchAction] for the invariant these
 * protect).
 */
class WakeSwitchStateTest {

    @Test
    fun `never enabled and not running shows off`() {
        assertEquals(WakeSwitchDisplay.OFF, wakeSwitchDisplay(serviceEnabled = false, serviceRunning = false))
    }

    @Test
    fun `intent on but not running shows paused with a resume action`() {
        // The M21.4 defect: the switch used to read ON here because it only looked at
        // serviceEnabled. Reality (serviceRunning) must win.
        assertEquals(WakeSwitchDisplay.PAUSED, wakeSwitchDisplay(serviceEnabled = true, serviceRunning = false))
    }

    @Test
    fun `actually running shows running regardless of the persisted intent`() {
        assertEquals(WakeSwitchDisplay.RUNNING, wakeSwitchDisplay(serviceEnabled = true, serviceRunning = true))
        assertEquals(WakeSwitchDisplay.RUNNING, wakeSwitchDisplay(serviceEnabled = false, serviceRunning = true))
    }

    @Test
    fun `toggling on from a fresh install starts without requiring serviceEnabled already true`() {
        // The M21.0 deadlock: start() was only ever reachable from callers gated on
        // serviceEnabled, a flag only the service itself set true. A fresh install
        // (serviceEnabled=false, serviceRunning=false) toggled on must still resolve to
        // START — and identically so whether serviceEnabled happens to already be true.
        assertEquals(
            WakeSwitchAction.START,
            decideWakeSwitchAction(toggledOn = true, serviceEnabled = false, serviceRunning = false),
        )
        assertEquals(
            WakeSwitchAction.START,
            decideWakeSwitchAction(toggledOn = true, serviceEnabled = true, serviceRunning = false),
        )
    }

    @Test
    fun `tapping resume while paused starts even though the switch itself wasn't toggled on`() {
        // SettingsScreen's Resume button calls the same startListening lambda as the
        // switch, without going through onCheckedChange(true) — toggledOn=false here.
        assertEquals(
            WakeSwitchAction.START,
            decideWakeSwitchAction(toggledOn = false, serviceEnabled = true, serviceRunning = false),
        )
    }

    @Test
    fun `toggling off while running stops`() {
        assertEquals(
            WakeSwitchAction.STOP,
            decideWakeSwitchAction(toggledOn = false, serviceEnabled = true, serviceRunning = true),
        )
    }
}
