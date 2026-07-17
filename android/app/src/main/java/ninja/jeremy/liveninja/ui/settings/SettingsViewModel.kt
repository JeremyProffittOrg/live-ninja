package ninja.jeremy.liveninja.ui.settings

import android.content.Context
import android.media.AudioDeviceInfo
import android.media.AudioManager
import android.os.Build
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import java.util.Optional
import javax.inject.Inject
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.ui.state.AccountActions
import ninja.jeremy.liveninja.ui.state.SettingsDocument
import ninja.jeremy.liveninja.ui.state.SettingsStore
import ninja.jeremy.liveninja.ui.state.SignInLauncher
import ninja.jeremy.liveninja.ui.state.WakeWordCatalogRepository
import ninja.jeremy.liveninja.ui.state.WakeWordOption

/** One persona catalog entry (server resolves the actual instructions by ID). */
data class PersonaPreset(val id: String, val label: String, val description: String)

/** One selectable input device (populated picker — never a typed ID). */
data class MicDeviceOption(val id: String?, val label: String)

/** Snackbar-level notices the screen shows (mapped to strings there). */
enum class SettingsNotice {
    VOICE_PREVIEW_UNAVAILABLE,
    SIGNED_OUT,
    SIGNED_OUT_EVERYWHERE,
    SIGN_OUT_FAILED,
}

data class SettingsUiState(
    val doc: SettingsDocument,
    val wakeOptions: List<WakeWordOption> = emptyList(),
    val wakeCatalogOffline: Boolean = false,
    val micDevices: List<MicDeviceOption> = emptyList(),
    val personaPresets: List<PersonaPreset> = SettingsViewModel.PERSONA_PRESETS,
    val porcupineAvailable: Boolean = false,
    val accountActionsAvailable: Boolean = false,
    val signedIn: Boolean = false,
    val signOutInProgress: Boolean = false,
)

@HiltViewModel
class SettingsViewModel @Inject constructor(
    @ApplicationContext private val context: Context,
    private val settingsStore: SettingsStore,
    private val catalog: WakeWordCatalogRepository,
    private val accountActions: Optional<AccountActions>,
    signInLauncher: Optional<SignInLauncher>,
) : ViewModel() {

    private val _state = MutableStateFlow(
        SettingsUiState(
            doc = settingsStore.document.value,
            wakeOptions = catalog.options.value,
            micDevices = enumerateMicDevices(),
            porcupineAvailable = ninja.jeremy.liveninja.BuildConfig.PORCUPINE_ENABLED,
            accountActionsAvailable = accountActions.isPresent,
        ),
    )
    val state: StateFlow<SettingsUiState> = _state

    private val _notices = MutableSharedFlow<SettingsNotice>(extraBufferCapacity = 4)
    val notices: SharedFlow<SettingsNotice> = _notices

    init {
        viewModelScope.launch {
            settingsStore.document.collect { doc -> _state.update { it.copy(doc = doc) } }
        }
        viewModelScope.launch {
            catalog.refresh()
            _state.update {
                it.copy(
                    wakeOptions = catalog.options.value,
                    wakeCatalogOffline = catalog.lastFetchFailed.value,
                )
            }
        }
        signInLauncher.orElse(null)?.let { launcher ->
            viewModelScope.launch {
                launcher.isSignedIn.collect { signed -> _state.update { it.copy(signedIn = signed) } }
            }
        }
    }

    // ---- Wake word ----
    fun setWakeWord(id: String) = settingsStore.setWakeWord(id)
    fun setWakeEngine(engine: String) = settingsStore.setWakeEngine(engine)
    fun setSensitivity(value: Float) = settingsStore.setSensitivity(value.coerceIn(0f, 1f))

    // ---- Conversation ----
    fun setPersona(presetId: String) {
        val doc = _state.value.doc
        settingsStore.setPersona(
            presetId,
            if (presetId == "custom") doc.personaSystemInstructions.orEmpty() else null,
        )
    }

    fun setCustomInstructions(text: String) {
        settingsStore.setPersona("custom", text.take(CUSTOM_INSTRUCTIONS_MAX))
    }

    fun setVoice(voice: String) = settingsStore.setVoice(voice)

    fun onVoicePreviewRequested() {
        // No bundled samples ship with the app and the backend TTS preview
        // endpoint doesn't exist yet — surface the designed "unavailable" notice.
        _notices.tryEmit(SettingsNotice.VOICE_PREVIEW_UNAVAILABLE)
    }

    fun setTurnDetection(value: String) = settingsStore.setTurnDetection(value)

    // ---- Audio ----
    fun setMicDevice(id: String?) = settingsStore.setMicDeviceId(id)

    fun refreshMicDevices() = _state.update { it.copy(micDevices = enumerateMicDevices()) }

    // ---- Appearance ----
    fun setTheme(theme: String) = settingsStore.setTheme(theme)

    // ---- Privacy ----
    fun setStoreAudio(enabled: Boolean) = with(_state.value.doc) {
        settingsStore.setPrivacy(enabled, storeTranscripts, retentionDays)
    }

    fun setStoreTranscripts(enabled: Boolean) = with(_state.value.doc) {
        settingsStore.setPrivacy(storeAudio, enabled, retentionDays)
    }

    fun setRetentionDays(days: Int) = with(_state.value.doc) {
        settingsStore.setPrivacy(storeAudio, storeTranscripts, days)
    }

    // ---- Account ----
    fun signOut() = performSignOut(everywhere = false)
    fun signOutEverywhere() = performSignOut(everywhere = true)

    private fun performSignOut(everywhere: Boolean) {
        val actions = accountActions.orElse(null) ?: return
        _state.update { it.copy(signOutInProgress = true) }
        viewModelScope.launch {
            try {
                if (everywhere) actions.signOutEverywhere() else actions.signOut()
                settingsStore.resetToDefaults()
                _notices.tryEmit(
                    if (everywhere) SettingsNotice.SIGNED_OUT_EVERYWHERE else SettingsNotice.SIGNED_OUT,
                )
            } catch (_: Exception) {
                _notices.tryEmit(SettingsNotice.SIGN_OUT_FAILED)
            } finally {
                _state.update { it.copy(signOutInProgress = false) }
            }
        }
    }

    private fun enumerateMicDevices(): List<MicDeviceOption> {
        val audioManager = context.getSystemService(Context.AUDIO_SERVICE) as AudioManager
        val devices: List<AudioDeviceInfo> = if (Build.VERSION.SDK_INT >= 31) {
            audioManager.availableCommunicationDevices.filter { it.isSource }
        } else {
            audioManager.getDevices(AudioManager.GET_DEVICES_INPUTS).toList()
        }
        val options = devices
            .filter {
                it.type in setOf(
                    AudioDeviceInfo.TYPE_BUILTIN_MIC,
                    AudioDeviceInfo.TYPE_BLUETOOTH_SCO,
                    AudioDeviceInfo.TYPE_WIRED_HEADSET,
                    AudioDeviceInfo.TYPE_USB_HEADSET,
                    AudioDeviceInfo.TYPE_USB_DEVICE,
                )
            }
            .map { device ->
                val typeLabel = when (device.type) {
                    AudioDeviceInfo.TYPE_BUILTIN_MIC -> "Built-in microphone"
                    AudioDeviceInfo.TYPE_BLUETOOTH_SCO -> "Bluetooth headset"
                    AudioDeviceInfo.TYPE_WIRED_HEADSET -> "Wired headset"
                    AudioDeviceInfo.TYPE_USB_HEADSET, AudioDeviceInfo.TYPE_USB_DEVICE -> "USB microphone"
                    else -> "Microphone"
                }
                val product = device.productName?.toString()?.takeIf { it.isNotBlank() }
                MicDeviceOption(
                    id = device.id.toString(),
                    label = if (product != null && product != Build.MODEL) {
                        "$typeLabel — $product"
                    } else {
                        typeLabel
                    },
                )
            }
            .distinctBy { it.id }
        return listOf(MicDeviceOption(id = null, label = "System default")) + options
    }

    companion object {
        const val CUSTOM_INSTRUCTIONS_MAX = 4000

        /**
         * Persona catalog (mockups/android/09-settings.html). IDs only travel to
         * the backend — the server resolves instructions server-side
         * (anti-prompt-injection, settings.schema.json `persona`).
         */
        val PERSONA_PRESETS = listOf(
            PersonaPreset("default", "Assistant", "Balanced, helpful default"),
            PersonaPreset("focused", "Focused Ninja", "Concise, task-first"),
            PersonaPreset("friendly", "Friendly Ninja", "Warm, conversational"),
            PersonaPreset("coach", "Coach Ninja", "Motivating, direct"),
            PersonaPreset("analyst", "Analyst Ninja", "Precise, data-driven"),
            PersonaPreset("custom", "Custom", "Write your own system instructions"),
        )
    }
}
