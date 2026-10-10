package ninja.jeremy.liveninja.ui.settings

import android.content.Context
import android.content.SharedPreferences
import io.mockk.every
import io.mockk.mockk
import ninja.jeremy.liveninja.realtime.CarAudioFailure
import ninja.jeremy.liveninja.realtime.CarAudioPreferences
import ninja.jeremy.liveninja.realtime.SessionBusyGate
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class CarAudioSettingsTest {
    private val mic = "android.permission.RECORD_AUDIO"
    private val bt = "android.permission.BLUETOOTH_CONNECT"

    private inner class Harness(var busy: Boolean = false, var enabled: Boolean = false) {
        val granted = mutableSetOf<String>()
        val writes = mutableListOf<Boolean>()
        val controller = CarAudioModeController(
            isBusy = { busy },
            isEnabled = { enabled },
            // Models the atomic idle-only commit: rejects while busy, writes
            // only a real change.
            tryCommit = { value ->
                if (busy) {
                    false
                } else {
                    if (enabled != value) {
                        writes += value
                        enabled = value
                    }
                    true
                }
            },
            missingPermissions = { listOf(mic, bt).filterNot { it in granted } },
        )
    }

    @Test
    fun defaultOff_preferenceDefaultIsFalseAndControlShowsOffWithoutPrompt() {
        val prefs = mockk<SharedPreferences>()
        // Returns whatever default the production code passes.
        every { prefs.getBoolean(any(), any()) } answers { secondArg() }
        val context = mockk<Context>()
        every { context.getSharedPreferences(any(), any()) } returns prefs
        val preferences = CarAudioPreferences(context)
        assertFalse(preferences.isEnabled)
        assertFalse(preferences.enabled.value)

        val display = carAudioControlDisplay(false, true, false, false, listOf(mic, bt), null)
        assertFalse(display.checked)
        assertTrue(display.toggleEnabled)
        assertEquals(CarAudioStatus.OFF, display.status)
        assertFalse(display.allowPermissionsVisible)
    }

    @Test
    fun requiredPermissions_bluetoothConnectOnlyOnApi31Plus() {
        assertEquals(listOf(mic), CarAudioPreferences.requiredRuntimePermissions(30))
        assertEquals(listOf(mic, bt), CarAudioPreferences.requiredRuntimePermissions(31))
    }

    @Test
    fun enablingWithMissingPermissions_requestsOnlyMissingAndDoesNotCommit() {
        val h = Harness()
        h.granted += mic
        val decision = h.controller.onToggle(requestedOn = true, localControls = true)
        assertEquals(CarAudioToggleDecision.RequestPermissions(listOf(bt)), decision)
        assertTrue(h.controller.permissionRequestInFlight)
        assertTrue(h.writes.isEmpty())
        assertFalse(h.enabled)
    }

    @Test
    fun denialOrDismissal_leavesOff() {
        val h = Harness()
        h.controller.onToggle(true, true)
        assertEquals(CarAudioPermissionOutcome.LEAVE_OFF_DENIED, h.controller.onPermissionResult(true))
        assertFalse(h.enabled)
        assertTrue(h.writes.isEmpty())

        // Partial grant (mic only) is still a denial.
        h.controller.onToggle(true, true)
        h.granted += mic
        assertEquals(CarAudioPermissionOutcome.LEAVE_OFF_DENIED, h.controller.onPermissionResult(true))
        assertFalse(h.enabled)
    }

    @Test
    fun fullGrant_enablesOnlyAfterRecheckAndWritesOnlyThePreference() {
        val h = Harness()
        h.controller.onToggle(true, true)
        h.granted += listOf(mic, bt)
        assertEquals(CarAudioPermissionOutcome.ENABLE, h.controller.onPermissionResult(true))
        assertTrue(h.enabled)
        // The single side effect is the mode write; nothing starts a session.
        assertEquals(listOf(true), h.writes)
        assertFalse(h.controller.permissionRequestInFlight)
    }

    @Test
    fun revokedPermissions_showNeedsPermissionAndAllowNeverChangesMode() {
        val h = Harness(enabled = true)
        val display = carAudioControlDisplay(true, true, false, false, listOf(bt), null)
        assertTrue(display.checked)
        assertEquals(CarAudioStatus.NEEDS_PERMISSION, display.status)
        assertTrue(display.allowPermissionsVisible)

        assertEquals(listOf(mic, bt), h.controller.onAllowPermissions(true))
        h.granted += listOf(mic, bt)
        assertEquals(CarAudioPermissionOutcome.REFRESH_ONLY, h.controller.onPermissionResult(true))
        assertTrue(h.writes.isEmpty())
        assertNull(Harness(enabled = true).controller.onAllowPermissions(localControls = false))
    }

    @Test
    fun busy_rejectsBothDirectionsAndDisablesToggle() {
        val h = Harness(busy = true)
        h.granted += listOf(mic, bt)
        assertEquals(CarAudioToggleDecision.RejectBusy, h.controller.onToggle(true, true))
        h.enabled = true
        assertEquals(CarAudioToggleDecision.RejectBusy, h.controller.onToggle(false, true))
        assertTrue(h.writes.isEmpty())
        val display = carAudioControlDisplay(true, true, true, false, emptyList(), null)
        assertFalse(display.toggleEnabled)
        assertEquals(CarAudioStatus.BUSY, display.status)
    }

    @Test
    fun remoteScope_rejectsAndDisables() {
        val h = Harness()
        h.granted += listOf(mic, bt)
        assertEquals(CarAudioToggleDecision.RejectRemote, h.controller.onToggle(true, false))
        assertTrue(h.writes.isEmpty())
        val display = carAudioControlDisplay(false, false, false, false, emptyList(), null)
        assertFalse(display.toggleEnabled)
        assertEquals(CarAudioStatus.REMOTE, display.status)
    }

    @Test
    fun resultArrivingDuringSessionOrAfterScopeChange_cannotEnable() {
        val h = Harness()
        h.controller.onToggle(true, true)
        h.busy = true
        h.granted += listOf(mic, bt)
        assertEquals(CarAudioPermissionOutcome.REJECT_BUSY, h.controller.onPermissionResult(true))
        assertFalse(h.enabled)

        val r = Harness()
        r.controller.onToggle(true, true)
        r.granted += listOf(mic, bt)
        assertEquals(CarAudioPermissionOutcome.REJECT_REMOTE, r.controller.onPermissionResult(false))
        assertFalse(r.enabled)
    }

    @Test
    fun busyStartingBetweenDecisionAndCommit_isRejected() {
        // The early read saw idle; the atomic commit then found a start had
        // marked busy and refused without writing.
        var enabled = false
        val writes = mutableListOf<Boolean>()
        val controller = CarAudioModeController(
            isBusy = { false },
            isEnabled = { enabled },
            tryCommit = { false },
            missingPermissions = { emptyList() },
        )
        assertEquals(CarAudioToggleDecision.RejectBusy, controller.onToggle(true, true))
        enabled = true
        assertEquals(CarAudioToggleDecision.RejectBusy, controller.onToggle(false, true))
        assertTrue(writes.isEmpty())
    }

    @Test
    fun busyStartingBetweenPermissionDecisionAndCommit_reportsRejectBusy() {
        var granted = false
        val controller = CarAudioModeController(
            isBusy = { false },
            isEnabled = { false },
            tryCommit = { false },
            missingPermissions = { if (granted) emptyList() else listOf(mic, bt) },
        )
        assertTrue(controller.onToggle(true, true) is CarAudioToggleDecision.RequestPermissions)
        granted = true
        assertEquals(CarAudioPermissionOutcome.REJECT_BUSY, controller.onPermissionResult(true))
        assertFalse(controller.permissionRequestInFlight)
    }

    @Test
    fun realGate_startMarkedBusyAfterDecisionRejectsCommitUntilCleanup() {
        val gate = SessionBusyGate()
        var enabled = false
        val writes = mutableListOf<Boolean>()
        var granted = false
        val controller = CarAudioModeController(
            isBusy = { gate.busy.value },
            isEnabled = { enabled },
            tryCommit = { value ->
                gate.runIfIdle {
                    if (enabled != value) {
                        writes += value
                        enabled = value
                    }
                }
            },
            missingPermissions = { if (granted) emptyList() else listOf(mic, bt) },
        )
        assertTrue(controller.onToggle(true, true) is CarAudioToggleDecision.RequestPermissions)

        // A start marks busy and reads the selection under the gate.
        val selectedAtStart = gate.markBusyAndRead { enabled }
        assertFalse(selectedAtStart)
        granted = true
        assertEquals(CarAudioPermissionOutcome.REJECT_BUSY, controller.onPermissionResult(true))
        assertTrue(writes.isEmpty())
        assertFalse(gate.runIfIdle { writes += true })
        assertTrue(writes.isEmpty())

        // Cleanup completed: the next explicit toggle commits.
        gate.set(false)
        assertEquals(CarAudioToggleDecision.Enable, controller.onToggle(true, true))
        assertEquals(listOf(true), writes)
        assertTrue(gate.markBusyAndRead { enabled })
    }

    @Test
    fun strayResultWithoutRequest_neverEnables() {
        val h = Harness()
        h.granted += listOf(mic, bt)
        assertEquals(CarAudioPermissionOutcome.REFRESH_ONLY, h.controller.onPermissionResult(true))
        assertFalse(h.enabled)
    }

    @Test
    fun disable_needsNoPermissions() {
        val h = Harness(enabled = true)
        assertEquals(CarAudioToggleDecision.Disable, h.controller.onToggle(false, true))
        assertEquals(listOf(false), h.writes)
    }

    @Test
    fun unavailableRoute_reportsReason() {
        val display = carAudioControlDisplay(true, true, false, false, emptyList(), CarAudioFailure.NO_DEVICE)
        assertEquals(CarAudioStatus.UNAVAILABLE, display.status)
        assertEquals(CarAudioFailure.NO_DEVICE, display.unavailableReason)
        assertEquals(
            CarAudioStatus.READY,
            carAudioControlDisplay(true, true, false, false, emptyList(), null).status,
        )
    }
}
