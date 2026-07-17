package ninja.jeremy.liveninja.assistant

import android.os.SystemClock
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.SharedFlow

/** What raised the assistant session. */
enum class AssistSource {
    /** System voice-interaction pipeline (long-press home / gesture / power-button assist). */
    VOICE_INTERACTION,

    /** Our own wake-word FGS detected the wake phrase. */
    WAKE_WORD,

    /** User tapped a mic affordance inside the app. */
    MANUAL,
}

/**
 * A single "open the conversation and start a realtime session" request.
 *
 * @param source what raised the session
 * @param launchedWhileLocked true when the device keyguard was locked at launch;
 *   consumers must route sensitive tool calls through [KeyguardGate].
 * @param timestampMillis [SystemClock.elapsedRealtime] at emission — collectors
 *   use it to de-duplicate replayed triggers.
 */
data class AssistTrigger(
    val source: AssistSource,
    val launchedWhileLocked: Boolean,
    val timestampMillis: Long = SystemClock.elapsedRealtime(),
)

/**
 * Process-wide bridge between the assistant entry points (VoiceInteractionSession,
 * wake-word service, in-app mic button) and the conversation UI / realtime layer.
 *
 * Emitters: [ninja.jeremy.liveninja.MainActivity] (on the assist intent from
 * [LiveNinjaSession]), the wake-word service, and manual UI affordances.
 * Collectors: `LiveNinjaRoot` (navigates to the Conversation tab) and the
 * realtime session ViewModel (starts the WebRTC session). `replay = 1` so a
 * trigger emitted before the Compose tree is up is still delivered; collectors
 * de-duplicate via [AssistTrigger.timestampMillis].
 */
@Singleton
class AssistantEvents @Inject constructor() {
    private val _triggers = MutableSharedFlow<AssistTrigger>(replay = 1, extraBufferCapacity = 4)

    /** Hot stream of assistant triggers; replays the most recent one. */
    val triggers: SharedFlow<AssistTrigger> = _triggers

    /** Emit a trigger. Never suspends or blocks (buffered). */
    fun emit(trigger: AssistTrigger) {
        _triggers.tryEmit(trigger)
    }
}
