package ninja.jeremy.liveninja

import android.content.Intent
import android.os.Bundle
import android.view.WindowManager
import androidx.activity.ComponentActivity
import androidx.activity.SystemBarStyle
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.viewModels
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import dagger.hilt.android.AndroidEntryPoint
import javax.inject.Inject
import ninja.jeremy.liveninja.assistant.AssistSource
import ninja.jeremy.liveninja.assistant.AssistTrigger
import ninja.jeremy.liveninja.assistant.AssistantEvents
import ninja.jeremy.liveninja.assistant.KeyguardGate
import ninja.jeremy.liveninja.assistant.LiveNinjaSession
import ninja.jeremy.liveninja.auth.AuthRepository
import ninja.jeremy.liveninja.realtime.SessionOrchestrator
import ninja.jeremy.liveninja.ui.LiveNinjaRoot
import ninja.jeremy.liveninja.ui.conversation.ConversationViewModel
import ninja.jeremy.liveninja.ui.state.SettingsStore
import ninja.jeremy.liveninja.ui.theme.LiveNinjaTheme
import ninja.jeremy.liveninja.wake.WakeBootReceiver
import ninja.jeremy.liveninja.wake.WakePreferences
import ninja.jeremy.liveninja.wake.WakeWordService

@AndroidEntryPoint
class MainActivity : ComponentActivity() {

    /** Bridge from assistant entry points to the Compose tree / realtime layer. */
    @Inject lateinit var assistantEvents: AssistantEvents

    /** Keyguard + biometric gating for lock-screen sessions (Android §2). */
    @Inject lateinit var keyguardGate: KeyguardGate

    /** LWA Custom-Tabs return URIs (App Link / custom scheme) land here. */
    @Inject lateinit var authRepository: AuthRepository

    /** Wake-word service enabled/muted state (resume-after-boot handoff). */
    @Inject lateinit var wakePreferences: WakePreferences

    /** Canonical settings doc — drives the light/dark/system theme choice. */
    @Inject lateinit var settingsStore: SettingsStore

    /**
     * Injecting the singleton orchestrator here constructs it (Hilt singletons
     * are lazy) so the manual/assist entry path works even when the wake service
     * is disabled — it binds the AssistantEvents/WakeEvents collectors on init.
     */
    @Inject lateinit var sessionOrchestrator: SessionOrchestrator

    /**
     * Activity-scoped conversation session state (same instance the
     * Conversation tab uses) — backgrounding the app while a session is live
     * shows the floating overlay bubble; foregrounding hides it.
     */
    private val conversationViewModel: ConversationViewModel by viewModels()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // HAL 9000 pins a near-black system-bar scrim (03-theme §C); other styles
        // keep the default adaptive bars.
        if (settingsStore.document.value.appStyle == "hal9000") {
            enableEdgeToEdge(
                SystemBarStyle.dark(0xFF050507.toInt()),
                SystemBarStyle.dark(0xFF050507.toInt()),
            )
        } else {
            enableEdgeToEdge()
        }
        handleAssistIntent(intent)
        handleAuthRedirect(intent)
        handleWakeResume(intent)
        applyLockScreenHygiene(intent)
        setContent {
            val settings by settingsStore.document.collectAsStateWithLifecycle()
            val darkTheme = when (settings.theme) {
                "light" -> false
                "dark" -> true
                else -> isSystemInDarkTheme()
            }
            // Keep-screen-awake toggle (foreground only, no permission): add/clear
            // FLAG_KEEP_SCREEN_ON as the setting flips.
            DisposableEffect(settings.keepScreenOn) {
                if (settings.keepScreenOn) {
                    window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
                } else {
                    window.clearFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
                }
                onDispose { window.clearFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON) }
            }
            LiveNinjaTheme(darkTheme = darkTheme) {
                LiveNinjaRoot(assistTriggers = assistantEvents.triggers)
            }
        }
    }

    /**
     * setShowWhenLocked/setTurnScreenOn are sticky Activity flags; clear them
     * when the user has turned wake-screen-on-wake OFF, unless this launch is a
     * genuine over-keyguard assist (handled in [handleAssistIntent]).
     */
    private fun applyLockScreenHygiene(intent: Intent?) {
        if (intent?.action == LiveNinjaSession.ACTION_ASSIST) return
        if (!settingsStore.document.value.wakeScreenOnWake) {
            setShowWhenLocked(false)
            setTurnScreenOn(false)
        }
    }

    override fun onStart() {
        super.onStart()
        conversationViewModel.onAppForegrounded()
    }

    override fun onStop() {
        super.onStop()
        conversationViewModel.onAppBackgrounded()
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        handleAssistIntent(intent)
        handleAuthRedirect(intent)
        handleWakeResume(intent)
    }

    /**
     * Android 15 forbids a microphone FGS start straight from BOOT_COMPLETED, so
     * [WakeBootReceiver] posts a tap-to-resume notification that lands here; a foreground
     * activity start is always permitted to launch the wake FGS.
     */
    private fun handleWakeResume(intent: Intent?) {
        if (intent?.getBooleanExtra(WakeBootReceiver.EXTRA_START_WAKE_SERVICE, false) != true) return
        if (wakePreferences.serviceEnabled) WakeWordService.start(this)
    }

    /** Route an LWA return URI into the auth flow (state check + code exchange). */
    private fun handleAuthRedirect(intent: Intent?) {
        if (intent?.action != Intent.ACTION_VIEW) return
        val uri = intent.data ?: return
        authRepository.handleRedirect(uri)
    }

    /**
     * Turn an assist launch from [LiveNinjaSession] (or the wake-word service)
     * into an [AssistTrigger]. When launched over the keyguard, show this
     * activity above it so the conversation can start immediately; sensitive
     * tool calls remain gated behind [keyguardGate] downstream.
     */
    private fun handleAssistIntent(intent: Intent?) {
        if (intent?.action != LiveNinjaSession.ACTION_ASSIST) return
        val locked = keyguardGate.isKeyguardLocked() ||
            intent.getBooleanExtra(LiveNinjaSession.EXTRA_LAUNCHED_WHILE_LOCKED, false)
        if (locked) {
            setShowWhenLocked(true)
            setTurnScreenOn(true)
        }
        val source = intent.getStringExtra(LiveNinjaSession.EXTRA_SOURCE)
            ?.let { name -> AssistSource.entries.firstOrNull { it.name == name } }
            ?: AssistSource.VOICE_INTERACTION
        assistantEvents.emit(
            AssistTrigger(source = source, launchedWhileLocked = locked),
        )
    }
}
