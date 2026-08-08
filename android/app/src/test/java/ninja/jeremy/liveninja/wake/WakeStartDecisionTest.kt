package ninja.jeremy.liveninja.wake

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Guards the fix for "there is no way to turn off listening, I had to restart the tablet"
 * (owner report, 2026-07-25).
 *
 * Two defects sat behind that. Both are asserted here because neither is reachable from a JVM
 * test through the service itself, and both are the kind of thing a later refactor silently
 * reintroduces.
 */
class WakeStartDecisionTest {

    /**
     * The important one. `onStartCommand` returns START_STICKY on the happy path, so an OEM
     * task-kill has Android recreate the service with a **null intent**. That used to be
     * treated as an explicit start: it re-ran the start path and re-wrote
     * `serviceEnabled = true`, so listening the user had switched off came back on its own
     * with the stored intent overwritten — indistinguishable, from outside, from an off switch
     * that does not work.
     */
    @Test
    fun stickyRestartAfterTheUserTurnedListeningOff_stopsInsteadOfResuming() {
        assertEquals(
            WakeStartDecision.STOP_USER_DISABLED,
            decideWakeStart(action = null, serviceEnabled = false),
        )
    }

    @Test
    fun stickyRestartWhileTheUserStillWantsListening_resumes() {
        assertEquals(
            WakeStartDecision.START,
            decideWakeStart(action = null, serviceEnabled = true),
        )
    }

    /**
     * An explicit start must never be gated on the persisted flag — that gate was the M21.0
     * fresh-install deadlock, where the only thing that set `serviceEnabled = true` was the
     * service itself.
     */
    @Test
    fun explicitStartIsNeverGatedOnThePersistedFlag() {
        assertEquals(
            WakeStartDecision.START,
            decideWakeStart(WakeWordService.ACTION_START, serviceEnabled = false),
        )
    }

    /**
     * Control actions must not re-enter the start path. Previously they fell through it, so a
     * mute tap re-asserted the wake intent, and a `startForeground` failure during that
     * fall-through could turn a mute into a service stop.
     */
    @Test
    fun controlActionsDoNotReRunTheStartPath() {
        for (action in listOf(
            WakeWordService.ACTION_MUTE,
            WakeWordService.ACTION_UNMUTE,
            WakeWordService.ACTION_END_SESSION,
        )) {
            assertEquals(
                "$action must not re-enter the start path",
                WakeStartDecision.HANDLE_ONLY,
                decideWakeStart(action, serviceEnabled = true),
            )
            // Also true when the flag is off: a control action is not a start request.
            assertEquals(
                "$action must not start listening",
                WakeStartDecision.HANDLE_ONLY,
                decideWakeStart(action, serviceEnabled = false),
            )
        }
    }

    @Test
    fun anUnknownActionIsTreatedAsAPlainStart() {
        assertEquals(
            WakeStartDecision.START,
            decideWakeStart("ninja.jeremy.liveninja.wake.SOMETHING_NEW", serviceEnabled = true),
        )
    }

    /**
     * Guards the fix for "the wake word doesn't work, on screen or not" (owner report,
     * 2026-08-08, Galaxy S9).
     *
     * `serviceEnabled` outlives the process but the service does not. Both callers of
     * WakeWordService.start were unreachable on an ordinary launch — the boot receiver's
     * notification, and MainActivity.handleWakeResume which returns early unless that
     * notification's EXTRA_START_WAKE_SERVICE is present. So after any force-stop or OEM
     * task-kill the switch still read ON and nothing was listening, with no way to tell.
     */
    @Test
    fun foregroundingWithListeningOnButNothingRunning_restartsTheService() {
        assertTrue(
            shouldResumeWakeService(
                serviceEnabled = true,
                alreadyRunning = false,
                micGranted = true,
            ),
        )
    }

    /** The user turned listening off; foregrounding must not resurrect it. */
    @Test
    fun foregroundingWithListeningOff_staysOff() {
        assertFalse(
            shouldResumeWakeService(
                serviceEnabled = false,
                alreadyRunning = false,
                micGranted = true,
            ),
        )
    }

    /** onStart fires on every return to the app; re-asserting a live service is pointless churn. */
    @Test
    fun foregroundingWhileAlreadyListening_doesNotRestart() {
        assertFalse(
            shouldResumeWakeService(
                serviceEnabled = true,
                alreadyRunning = true,
                micGranted = true,
            ),
        )
    }

    /**
     * Revoking the mic permission leaves serviceEnabled true. Starting anyway would fail the
     * engine start and post an error notification on every app launch.
     */
    @Test
    fun foregroundingWithoutMicPermission_doesNotStart() {
        assertFalse(
            shouldResumeWakeService(
                serviceEnabled = true,
                alreadyRunning = false,
                micGranted = false,
            ),
        )
    }
}
