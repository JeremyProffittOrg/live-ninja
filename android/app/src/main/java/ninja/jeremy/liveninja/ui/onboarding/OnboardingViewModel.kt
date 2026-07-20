package ninja.jeremy.liveninja.ui.onboarding

import android.Manifest
import android.app.Activity
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import androidx.core.content.ContextCompat
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import java.util.Optional
import javax.inject.Inject
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.assistant.AssistantRoleController
import ninja.jeremy.liveninja.ui.state.ConsentEvent
import ninja.jeremy.liveninja.ui.state.ConsentLog
import ninja.jeremy.liveninja.ui.state.OnboardingStore
import ninja.jeremy.liveninja.ui.state.SettingsStore
import ninja.jeremy.liveninja.ui.state.SignInLauncher
import ninja.jeremy.liveninja.ui.state.WakeWordCatalogRepository
import ninja.jeremy.liveninja.ui.state.WakeWordOption

/** Ordered wizard steps (mockups/android/01..04 + battery + wake-word pick). */
enum class OnboardingStep {
    WELCOME,
    SIGN_IN,
    MIC_PERMISSION,
    NOTIFICATIONS,
    ASSISTANT_ROLE,
    BATTERY,
    WAKE_WORD,
}

data class OnboardingUiState(
    val step: OnboardingStep = OnboardingStep.WELCOME,
    val signInAvailable: Boolean = false,
    val signedIn: Boolean = false,
    val micGranted: Boolean = false,
    val notificationsGranted: Boolean = false,
    /** Below API 33 POST_NOTIFICATIONS doesn't exist — treated as granted. */
    val notificationsRequestable: Boolean = true,
    val roleHeld: Boolean = false,
    val roleRequestAttempted: Boolean = false,
    val overlayGranted: Boolean = false,
    /** Doze battery-optimization exemption held (reliable background listening). */
    val batteryOptimizationIgnored: Boolean = false,
    val wakeWordOptions: List<WakeWordOption> = emptyList(),
    val selectedWakeWordId: String = "hey-live-ninja",
)

@HiltViewModel
class OnboardingViewModel @Inject constructor(
    @ApplicationContext private val context: Context,
    private val onboardingStore: OnboardingStore,
    private val consentLog: ConsentLog,
    private val settingsStore: SettingsStore,
    private val catalog: WakeWordCatalogRepository,
    private val signInLauncher: Optional<SignInLauncher>,
    private val assistantRole: AssistantRoleController,
) : ViewModel() {

    private var rolePollJob: Job? = null

    private val _state = MutableStateFlow(
        OnboardingUiState(
            signInAvailable = signInLauncher.isPresent,
            selectedWakeWordId = settingsStore.document.value.wakeWord,
            wakeWordOptions = catalog.options.value,
        ),
    )
    val state: StateFlow<OnboardingUiState> = _state

    init {
        refreshStatuses()
        viewModelScope.launch {
            catalog.refresh()
            _state.update { it.copy(wakeWordOptions = catalog.options.value) }
        }
        signInLauncher.orElse(null)?.let { launcher ->
            viewModelScope.launch {
                launcher.isSignedIn.collect { signed ->
                    _state.update { it.copy(signedIn = signed) }
                }
            }
        }
    }

    /** Re-check everything that can change while the user is off in system settings. */
    fun refreshStatuses() {
        val micGranted = ContextCompat.checkSelfPermission(
            context, Manifest.permission.RECORD_AUDIO,
        ) == PackageManager.PERMISSION_GRANTED
        val notifRequestable = Build.VERSION.SDK_INT >= 33
        val notifGranted = if (notifRequestable) {
            ContextCompat.checkSelfPermission(
                context, Manifest.permission.POST_NOTIFICATIONS,
            ) == PackageManager.PERMISSION_GRANTED
        } else {
            true
        }
        _state.update {
            it.copy(
                micGranted = micGranted,
                notificationsGranted = notifGranted,
                notificationsRequestable = notifRequestable,
                roleHeld = isAssistantRoleHeld(),
                overlayGranted = Settings.canDrawOverlays(context),
                batteryOptimizationIgnored = isIgnoringBatteryOptimizations(),
            )
        }
    }

    /**
     * Intent that opens the per-app "ignore battery optimizations" system
     * prompt (REQUEST_IGNORE_BATTERY_OPTIMIZATIONS permission, declared in the
     * manifest). Launched from the composable; result observed on ON_RESUME via
     * [refreshStatuses] (01-platform §C).
     */
    fun batteryExemptionIntent(): Intent =
        Intent(
            Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
            Uri.parse("package:${context.packageName}"),
        )

    private fun isIgnoringBatteryOptimizations(): Boolean {
        val pm = context.getSystemService(Context.POWER_SERVICE) as PowerManager
        return pm.isIgnoringBatteryOptimizations(context.packageName)
    }

    fun goTo(step: OnboardingStep) = _state.update { it.copy(step = step) }

    fun next() {
        val steps = OnboardingStep.entries
        val index = steps.indexOf(_state.value.step)
        if (index < steps.lastIndex) {
            _state.update { it.copy(step = steps[index + 1]) }
        }
    }

    fun back() {
        val steps = OnboardingStep.entries
        val index = steps.indexOf(_state.value.step)
        if (index > 0) {
            _state.update { it.copy(step = steps[index - 1]) }
        }
    }

    fun beginSignIn(activity: Activity) {
        signInLauncher.orElse(null)?.beginSignIn(activity)
    }

    /** Called when the mic step becomes visible — logs the prominent disclosure. */
    fun onMicDisclosureShown() {
        if (!consentLog.hasRecorded(ConsentEvent.MIC_DISCLOSURE_SHOWN)) {
            consentLog.record(ConsentEvent.MIC_DISCLOSURE_SHOWN, "onboarding")
        }
    }

    fun onMicPermissionResult(granted: Boolean) {
        consentLog.record(
            if (granted) ConsentEvent.MIC_PERMISSION_GRANTED else ConsentEvent.MIC_PERMISSION_DENIED,
            "onboarding",
        )
        _state.update { it.copy(micGranted = granted) }
    }

    fun onNotificationsResult(granted: Boolean) {
        consentLog.record(
            if (granted) ConsentEvent.NOTIFICATIONS_GRANTED else ConsentEvent.NOTIFICATIONS_DENIED,
            "onboarding",
        )
        _state.update { it.copy(notificationsGranted = granted) }
    }

    /**
     * The RoleManager request intent for ROLE_ASSISTANT when this OEM allows
     * requesting it via dialog, else null (caller falls back to the guided
     * system-settings walkthrough). Delegates to [AssistantRoleController].
     */
    fun assistantRoleRequestIntent(): android.content.Intent? =
        assistantRole.requestRoleIntent()

    /**
     * Deep-link to the OEM's "default digital assistant" settings screen
     * (resolve-checked candidate chain from [AssistantRoleController]) and
     * start polling for the user completing the switch out-of-band.
     */
    fun openAssistantSettings(activityContext: Context): Boolean {
        val opened = assistantRole.openAssistantSettings(activityContext) != null
        if (opened) startRolePolling()
        return opened
    }

    /**
     * Poll `isRoleHeld` while the user is off in system settings; stops as
     * soon as the role is held (recording consent) or after the controller's
     * timeout. Idempotent — a running poll is reused.
     */
    fun startRolePolling() {
        if (rolePollJob?.isActive == true) return
        rolePollJob = viewModelScope.launch {
            assistantRole.pollRoleHeld().collect { held ->
                val wasHeld = _state.value.roleHeld
                _state.update { it.copy(roleHeld = held) }
                if (held && !wasHeld) {
                    consentLog.record(ConsentEvent.ASSISTANT_ROLE_ACQUIRED, "onboarding")
                }
            }
        }
    }

    fun onRoleRequestReturned() {
        val held = isAssistantRoleHeld()
        _state.update { it.copy(roleHeld = held, roleRequestAttempted = true) }
        if (held) consentLog.record(ConsentEvent.ASSISTANT_ROLE_ACQUIRED, "onboarding")
    }

    fun onRoleSkipped() {
        consentLog.record(ConsentEvent.ASSISTANT_ROLE_DECLINED, "onboarding")
        next()
    }

    fun onOverlayReturned() {
        val granted = Settings.canDrawOverlays(context)
        if (granted && !_state.value.overlayGranted) {
            consentLog.record(ConsentEvent.OVERLAY_PERMISSION_GRANTED, "onboarding")
        }
        _state.update { it.copy(overlayGranted = granted) }
    }

    fun selectWakeWord(id: String) = _state.update { it.copy(selectedWakeWordId = id) }

    fun finish() {
        settingsStore.setWakeWord(_state.value.selectedWakeWordId)
        onboardingStore.markCompleted()
    }

    private fun isAssistantRoleHeld(): Boolean = assistantRole.refresh()

    /** OEM bucket for the settings-walkthrough copy on the assistant-role step. */
    val oemBucket: OemBucket = when {
        Build.MANUFACTURER.equals("samsung", ignoreCase = true) -> OemBucket.SAMSUNG
        Build.MANUFACTURER.equals("xiaomi", ignoreCase = true) ||
            Build.MANUFACTURER.equals("redmi", ignoreCase = true) -> OemBucket.XIAOMI
        Build.MANUFACTURER.equals("oneplus", ignoreCase = true) ||
            Build.MANUFACTURER.equals("oppo", ignoreCase = true) -> OemBucket.ONEPLUS_OPPO
        else -> OemBucket.AOSP_PIXEL
    }
}

enum class OemBucket { AOSP_PIXEL, SAMSUNG, XIAOMI, ONEPLUS_OPPO }
