package ninja.jeremy.liveninja.wake

/**
 * What [WakeWordService.onStartCommand] should do with a delivered intent.
 *
 * Extracted as a pure function so the decision is unit-tested rather than buried in a service
 * that needs a device to exercise.
 */
enum class WakeStartDecision {
    /** Run the full start path: claim the mic, go foreground, arm the watchdog. */
    START,

    /**
     * Android is sticky-restarting us but the user has turned listening off. Stop instead of
     * resuming, or the service resurrects itself against an explicit instruction.
     */
    STOP_USER_DISABLED,

    /**
     * A control action (mute/unmute/end-session) on an already-running service. Apply it and
     * nothing else — re-running the start path here is what let a mute tap re-assert the wake
     * intent, and worse, let a failed `startForeground` turn a mute into a service stop.
     */
    HANDLE_ONLY,
}

/**
 * Decide what a delivered [action] means, given the user's persisted intent.
 *
 * [WakeWordService.ACTION_STOP] is deliberately **not** handled here: it is an unconditional
 * early return in the service (clear intent, cancel watchdog, stopSelf) and never reaches this.
 *
 * The null case is the load-bearing one. `onStartCommand` returns `START_STICKY` on the happy
 * path, so if the process is killed Android recreates the service with a **null intent** — and
 * the previous code treated that identically to an explicit start, unconditionally setting
 * `serviceEnabled = true` and going foreground again. That means a user who had just turned
 * listening off could have it silently switched back on (and the persisted OFF intent
 * overwritten) by an OEM task-kill, with no way to tell what happened. Honouring the stored
 * intent on a sticky restart is what makes "off" actually stick.
 */
fun decideWakeStart(action: String?, serviceEnabled: Boolean): WakeStartDecision = when (action) {
    // Sticky restart: resume only if the user still wants listening on.
    null -> if (serviceEnabled) WakeStartDecision.START else WakeStartDecision.STOP_USER_DISABLED

    WakeWordService.ACTION_START -> WakeStartDecision.START

    WakeWordService.ACTION_MUTE,
    WakeWordService.ACTION_UNMUTE,
    WakeWordService.ACTION_END_SESSION,
    -> WakeStartDecision.HANDLE_ONLY

    // An unrecognised action is treated as a plain start request, which is what an
    // explicit `startService` without an action has always meant here.
    else -> WakeStartDecision.START
}

/**
 * Whether bringing the app to the foreground should (re)start the wake service.
 *
 * The persisted `serviceEnabled` intent outlives the process but the service does not, so a
 * force-stop, an OEM task-kill, or a reboot leaves the flag true with nothing listening. The
 * foreground transition is the one moment the app can fix that: a foreground activity may always
 * start a microphone FGS, which is the restriction that blocks the BOOT_COMPLETED path.
 *
 * Pure so the three guards are unit-tested rather than needing a device.
 */
fun shouldResumeWakeService(
    serviceEnabled: Boolean,
    alreadyRunning: Boolean,
    micGranted: Boolean,
): Boolean = serviceEnabled && !alreadyRunning && micGranted
