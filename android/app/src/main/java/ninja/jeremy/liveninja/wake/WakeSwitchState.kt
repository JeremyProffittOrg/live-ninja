package ninja.jeremy.liveninja.wake

/**
 * WS-5 M21.0 + M21.4 pure state machine for the Settings "Always listening" switch.
 *
 * [WakePreferences.serviceEnabled] is the persisted *intent* ("the user wants this on").
 * [WakeWordService.runningFlow] is the observed *reality* ("a service instance is alive right
 * now"). They are allowed to diverge — most commonly right after a reboot, since Android 15+
 * refuses to start a microphone-type foreground service straight from BOOT_COMPLETED, so
 * [WakeBootReceiver] degrades to a tap-to-resume notification instead. The switch must reflect
 * reality, never the bare intent, or it lies exactly the way the M21.4 defect did ("ON while
 * nothing was listening").
 *
 * Mirrors SettingsScreen's inline `wakePaused`/`onCheckedChange` computation exactly, pulled
 * out here so the decision is unit-testable without a Compose or Robolectric harness.
 */
enum class WakeSwitchDisplay {
    /** Never enabled, or the user explicitly stopped it. Switch shows off, no banner. */
    OFF,

    /** A service instance is genuinely alive. Switch shows on. */
    RUNNING,

    /** Intent is on but nothing is actually listening — paused banner + Resume action. */
    PAUSED,
}

fun wakeSwitchDisplay(serviceEnabled: Boolean, serviceRunning: Boolean): WakeSwitchDisplay = when {
    serviceRunning -> WakeSwitchDisplay.RUNNING
    serviceEnabled -> WakeSwitchDisplay.PAUSED
    else -> WakeSwitchDisplay.OFF
}

/** What the switch (or its Resume button) should do next, given a user interaction. */
enum class WakeSwitchAction { START, STOP }

/**
 * Decision for the Settings switch's `onCheckedChange`. Deliberately does NOT require
 * [serviceEnabled] to already be true before allowing a START: gating a start on a flag only
 * [WakeWordService] itself ever sets true (from inside its own onStartCommand) is exactly the
 * M21.0 deadlock — a fresh install had no reachable path to a running service. [toggledOn]
 * alone is sufficient to start, independent of [serviceEnabled]'s current value.
 */
fun decideWakeSwitchAction(
    toggledOn: Boolean,
    serviceEnabled: Boolean,
    serviceRunning: Boolean,
): WakeSwitchAction {
    val paused = wakeSwitchDisplay(serviceEnabled, serviceRunning) == WakeSwitchDisplay.PAUSED
    return if (toggledOn || paused) WakeSwitchAction.START else WakeSwitchAction.STOP
}
