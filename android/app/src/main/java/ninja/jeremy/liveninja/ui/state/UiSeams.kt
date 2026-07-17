package ninja.jeremy.liveninja.ui.state

import android.app.Activity
import dagger.BindsOptionalOf
import dagger.Module
import dagger.hilt.InstallIn
import dagger.hilt.components.SingletonComponent
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.StateFlow

/**
 * Integration seams between the UI layer (this package) and the M4 workstreams
 * that run in parallel with it (auth exchange, realtime WebRTC session, wake
 * service). Each seam is declared `@BindsOptionalOf`, so:
 *
 *  - the UI compiles and runs before the sibling workstream lands its
 *    implementation (the Optional is absent and the UI shows its explicit
 *    "not connected in this build" state — a real, designed state, not a crash);
 *  - the moment the sibling module contributes an `@Binds` for the type, the
 *    Optional becomes present with zero UI changes.
 */

/** Auth-workstream seam: LWA Custom Tabs + PKCE sign-in (plan.md M4, Android §6). */
interface SignInLauncher {
    /** True while a valid session (refresh token in Keystore) exists. */
    val isSignedIn: StateFlow<Boolean>

    /**
     * Launch the Custom-Tabs LWA authorize flow from [activity]. Completion
     * arrives via the App-Link/custom-scheme redirect into MainActivity and is
     * reflected on [isSignedIn].
     */
    fun beginSignIn(activity: Activity)
}

/** Auth-workstream seam: session teardown actions surfaced in Settings. */
interface AccountActions {
    /** `POST /auth/logout` + clear local tokens. */
    suspend fun signOut()

    /** `POST /v1/account/logout-everywhere` + clear local tokens. */
    suspend fun signOutEverywhere()
}

/** Events the realtime session layer emits for the conversation UI. */
sealed interface SessionUiEvent {
    /** Incremental transcript text for one conversation item. */
    data class TranscriptDelta(
        val itemId: String,
        val role: TranscriptRole,
        val textDelta: String,
        val done: Boolean,
    ) : SessionUiEvent

    /** Assistant audio playback started/stopped (drives the speaking indicator). */
    data class AssistantSpeaking(val speaking: Boolean) : SessionUiEvent

    /** Server VAD detected the user speaking — barge-in trigger + visual. */
    object UserSpeechStarted : SessionUiEvent

    /** A server-side tool call ran (rendered as a tool chip in the transcript). */
    data class ToolCall(val itemId: String, val name: String, val summary: String) : SessionUiEvent

    /** Fatal session error; the UI transitions to its error state. */
    data class SessionError(val message: String) : SessionUiEvent
}

enum class TranscriptRole { USER, ASSISTANT }

/**
 * Realtime-workstream seam: one live GPT-Realtime session
 * (token from `GET /v1/realtime/session`, WebRTC via RealtimeTransport).
 */
interface RealtimeSessionController {
    /** True between a successful [start] and [stop]/failure. */
    val connected: StateFlow<Boolean>

    /** Hot stream of UI events for the active session. */
    val events: Flow<SessionUiEvent>

    /** Bootstrap + connect. Throws on auth/network/negotiation failure. */
    suspend fun start()

    /** End the session and release audio. */
    suspend fun stop()

    /** Mute/unmute the outgoing mic track. */
    fun setMicMuted(muted: Boolean)

    /** Barge-in: cancel assistant response + stop local playback immediately. */
    fun interruptAssistant()
}

@Module
@InstallIn(SingletonComponent::class)
abstract class UiSeamsModule {
    @BindsOptionalOf
    abstract fun signInLauncher(): SignInLauncher

    @BindsOptionalOf
    abstract fun accountActions(): AccountActions

    @BindsOptionalOf
    abstract fun realtimeSessionController(): RealtimeSessionController
}
