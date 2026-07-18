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
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.WakeWordCreateRequest
import ninja.jeremy.liveninja.ui.state.AccountActions
import ninja.jeremy.liveninja.ui.state.SettingsDocument
import ninja.jeremy.liveninja.ui.state.SettingsStore
import ninja.jeremy.liveninja.ui.state.SignInLauncher
import ninja.jeremy.liveninja.ui.state.WakeWordCatalogRepository
import ninja.jeremy.liveninja.ui.state.WakeWordOption
import ninja.jeremy.liveninja.wake.ModelManager
import ninja.jeremy.liveninja.wake.ModelSyncResult
import ninja.jeremy.liveninja.wake.WakePreferences
import retrofit2.HttpException

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
    WAKE_MODEL_READY,
    WAKE_MODEL_SIGNED_OUT,
    WAKE_MODEL_FAILED,
    WAKE_TRAIN_REQUESTED,
    WAKE_TRAIN_READY,
    WAKE_TRAIN_FAILED,
    WAKE_TRAIN_LIMIT,
    WAKE_TRAIN_INVALID,
    WAKE_TRAIN_REQUEST_FAILED,
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
    // ---- custom wake-word training (M6 FR-K03) ----
    val customPhrase: String = "",
    val customJob: CustomWakeJob? = null,
    val customRequestInProgress: Boolean = false,
) {
    val customPhraseValid: Boolean
        get() = SettingsViewModel.isValidWakePhrase(customPhrase)
}

@HiltViewModel
class SettingsViewModel @Inject constructor(
    @ApplicationContext private val context: Context,
    private val settingsStore: SettingsStore,
    private val catalog: WakeWordCatalogRepository,
    private val api: LiveNinjaApi,
    private val modelManager: ModelManager,
    private val wakePrefs: WakePreferences,
    private val customStore: CustomWakeWordStore,
    private val accountActions: Optional<AccountActions>,
    signInLauncher: Optional<SignInLauncher>,
) : ViewModel() {

    private val _state = MutableStateFlow(
        SettingsUiState(
            doc = settingsStore.document.value,
            wakeOptions = mergedOptions(catalog.options.value, customStore.load()),
            micDevices = enumerateMicDevices(),
            porcupineAvailable = ninja.jeremy.liveninja.BuildConfig.PORCUPINE_ENABLED,
            accountActionsAvailable = accountActions.isPresent,
            customJob = customStore.load(),
        ),
    )
    val state: StateFlow<SettingsUiState> = _state

    private val _notices = MutableSharedFlow<SettingsNotice>(extraBufferCapacity = 4)
    val notices: SharedFlow<SettingsNotice> = _notices

    private var modelSyncJob: Job? = null
    private var pollJob: Job? = null

    init {
        viewModelScope.launch {
            settingsStore.document.collect { doc -> _state.update { it.copy(doc = doc) } }
        }
        viewModelScope.launch {
            catalog.refresh()
            _state.update {
                it.copy(
                    wakeOptions = mergedOptions(catalog.options.value, it.customJob),
                    wakeCatalogOffline = catalog.lastFetchFailed.value,
                )
            }
        }
        signInLauncher.orElse(null)?.let { launcher ->
            viewModelScope.launch {
                launcher.isSignedIn.collect { signed -> _state.update { it.copy(signedIn = signed) } }
            }
        }
        // Resume polling a training job that outlived the previous process
        // (Batch jobs run up to 20 min; the SES "ready" email is the backstop).
        startPollingCustomJob()
    }

    // ---- Wake word ----

    /**
     * Select a wake word: canonical settings doc + write-through to the wake
     * stack's own prefs (the running FGS reads those), then fetch + SHA-verify
     * the model manifest so [ModelManager.headModel] hot-swaps the live engine
     * (wakeword-manifest.md client sequence). On any failure the previous
     * model keeps listening — never a gap.
     */
    fun setWakeWord(id: String) {
        settingsStore.setWakeWord(id)
        wakePrefs.wakeWordId = id
        syncWakeModel(id, wakePrefs.wakeEngine)
    }

    fun setWakeEngine(engine: String) {
        settingsStore.setWakeEngine(engine)
        wakePrefs.wakeEngine = engine
        syncWakeModel(wakePrefs.wakeWordId, engine)
    }

    fun setSensitivity(value: Float) {
        val clamped = value.coerceIn(0f, 1f)
        settingsStore.setSensitivity(clamped)
        // Write-through: the engine consumes sensitivityFlow live.
        wakePrefs.sensitivity = clamped
    }

    private fun syncWakeModel(id: String, engine: String) {
        modelSyncJob?.cancel()
        modelSyncJob = viewModelScope.launch {
            when (modelManager.sync(id, engine)) {
                is ModelSyncResult.Active -> _notices.tryEmit(SettingsNotice.WAKE_MODEL_READY)
                is ModelSyncResult.NoAuth -> _notices.tryEmit(SettingsNotice.WAKE_MODEL_SIGNED_OUT)
                // VerifyFailed / UnsupportedFormat / Failed: previous model
                // stays active per contract; surface one honest notice.
                else -> _notices.tryEmit(SettingsNotice.WAKE_MODEL_FAILED)
            }
        }
    }

    // ---- Custom wake-word training (M6 FR-K03) ----

    fun setCustomPhrase(text: String) =
        _state.update { it.copy(customPhrase = text.take(MAX_PHRASE_LENGTH)) }

    /** POST the phrase to the training pipeline and start status polling. */
    fun requestCustomWakeWord() {
        val phrase = _state.value.customPhrase.trim()
        if (!isValidWakePhrase(phrase) || _state.value.customRequestInProgress) return
        _state.update { it.copy(customRequestInProgress = true) }
        viewModelScope.launch {
            try {
                val dto = api.createWakeWord(
                    // openWakeWord is the only server-side training path (M6
                    // locked decision — Porcupine needs a Picovoice account).
                    WakeWordCreateRequest(
                        phrase = phrase,
                        engine = WakePreferences.ENGINE_OPENWAKEWORD,
                    ),
                )
                val job = CustomWakeJob(
                    id = dto.id,
                    phrase = dto.phrase ?: phrase,
                    engine = dto.engine ?: WakePreferences.ENGINE_OPENWAKEWORD,
                    status = dto.status ?: "pending",
                    error = dto.error,
                )
                customStore.save(job)
                _state.update {
                    it.copy(
                        customJob = job,
                        customPhrase = "",
                        wakeOptions = mergedOptions(catalog.options.value, job),
                    )
                }
                _notices.tryEmit(SettingsNotice.WAKE_TRAIN_REQUESTED)
                startPollingCustomJob()
            } catch (e: HttpException) {
                _notices.tryEmit(
                    when (e.code()) {
                        429 -> SettingsNotice.WAKE_TRAIN_LIMIT // ≤3/day/user, conc ≤2
                        400, 409, 422 -> SettingsNotice.WAKE_TRAIN_INVALID
                        else -> SettingsNotice.WAKE_TRAIN_REQUEST_FAILED
                    },
                )
            } catch (e: Exception) {
                _notices.tryEmit(SettingsNotice.WAKE_TRAIN_REQUEST_FAILED)
            } finally {
                _state.update { it.copy(customRequestInProgress = false) }
            }
        }
    }

    /** Ready job → select it (settings + model download + engine hot-swap). */
    fun useCustomWakeWord() {
        val job = _state.value.customJob ?: return
        if (!job.ready) return
        setWakeWord(job.id)
    }

    /**
     * Dismiss the status card. A ready job's catalog entry stays in the
     * combobox for this session; long-term the shared catalog snapshot
     * carries the user's ready models.
     */
    fun clearCustomJob() {
        pollJob?.cancel()
        customStore.clear()
        _state.update { it.copy(customJob = null) }
    }

    private fun startPollingCustomJob() {
        pollJob?.cancel()
        val job = customStore.load() ?: return
        if (!job.inFlight) return
        pollJob = viewModelScope.launch {
            while (true) {
                delay(POLL_INTERVAL_MS)
                val current = customStore.load() ?: return@launch
                val dto = try {
                    api.getWakeWord(current.id)
                } catch (e: Exception) {
                    continue // transient — keep polling while the VM lives
                }
                val updated = current.copy(
                    status = dto.status ?: current.status,
                    error = dto.error,
                )
                customStore.save(updated)
                _state.update {
                    it.copy(
                        customJob = updated,
                        wakeOptions = mergedOptions(catalog.options.value, updated),
                    )
                }
                when {
                    updated.ready -> {
                        _notices.tryEmit(SettingsNotice.WAKE_TRAIN_READY)
                        return@launch
                    }
                    updated.status == "failed" -> {
                        _notices.tryEmit(SettingsNotice.WAKE_TRAIN_FAILED)
                        return@launch
                    }
                }
            }
        }
    }

    /** Catalog entries + this user's ready custom model (server wins on id collision). */
    private fun mergedOptions(
        catalogOptions: List<WakeWordOption>,
        job: CustomWakeJob?,
    ): List<WakeWordOption> {
        if (job == null || !job.ready || catalogOptions.any { it.id == job.id }) {
            return catalogOptions
        }
        return catalogOptions + WakeWordOption(
            id = job.id,
            label = "“${job.phrase}”",
            description = "Custom trained phrase",
            engines = listOf(job.engine),
        )
    }

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

        /** Client-side pre-check mirror of the backend phrase validation. */
        const val MAX_PHRASE_LENGTH = 40
        const val MIN_PHRASE_LENGTH = 3
        private const val MAX_PHRASE_WORDS = 6

        /** Poll cadence for an in-flight training job (jobs run minutes, not seconds). */
        const val POLL_INTERVAL_MS = 15_000L

        /**
         * Cheap client-side gate for the training form (backend re-validates
         * phonemes/profanity/collision authoritatively): letters, spaces,
         * apostrophes, hyphens; 3–40 chars; ≤6 words.
         */
        fun isValidWakePhrase(raw: String): Boolean {
            val phrase = raw.trim()
            if (phrase.length !in MIN_PHRASE_LENGTH..MAX_PHRASE_LENGTH) return false
            if (!phrase.all { it.isLetter() || it == ' ' || it == '\'' || it == '-' }) return false
            return phrase.split(Regex("\\s+")).size <= MAX_PHRASE_WORDS
        }

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
