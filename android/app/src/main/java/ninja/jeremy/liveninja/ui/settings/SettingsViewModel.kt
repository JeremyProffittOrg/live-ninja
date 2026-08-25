package ninja.jeremy.liveninja.ui.settings

import android.content.Context
import android.content.Intent
import android.media.AudioDeviceInfo
import android.media.AudioManager
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import java.util.Optional
import javax.inject.Inject
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.jsonPrimitive
import ninja.jeremy.liveninja.log.LogExporter
import ninja.jeremy.liveninja.log.LogSink
import ninja.jeremy.liveninja.net.PersonaInfoDto
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.WakeWordCreateRequest
import ninja.jeremy.liveninja.ui.state.AccountActions
import ninja.jeremy.liveninja.ui.state.DiagnosticsConfig
import ninja.jeremy.liveninja.ui.state.SettingsDocument
import ninja.jeremy.liveninja.ui.state.SettingsStore
import ninja.jeremy.liveninja.ui.state.SignInLauncher
import ninja.jeremy.liveninja.ui.state.WakeWordCatalogRepository
import ninja.jeremy.liveninja.ui.state.WakeWordOption
import ninja.jeremy.liveninja.wake.ModelManager
import ninja.jeremy.liveninja.wake.ModelSyncResult
import ninja.jeremy.liveninja.wake.WakePreferences
import org.json.JSONObject
import retrofit2.HttpException

/**
 * One persona catalog entry (server resolves the actual instructions by ID).
 *
 * [group] is the picker section from the server ("General" | "PDLC" | "ESP32"
 * | "Fun"); empty means the server predates grouping, or this is a locally
 * synthesized entry like "custom".
 */
data class PersonaPreset(
    val id: String,
    val label: String,
    val description: String,
    val group: String = "",
)

/**
 * One selectable Gemini Live voice (M13, D4) — populated from the
 * `geminiVoices` catalog on `GET /api/v1/realtime/voices`, never typed.
 */
data class GeminiVoiceOption(
    val id: String,
    val label: String,
    val description: String,
    val default: Boolean,
)

/** One selectable input device (populated picker — never a typed ID). */
data class MicDeviceOption(val id: String?, val label: String)

/** Snackbar-level notices the screen shows (mapped to strings there). */
enum class SettingsNotice {
    VOICE_PREVIEW_UNAVAILABLE,
    SIGNED_OUT,
    SIGNED_OUT_EVERYWHERE,
    SIGN_OUT_FAILED,
    WAKE_MODEL_READY,
    WAKE_MODEL_BUILTIN,
    WAKE_MODEL_UNTRAINED,
    WAKE_MODEL_SIGNED_OUT,
    WAKE_MODEL_FAILED,
    WAKE_TRAIN_REQUESTED,
    WAKE_TRAIN_READY,
    WAKE_TRAIN_FAILED,
    WAKE_TRAIN_LIMIT,
    WAKE_TRAIN_INVALID,
    WAKE_TRAIN_REQUEST_FAILED,
    SETTINGS_SYNC_FAILED,
    SETTINGS_APPLIED,
    SETTINGS_INHERITED,
    MICROPHONE_TARGET_LOCAL_ONLY,
    DEVICE_RENAMED,
    DEVICE_RENAME_FAILED,
}

data class SettingsHostUi(
    val id: String,
    val name: String,
    val surface: String? = null,
    val metadata: Map<String, String> = emptyMap(),
    val capabilities: Set<String> = emptySet(),
    val isCurrent: Boolean = false,
    val inherited: Boolean = true,
    val settings: JSONObject = JSONObject(),
) {
    fun supports(section: SettingsSection): Boolean =
        capabilities.isEmpty() || section.apiId in capabilities
}

data class SettingsSectionScopeUi(
    val version: Int = 0,
    val hosts: List<SettingsHostUi> = emptyList(),
    val viewedDeviceId: String? = null,
    val loading: Boolean = false,
) {
    val viewedHost: SettingsHostUi?
        get() = hosts.firstOrNull { it.id == viewedDeviceId }
            ?: hosts.firstOrNull { it.isCurrent }
            ?: hosts.firstOrNull()
}

data class SettingsUiState(
    val doc: SettingsDocument,
    val wakeOptions: List<WakeWordOption> = emptyList(),
    val wakeCatalogOffline: Boolean = false,
    /**
     * Catalog id of the head model actually loaded on this device (WS-5 M21.3). When it
     * differs from `doc.wakeWord` the selected phrase cannot be detected, and Settings
     * says so instead of letting the picker imply otherwise.
     */
    val activeWakeWordId: String = "",
    val micDevices: List<MicDeviceOption> = emptyList(),
    val personaPresets: List<PersonaPreset> = SettingsViewModel.PERSONA_PRESETS,
    /** Gemini Live voice catalog; empty until fetched (or when the fetch failed offline). */
    val geminiVoices: List<GeminiVoiceOption> = emptyList(),
    val porcupineAvailable: Boolean = false,
    val accountActionsAvailable: Boolean = false,
    val signedIn: Boolean = false,
    val signOutInProgress: Boolean = false,
    // ---- custom wake-word training (M6 FR-K03) ----
    val customPhrase: String = "",
    val customJob: CustomWakeJob? = null,
    val customRequestInProgress: Boolean = false,
    /** True when Live Ninja is exempt from Doze battery optimization (01-platform §C). */
    val batteryOptimizationIgnored: Boolean = false,
    /** Named hosts and per-host values keyed by configurable accordion. */
    val sectionScopes: Map<SettingsSection, SettingsSectionScopeUi> = emptyMap(),
    /** Remote previews; current runtime [doc] remains untouched while browsing. */
    val sectionDocuments: Map<SettingsSection, SettingsDocument> = emptyMap(),
    val devices: List<SettingsHostUi> = emptyList(),
    val settingsSyncing: Boolean = false,
) {
    val customPhraseValid: Boolean
        get() = SettingsViewModel.isValidWakePhrase(customPhrase)

    fun documentFor(section: SettingsSection): SettingsDocument =
        if (sectionScopes[section]?.viewedHost?.isCurrent == false) {
            sectionDocuments[section] ?: doc
        } else {
            doc
    }
}

private data class SectionSaveKey(
    val section: SettingsSection,
    val deviceId: String,
)

private data class PendingSectionSave(
    val mutations: List<(JSONObject) -> Unit>,
    val optimisticSettings: JSONObject,
    val editingCurrent: Boolean,
)

private fun mergePendingSectionSaves(
    older: PendingSectionSave,
    newer: PendingSectionSave,
): PendingSectionSave =
    newer.copy(mutations = older.mutations + newer.mutations)

@HiltViewModel
class SettingsViewModel @Inject constructor(
    @ApplicationContext private val context: Context,
    private val settingsStore: SettingsStore,
    private val settingsRepository: SettingsRepository,
    private val catalog: WakeWordCatalogRepository,
    private val api: LiveNinjaApi,
    private val modelManager: ModelManager,
    private val wakePrefs: WakePreferences,
    private val customStore: CustomWakeWordStore,
    private val logSink: LogSink,
    private val logExporter: LogExporter,
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
            batteryOptimizationIgnored = isIgnoringBatteryOptimizations(),
        ),
    )
    val state: StateFlow<SettingsUiState> = _state

    /**
     * Always-listening switch state. Read straight off [WakePreferences] rather than
     * mirrored into [SettingsUiState]: the wake service itself flips this flag when it
     * starts or stops (including from its notification's Stop action), so the switch has
     * to follow the service, not a copy of it.
     */
    val wakeServiceEnabled: StateFlow<Boolean> = wakePrefs.serviceEnabledFlow

    /**
     * Whether the wake service is *actually* running (WS-5 M21.4), as opposed to
     * [wakeServiceEnabled] which is only the persisted intent. The two diverge after a
     * reboot, because Android 15+ refuses a microphone FGS start from BOOT_COMPLETED.
     */
    val wakeServiceRunning: StateFlow<Boolean> = ninja.jeremy.liveninja.wake.WakeWordService.runningFlow

    init {
        // Track the loaded head model so the wake picker can flag a selection that has
        // no model behind it yet (WS-5 M21.3).
        viewModelScope.launch {
            modelManager.headModel.collect { ref ->
                _state.update { it.copy(activeWakeWordId = ref.wakeWordId) }
            }
        }
    }

    private val _notices = MutableSharedFlow<SettingsNotice>(extraBufferCapacity = 4)
    val notices: SharedFlow<SettingsNotice> = _notices

    private var modelSyncJob: Job? = null
    private var pollJob: Job? = null

    /**
     * Last catalog fetched, so a document change can rebuild without refetching.
     *
     * Must stay declared ABOVE the init block that reads it. viewModelScope runs on
     * Dispatchers.Main.immediate, so the `settingsStore.document.collect` below starts
     * synchronously during construction and StateFlow replays its current value before
     * suspending — that first emission reaches buildPersonaPresets() while the class body
     * is still initializing. Declared after that init block, the backing field is still
     * JVM-null there and the non-null parameter check throws, taking down the whole
     * Settings screen on open.
     */
    private var personaCatalog: List<PersonaInfoDto> = emptyList()
    private val settingsWrites = SettingsWriteCoordinator()
    private val sectionSaveQueue = LatestSaveQueue<SectionSaveKey, PendingSectionSave>(
        scope = viewModelScope,
        debounceMillis = SECTION_SAVE_DEBOUNCE_MILLIS,
        merge = ::mergePendingSectionSaves,
        save = ::saveQueuedSection,
        onDrained = { key ->
            if (key.deviceId == settingsRepository.currentDeviceId) {
                settingsRepository.clearSectionPending(key.section)
            }
        },
    )

    init {
        viewModelScope.launch {
            settingsStore.document.collect { doc ->
                _state.update {
                    // The picker depends on the document (persona.hidden and
                    // the selected id), so it is rebuilt here rather than only
                    // when the catalog is fetched — hiding a persona on the web
                    // reaches this picker on the next sync, with no refetch.
                    it.copy(doc = doc, personaPresets = buildPersonaPresets(personaCatalog))
                }
            }
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
        refreshGeminiVoices()
        refreshPersonas()
        // Resume polling a training job that outlived the previous process
        // (Batch jobs run up to 20 min; the SES "ready" email is the backstop).
        startPollingCustomJob()
        initializeDeviceSettings()
    }

    // ---- named devices + per-section settings ----

    /**
     * Register this random app-instance id, upload the pre-existing local
     * settings as current-device overrides exactly once, and only then adopt
     * the server's effective document. A failed migration leaves the local
     * document authoritative and retries on the next Settings ViewModel.
     */
    private fun initializeDeviceSettings() {
        viewModelScope.launch {
            _state.update { it.copy(settingsSyncing = true) }
            try {
                settingsRepository.registerCurrentDevice()
                loadDeviceDirectory()

                val migrationMode = initialSettingsMigrationMode(
                    migrationComplete = settingsRepository.migrationComplete,
                    hasPersistedDocument = settingsStore.hasPersistedDocument,
                )
                if (migrationMode == InitialSettingsMigrationMode.FRESH_INSTALL) {
                    // No legacy document exists: inherit the account's
                    // effective values instead of uploading synthesized
                    // Android defaults as per-device overrides.
                    settingsRepository.clearPendingSections()
                    settingsRepository.markMigrationComplete()
                }
                val migratingAllLocalSettings =
                    migrationMode == InitialSettingsMigrationMode.LEGACY_DOCUMENT
                val migrationSections = SettingsSection.entries
                    .filter { section ->
                        section.apiId != null &&
                            (
                                migratingAllLocalSettings ||
                                    (
                                        migrationMode == InitialSettingsMigrationMode.NONE &&
                                            section.apiId in
                                            settingsRepository.pendingSettingsSections
                                        )
                                )
                    }
                if (migrationSections.isNotEmpty()) {
                    val local = settingsStore.rawSnapshot()
                    for (section in migrationSections) {
                        val values = extractSectionSettings(section, local)
                        if (values.length() == 0) {
                            if (!migratingAllLocalSettings) {
                                settingsRepository.clearSectionPending(section)
                            }
                            continue
                        }
                        settingsWrites.write {
                            val envelope = settingsRepository.getSection(section)
                            patchWithConflictRetry(
                                section = section,
                                envelope = envelope,
                                operation = "set",
                                target = SettingsApplyTarget(SettingsTargetMode.CURRENT),
                                settingsFor = { values },
                            )
                        }
                        if (!migratingAllLocalSettings) {
                            settingsRepository.clearSectionPending(section)
                        }
                    }
                }
                if (migrationMode == InitialSettingsMigrationMode.LEGACY_DOCUMENT) {
                    settingsRepository.markMigrationComplete()
                }

                refreshEffectiveSettings()
                SettingsSection.entries
                    .filter { it.apiId != null }
                    .map { section -> async { loadSectionNow(section) } }
                    .awaitAll()
            } catch (_: Exception) {
                _notices.tryEmit(SettingsNotice.SETTINGS_SYNC_FAILED)
            } finally {
                _state.update { it.copy(settingsSyncing = false) }
            }
        }
    }

    fun loadSection(section: SettingsSection) {
        sectionSaveQueue.retryWhere { it.section == section }
        if (section.apiId == null) {
            viewModelScope.launch { loadDeviceDirectory() }
            return
        }
        if (_state.value.sectionScopes[section]?.hosts?.isNotEmpty() == true) return
        viewModelScope.launch {
            runCatching { loadSectionNow(section) }
                .onFailure { _notices.tryEmit(SettingsNotice.SETTINGS_SYNC_FAILED) }
        }
    }

    private suspend fun loadSectionNow(section: SettingsSection) {
        _state.update { state ->
            state.copy(
                sectionScopes = state.sectionScopes +
                    (section to (state.sectionScopes[section] ?: SettingsSectionScopeUi()).copy(loading = true)),
            )
        }
        consumeSectionEnvelope(section, settingsRepository.getSection(section))
    }

    private suspend fun loadDeviceDirectory() {
        val currentId = settingsRepository.currentDeviceId
        val devices = settingsRepository.listDevices().mapNotNull { dto ->
            val id = dto.deviceKey ?: return@mapNotNull null
            SettingsHostUi(
                id = id,
                name = dto.displayName.ifBlank { id },
                surface = dto.surface ?: dto.platform ?: dto.type,
                metadata = dto.metadata.toStringMap(),
                capabilities = dto.capabilities.toSet(),
                isCurrent = dto.isCurrent || id == currentId,
            )
        }
        _state.update { it.copy(devices = devices) }
    }

    private fun consumeSectionEnvelope(
        section: SettingsSection,
        envelope: ninja.jeremy.liveninja.net.SettingsSectionEnvelope,
    ) {
        val currentId = envelope.currentDeviceId ?: settingsRepository.currentDeviceId
        val directoryHosts = envelope.devices.map { dto ->
            SettingsHostUi(
                id = dto.deviceId,
                name = dto.name.ifBlank { dto.deviceId },
                surface = dto.surface,
                metadata = dto.metadata.toStringMap(),
                capabilities = dto.capabilities.toSet(),
                isCurrent = dto.isCurrent || dto.deviceId == currentId,
                inherited = dto.inherited,
                settings = JSONObject(dto.settings.toString()),
            )
        }
        _state.update { state ->
            val existing = state.sectionScopes[section]
            if (!shouldAcceptSectionEnvelope(envelope.version, existing?.version)) {
                state.copy(
                    sectionScopes = state.sectionScopes +
                        (section to requireNotNull(existing).copy(loading = false)),
                )
            } else {
                val hosts = directoryHosts
                val viewedId = supportedSettingsHostId(
                    section = section,
                    hosts = hosts,
                    previousDeviceId = existing?.viewedDeviceId,
                )
                val viewedSettings =
                    hosts.firstOrNull { it.id == viewedId }?.settings ?: JSONObject()
                state.copy(
                    sectionScopes = state.sectionScopes +
                        (
                            section to SettingsSectionScopeUi(
                                version = envelope.version,
                                hosts = hosts,
                                viewedDeviceId = viewedId,
                                loading = false,
                            )
                        ),
                    sectionDocuments = state.sectionDocuments +
                        (section to settingsStore.preview(viewedSettings)),
                    devices = if (state.devices.isEmpty()) directoryHosts else state.devices,
                )
            }
        }
    }

    fun viewSettingsFor(section: SettingsSection, deviceId: String) {
        val scope = _state.value.sectionScopes[section] ?: return
        val host = scope.hosts.firstOrNull { it.id == deviceId } ?: return
        if (!host.supports(section)) return
        _state.update { state ->
            state.copy(
                sectionScopes = state.sectionScopes +
                    (section to scope.copy(viewedDeviceId = deviceId)),
                sectionDocuments = state.sectionDocuments +
                    (section to settingsStore.preview(host.settings)),
            )
        }
    }

    fun applySection(
        section: SettingsSection,
        target: SettingsApplyTarget,
        inherit: Boolean,
    ) {
        if (inherit && target.mode == SettingsTargetMode.ALL) return
        val scope = _state.value.sectionScopes[section] ?: return
        val viewed = scope.viewedHost
        val settings = settingsForApply(
            section = section,
            currentDocument = _state.value.documentFor(section).raw,
            viewedSettings = viewed?.settings,
            viewedIsCurrent = viewed?.isCurrent == true,
        )
        if (!inherit &&
            section == SettingsSection.MICROPHONE &&
            !microphoneSettingsCanApply(
                settings = settings,
                sourceIsCurrent = viewed?.isCurrent == true,
                targetMode = target.mode,
            )
        ) {
            _notices.tryEmit(SettingsNotice.MICROPHONE_TARGET_LOCAL_ONLY)
            return
        }
        val destinationIds = when (target.mode) {
            SettingsTargetMode.CURRENT -> setOf(settingsRepository.currentDeviceId)
            SettingsTargetMode.SELECTED -> target.deviceIds
            SettingsTargetMode.ALL ->
                scope.hosts.mapTo(mutableSetOf()) { it.id } + settingsRepository.currentDeviceId
        }
        val supersededSaves = destinationIds.mapNotNull { deviceId ->
            val key = SectionSaveKey(section, deviceId)
            sectionSaveQueue.discard(key)?.let { pending -> key to pending }
        }.toMap()
        viewModelScope.launch {
            var patchCommitted = false
            try {
                settingsWrites.write {
                    val envelope = settingsRepository.getSection(section)
                    val refreshed = patchWithConflictRetry(
                        section = section,
                        envelope = envelope,
                        operation = if (inherit) "inherit" else "set",
                        target = target,
                        settingsFor = {
                            if (inherit) JSONObject() else JSONObject(settings.toString())
                        },
                    )
                    patchCommitted = true
                    consumeSectionEnvelope(section, refreshed)
                    val currentKey = SectionSaveKey(section, settingsRepository.currentDeviceId)
                    if (targetIncludesCurrent(target) &&
                        !sectionSaveQueue.hasPending(currentKey)
                    ) {
                        refreshEffectiveSettings()
                    }
                }
                val currentKey = SectionSaveKey(section, settingsRepository.currentDeviceId)
                if (targetIncludesCurrent(target) &&
                    !sectionSaveQueue.hasPending(currentKey)
                ) {
                    settingsRepository.clearSectionPending(section)
                }
                _notices.tryEmit(
                    if (inherit) SettingsNotice.SETTINGS_INHERITED else SettingsNotice.SETTINGS_APPLIED,
                )
            } catch (_: Exception) {
                if (!patchCommitted) {
                    destinationIds.forEach { deviceId ->
                        val key = SectionSaveKey(section, deviceId)
                        val newer = sectionSaveQueue.discard(key)
                        val older = supersededSaves[key]
                        val restored = when {
                            older != null && newer != null ->
                                mergePendingSectionSaves(older, newer)
                            older != null -> older
                            else -> newer
                        }
                        restored?.let { pending ->
                            sectionSaveQueue.submit(key, pending)
                            if (pending.editingCurrent) {
                                settingsRepository.markSectionPending(section)
                            }
                        }
                    }
                } else {
                    val currentKey = SectionSaveKey(section, settingsRepository.currentDeviceId)
                    if (targetIncludesCurrent(target) &&
                        !sectionSaveQueue.hasPending(currentKey)
                    ) {
                        settingsRepository.clearSectionPending(section)
                    }
                }
                _notices.tryEmit(SettingsNotice.SETTINGS_SYNC_FAILED)
            }
        }
    }

    private suspend fun patchWithConflictRetry(
        section: SettingsSection,
        envelope: ninja.jeremy.liveninja.net.SettingsSectionEnvelope,
        operation: String,
        target: SettingsApplyTarget,
        settingsFor: (ninja.jeremy.liveninja.net.SettingsSectionEnvelope) -> JSONObject,
    ): ninja.jeremy.liveninja.net.SettingsSectionEnvelope {
        return try {
            settingsRepository.patchSection(
                section,
                envelope.version,
                operation,
                target,
                settingsFor(envelope),
            )
        } catch (e: HttpException) {
            if (e.code() != 409) throw e
            val fresh = settingsRepository.getSection(section)
            settingsRepository.patchSection(
                section,
                fresh.version,
                operation,
                target,
                settingsFor(fresh),
            )
        }
    }

    private suspend fun refreshEffectiveSettings() {
        val previous = settingsStore.document.value
        settingsStore.replaceFromServer(settingsRepository.getEffectiveSettings())
        reconcileEffectiveRuntime(previous, settingsStore.document.value)
    }

    private fun reconcileEffectiveRuntime(
        previous: SettingsDocument,
        effective: SettingsDocument,
    ) {
        wakePrefs.wakeWordId = effective.wakeWord
        wakePrefs.wakeEngine = effective.wakeEngine
        wakePrefs.sensitivity = effective.sensitivity
        if (previous.wakeWord != effective.wakeWord ||
            previous.wakeEngine != effective.wakeEngine
        ) {
            syncWakeModel(effective.wakeWord, effective.wakeEngine)
        }
    }

    private fun targetIncludesCurrent(target: SettingsApplyTarget): Boolean =
        targetIncludesDevice(target, settingsRepository.currentDeviceId)

    private fun targetIncludesDevice(target: SettingsApplyTarget, deviceId: String): Boolean =
        when (target.mode) {
            SettingsTargetMode.CURRENT -> deviceId == settingsRepository.currentDeviceId
            SettingsTargetMode.SELECTED -> deviceId in target.deviceIds
            SettingsTargetMode.ALL -> true
        }

    fun renameDevice(deviceId: String, name: String) {
        if (name.isBlank()) return
        viewModelScope.launch {
            try {
                settingsRepository.renameDevice(deviceId, name)
                loadDeviceDirectory()
                SettingsSection.entries.filter { it.apiId != null }.forEach { section ->
                    runCatching { loadSectionNow(section) }
                }
                _notices.tryEmit(SettingsNotice.DEVICE_RENAMED)
            } catch (_: Exception) {
                _notices.tryEmit(SettingsNotice.DEVICE_RENAME_FAILED)
            }
        }
    }

    /**
     * Apply one portable edit to the host currently being viewed for this
     * section. Current-host edits update runtime state immediately; remote
     * previews are updated independently and never flow into WakeWordService,
     * theme, logging, or the active conversation.
     */
    private fun editPortableSection(
        section: SettingsSection,
        afterCurrentEdit: () -> Unit = {},
        mutate: (JSONObject) -> Unit,
    ) {
        val state = _state.value
        val scope = state.sectionScopes[section]
        val viewed = scope?.viewedHost
        if (viewed != null && !viewed.supports(section)) return
        val editingCurrent = viewed == null || viewed.isCurrent ||
            viewed.id == settingsRepository.currentDeviceId
        val targetDeviceId = viewed?.id ?: settingsRepository.currentDeviceId

        val sectionValues: JSONObject
        if (editingCurrent) {
            settingsStore.update(mutate)
            afterCurrentEdit()
            sectionValues = extractSectionSettings(section, settingsStore.rawSnapshot())
        } else {
            val previewRaw = JSONObject(state.documentFor(section).raw.toString())
            mutate(previewRaw)
            sectionValues = extractSectionSettings(section, previewRaw)
        }

        if (scope != null) {
            val updatedHosts = scope.hosts.map { host ->
                if (host.id == targetDeviceId) {
                    host.copy(
                        settings = JSONObject(sectionValues.toString()),
                        inherited = false,
                    )
                } else {
                    host
                }
            }
            _state.update {
                it.copy(
                    sectionScopes = it.sectionScopes +
                        (section to scope.copy(hosts = updatedHosts)),
                    sectionDocuments = if (editingCurrent) {
                        it.sectionDocuments
                    } else {
                        it.sectionDocuments +
                            (section to settingsStore.preview(sectionValues))
                    },
                )
            }
        }

        val key = SectionSaveKey(section, targetDeviceId)
        sectionSaveQueue.submit(
            key,
            PendingSectionSave(
                mutations = listOf(mutate),
                optimisticSettings = JSONObject(sectionValues.toString()),
                editingCurrent = editingCurrent,
            ),
        )
        if (editingCurrent) {
            settingsRepository.markSectionPending(section)
        }
    }

    /**
     * Save the coalesced mutations against a freshly-read host baseline. A
     * 409 performs the same rebase again, so unknown concurrent fields survive
     * and an older full section can never overwrite a newer server edit.
     */
    private suspend fun saveQueuedSection(
        key: SectionSaveKey,
        pending: PendingSectionSave,
    ): Boolean {
        return try {
            settingsWrites.write {
                val envelope = settingsRepository.getSection(key.section)
                val target = if (pending.editingCurrent) {
                    SettingsApplyTarget(SettingsTargetMode.CURRENT)
                } else {
                    SettingsApplyTarget(SettingsTargetMode.SELECTED, setOf(key.deviceId))
                }
                val refreshed = patchWithConflictRetry(
                    section = key.section,
                    envelope = envelope,
                    operation = "set",
                    target = target,
                    settingsFor = { fresh ->
                        rebaseSectionMutations(key, pending, fresh)
                    },
                )
                val drained = !sectionSaveQueue.hasWaiting(key)
                val confirmedSettings = if (drained) {
                    deviceSectionSettingsAfterSave(
                        deviceId = key.deviceId,
                        refreshed = refreshed,
                        optimisticSettings = pending.optimisticSettings,
                    )
                } else {
                    null
                }
                _state.update { state ->
                    val scope = state.sectionScopes[key.section]
                    if (scope == null) {
                        state
                    } else {
                        val viewingSavedRemote =
                            confirmedSettings != null &&
                                !pending.editingCurrent &&
                                scope.viewedDeviceId == key.deviceId
                        state.copy(
                            sectionScopes = state.sectionScopes +
                                (
                                    key.section to scope.copy(
                                        version = refreshed.version,
                                        hosts = if (confirmedSettings == null) {
                                            scope.hosts
                                        } else {
                                            scope.hosts.map { host ->
                                                if (host.id == key.deviceId) {
                                                    host.copy(
                                                        settings = JSONObject(
                                                            confirmedSettings.toString(),
                                                        ),
                                                        inherited = false,
                                                    )
                                                } else {
                                                    host
                                                }
                                            }
                                        },
                                    )
                                ),
                            sectionDocuments = if (viewingSavedRemote) {
                                state.sectionDocuments +
                                    (
                                        key.section to settingsStore.preview(
                                            requireNotNull(confirmedSettings),
                                        )
                                    )
                            } else {
                                state.sectionDocuments
                            },
                        )
                    }
                }
                if (confirmedSettings != null && pending.editingCurrent) {
                    val previous = settingsStore.document.value
                    settingsStore.applySyncedSection(confirmedSettings)
                    reconcileEffectiveRuntime(previous, settingsStore.document.value)
                }
            }
            true
        } catch (cancelled: CancellationException) {
            throw cancelled
        } catch (_: Exception) {
            _notices.tryEmit(SettingsNotice.SETTINGS_SYNC_FAILED)
            false
        }
    }

    private fun rebaseSectionMutations(
        key: SectionSaveKey,
        pending: PendingSectionSave,
        envelope: ninja.jeremy.liveninja.net.SettingsSectionEnvelope,
    ): JSONObject {
        val base = envelope.devices
            .firstOrNull { it.deviceId == key.deviceId }
            ?.settings
            ?: envelope.accountDefaults
        return applySectionMutations(
            section = key.section,
            baseline = JSONObject(base.toString()),
            mutations = pending.mutations,
        )
    }

    private fun editCurrentHostOnly(
        section: SettingsSection,
        edit: () -> Unit,
    ) {
        val viewed = _state.value.sectionScopes[section]?.viewedHost
        if (viewed != null && !viewed.isCurrent && viewed.id != settingsRepository.currentDeviceId) {
            _notices.tryEmit(SettingsNotice.MICROPHONE_TARGET_LOCAL_ONLY)
            return
        }
        edit()
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
        editPortableSection(
            section = SettingsSection.WAKE_WORD,
            mutate = { it.put("wakeWord", id) },
            afterCurrentEdit = {
                wakePrefs.wakeWordId = id
                syncWakeModel(id, wakePrefs.wakeEngine)
            },
        )
    }

    fun setWakeEngine(engine: String) {
        editPortableSection(
            section = SettingsSection.WAKE_WORD,
            mutate = { it.put("wakeEngine", engine) },
            afterCurrentEdit = {
                wakePrefs.wakeEngine = engine
                syncWakeModel(wakePrefs.wakeWordId, engine)
            },
        )
    }

    fun setSensitivity(value: Float) {
        val clamped = value.coerceIn(0f, 1f)
        editPortableSection(
            section = SettingsSection.WAKE_WORD,
            mutate = { it.put("sensitivity", clamped.toDouble()) },
            afterCurrentEdit = {
                // Write-through: the engine consumes sensitivityFlow live.
                wakePrefs.sensitivity = clamped
            },
        )
    }

    private fun syncWakeModel(id: String, engine: String) {
        modelSyncJob?.cancel()
        modelSyncJob = viewModelScope.launch {
            when (modelManager.sync(id, engine)) {
                is ModelSyncResult.Active -> _notices.tryEmit(SettingsNotice.WAKE_MODEL_READY)
                // Builtin bytes ship in the apk: selecting one has fully succeeded, even though
                // the server answers its manifest route with a by-design 404.
                is ModelSyncResult.Builtin -> _notices.tryEmit(SettingsNotice.WAKE_MODEL_BUILTIN)
                // Nothing is broken — the catalog just offers a phrase nobody has trained.
                is ModelSyncResult.NotTrained -> _notices.tryEmit(SettingsNotice.WAKE_MODEL_UNTRAINED)
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
        val doc = _state.value.documentFor(SettingsSection.PERSONA)
        editPortableSection(SettingsSection.PERSONA) {
            val persona = it.optJSONObject("persona") ?: JSONObject()
            persona.put("presetId", presetId)
            persona.put(
                "systemInstructions",
                if (presetId == "custom") doc.personaSystemInstructions.orEmpty() else JSONObject.NULL,
            )
            it.put("persona", persona)
        }
    }

    fun setCustomInstructions(text: String) {
        editPortableSection(SettingsSection.PERSONA) {
            val persona = it.optJSONObject("persona") ?: JSONObject()
            persona.put("presetId", "custom")
            persona.put("systemInstructions", text.take(CUSTOM_INSTRUCTIONS_MAX))
            it.put("persona", persona)
        }
    }

    fun setVoice(voice: String) =
        editPortableSection(SettingsSection.PERSONA) { it.put("voice", voice) }

    fun onVoicePreviewRequested() {
        // No bundled samples ship with the app and the backend TTS preview
        // endpoint doesn't exist yet — surface the designed "unavailable" notice.
        _notices.tryEmit(SettingsNotice.VOICE_PREVIEW_UNAVAILABLE)
    }

    fun setTurnDetection(value: String) =
        editPortableSection(SettingsSection.TURN_DETECTION) { it.put("turnDetection", value) }

    /** Voice engine picker (M12 FR-VE-04): sets voiceEngine.default. */
    fun setVoiceEngine(engine: String) {
        editPortableSection(SettingsSection.VOICE_ENGINE) {
            val voiceEngine = it.optJSONObject("voiceEngine")
                ?: JSONObject().apply { put("devices", JSONObject()) }
            voiceEngine.put("default", engine)
            if (!voiceEngine.has("devices")) voiceEngine.put("devices", JSONObject())
            it.put("voiceEngine", voiceEngine)
        }
        // The Gemini voice picker appears with this selection; retry the
        // catalog fetch if the init-time attempt failed (e.g. offline).
        if (engine == GEMINI_ENGINE && _state.value.geminiVoices.isEmpty()) {
            refreshGeminiVoices()
        }
    }

    /** Gemini voice picker (M13, D4): sets the top-level `geminiVoice` key. */
    fun setGeminiVoice(voiceId: String) =
        editPortableSection(SettingsSection.VOICE_ENGINE) { it.put("geminiVoice", voiceId) }

    /**
     * Fetch the persona catalog from `GET /api/v1/realtime/personas` and build
     * the picker list.
     *
     * Three things this has to get right, none of which the old hardcoded list
     * did:
     *
     *  - **Hidden personas.** `persona.hidden` (2026-08-01) is the picker
     *    off-switch. It is presentation only — the server's ResolvePersona
     *    never reads it — so it is applied here and nowhere else.
     *  - **The selected persona always survives.** If the stored `presetId`
     *    is not in the fetched catalog (hidden, deleted, or a server older
     *    than this build), it is appended rather than dropped. Without that
     *    the picker silently displays a DIFFERENT persona than the one in the
     *    document, and the next save writes that wrong value back.
     *  - **`custom` is client-side.** The schema defines it; the server
     *    catalog does not list it, so it is appended last.
     *
     * Failure leaves the current list untouched (offline keeps the fallback,
     * or whatever was fetched last), matching refreshGeminiVoices().
     */
    private fun refreshPersonas() {
        viewModelScope.launch {
            val catalog = try {
                api.listPersonas()
            } catch (_: Exception) {
                return@launch // transient — the fallback list stays usable
            }
            personaCatalog = catalog.personas
            _state.update { it.copy(personaPresets = buildPersonaPresets(personaCatalog)) }
        }
    }

    /** Catalog -> picker list. Pure, so the rules above are unit-testable. */
    internal fun buildPersonaPresets(catalog: List<PersonaInfoDto>): List<PersonaPreset> {
        if (catalog.isEmpty()) return PERSONA_PRESETS
        val selectedId = settingsStore.document.value.personaPresetId
        val hidden = settingsStore.document.value.hiddenPersonas
        val presets = catalog
            .filter { it.id == selectedId || it.id == "default" || it.id !in hidden }
            .map {
                PersonaPreset(
                    id = it.id,
                    label = it.name ?: it.id,
                    description = it.description.orEmpty(),
                    group = it.group,
                )
            }
            .toMutableList()

        // A stored persona the catalog does not list is kept, labelled so the
        // state is visible rather than silently corrected.
        if (selectedId.isNotEmpty() &&
            selectedId != CUSTOM_PERSONA_ID &&
            presets.none { it.id == selectedId }
        ) {
            presets += PersonaPreset(selectedId, selectedId, "Kept as-is — not in the catalog")
        }
        if (presets.none { it.id == "default" }) {
            presets.add(0, PERSONA_PRESETS.first())
        }
        presets += PersonaPreset(CUSTOM_PERSONA_ID, "Custom", "Write your own system instructions")
        return presets
    }

    /**
     * Fetch the Gemini Live voice catalog (`geminiVoices` on
     * GET /api/v1/realtime/voices). Failure leaves the current list — the
     * picker shows its offline note instead of an empty combobox.
     */
    private fun refreshGeminiVoices() {
        viewModelScope.launch {
            val catalog = try {
                api.listVoices()
            } catch (_: Exception) {
                return@launch // transient — retried when the engine is selected
            }
            val options = catalog.geminiVoices.map { dto ->
                GeminiVoiceOption(
                    id = dto.id,
                    label = dto.name ?: dto.id,
                    description = dto.description.orEmpty(),
                    default = dto.default,
                )
            }
            _state.update { it.copy(geminiVoices = options) }
        }
    }

    // ---- Voice & Screen (01-platform §B-iv) ----
    fun setLockedSessions(enabled: Boolean) =
        editCurrentHostOnly(SettingsSection.WAKE_WORD) { settingsStore.setLockedSessions(enabled) }
    fun setWakeScreenOnWake(enabled: Boolean) =
        editCurrentHostOnly(SettingsSection.WAKE_WORD) { settingsStore.setWakeScreenOnWake(enabled) }
    fun setKeepScreenOn(enabled: Boolean) =
        editCurrentHostOnly(SettingsSection.WAKE_WORD) { settingsStore.setKeepScreenOn(enabled) }

    // ---- Battery optimization health (01-platform §C) ----

    /** Re-read the Doze exemption state (call when returning from the system prompt). */
    fun refreshBatteryStatus() =
        _state.update { it.copy(batteryOptimizationIgnored = isIgnoringBatteryOptimizations()) }

    /**
     * Intent that opens the per-app "ignore battery optimizations" system
     * prompt. Requires the REQUEST_IGNORE_BATTERY_OPTIMIZATIONS permission
     * (declared in the manifest). Launched from the screen so the result can be
     * observed on ON_RESUME via [refreshBatteryStatus].
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

    // ---- Diagnostics / verbose logging (04-logging §A5) ----
    fun setDiagnosticsEnabled(enabled: Boolean) =
        editCurrentHostOnly(SettingsSection.PRIVACY) { settingsStore.setDiagnosticsEnabled(enabled) }
    fun setDiagnosticsMinLevel(level: String) =
        editCurrentHostOnly(SettingsSection.PRIVACY) { settingsStore.setDiagnosticsMinLevel(level) }
    fun setDiagnosticsCategory(category: String, enabled: Boolean) =
        editCurrentHostOnly(SettingsSection.PRIVACY) {
            settingsStore.setDiagnosticsCategory(category, enabled)
        }

    /** Select-all / select-none for the eight capture categories, preserving enabled + minLevel. */
    fun setAllDiagnosticsCategories(enabled: Boolean) {
        editCurrentHostOnly(SettingsSection.PRIVACY) {
            val current = _state.value.doc.diagnostics
            settingsStore.setDiagnostics(
                current.copy(categories = DiagnosticsConfig.CATEGORY_KEYS.associateWith { enabled }),
            )
        }
    }

    /** Flush + zip the logs and return an ACTION_SEND share Intent (null = nothing to export). */
    suspend fun exportLogs(): Intent? = logExporter.exportZip()

    /** Clear the in-app ring buffer (rotated files on disk are unaffected). */
    fun clearLogs() = logSink.clear()

    // ---- Audio ----
    fun setMicDevice(id: String?) {
        val viewed = _state.value.sectionScopes[SettingsSection.MICROPHONE]?.viewedHost
        val remote = viewed != null && !viewed.isCurrent &&
            viewed.id != settingsRepository.currentDeviceId
        if (remote && id != null) {
            _notices.tryEmit(SettingsNotice.MICROPHONE_TARGET_LOCAL_ONLY)
            return
        }
        editPortableSection(SettingsSection.MICROPHONE) {
            it.put("micDeviceId", id ?: JSONObject.NULL)
        }
    }

    fun refreshMicDevices() = _state.update { it.copy(micDevices = enumerateMicDevices()) }

    // ---- Appearance ----
    fun setTheme(theme: String) =
        editPortableSection(SettingsSection.APPEARANCE) { it.put("theme", theme) }

    /** Style picker (M8.1/M8.2, 03-theme): hal9000/ninja/minimal/terminal. */
    fun setAppStyle(style: String) =
        editPortableSection(SettingsSection.APPEARANCE) {
            val appearance = it.optJSONObject("appearance") ?: JSONObject()
            appearance.put("appStyle", style)
            it.put("appearance", appearance)
            it.remove("appStyle")
        }

    // ---- Privacy ----
    fun setStoreAudio(enabled: Boolean) =
        editPortableSection(SettingsSection.PRIVACY) {
            val privacy = it.optJSONObject("privacy") ?: JSONObject()
            privacy.put("storeAudio", enabled)
            it.put("privacy", privacy)
        }

    fun setStoreTranscripts(enabled: Boolean) =
        editPortableSection(SettingsSection.PRIVACY) {
            val privacy = it.optJSONObject("privacy") ?: JSONObject()
            privacy.put("storeTranscripts", enabled)
            it.put("privacy", privacy)
        }

    fun setRetentionDays(days: Int) =
        editPortableSection(SettingsSection.PRIVACY) {
            val privacy = it.optJSONObject("privacy") ?: JSONObject()
            privacy.put("retentionDays", days)
            it.put("privacy", privacy)
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
                settingsRepository.clearPendingSections()
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

        /** Engine value whose selection reveals the Gemini voice picker (M13). */
        const val GEMINI_ENGINE = "gemini-flash-live"

        /** Azure OpenAI Realtime pins (azure-voice-plan.md WS-F M2). */
        const val AZURE_ENGINE = "gpt-live-azure"
        const val AZURE_MINI_ENGINE = "gpt-live-azure-mini"

        /** Client-side pre-check mirror of the backend phrase validation. */
        const val MAX_PHRASE_LENGTH = 40
        const val MIN_PHRASE_LENGTH = 3
        private const val MAX_PHRASE_WORDS = 6

        /** Poll cadence for an in-flight training job (jobs run minutes, not seconds). */
        const val POLL_INTERVAL_MS = 15_000L
        private const val SECTION_SAVE_DEBOUNCE_MILLIS = 300L

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
        /**
         * Offline/first-paint fallback ONLY. The real catalog comes from
         * `GET /api/v1/realtime/personas` (refreshPersonas()).
         *
         * This list used to be the whole catalog, and it was wrong: it offered
         * `focused`, `friendly`, `coach` and `analyst`, none of which exist in
         * the server registry. ResolvePersona falls back to `default` for an
         * unknown id, so picking "Coach Ninja" on Android silently gave you the
         * standard persona and nothing said so. Only the two ids that are real
         * everywhere survive here: `default`, which the server guarantees, and
         * `custom`, which is a client-side concept the schema defines.
         */
        val PERSONA_PRESETS = listOf(
            PersonaPreset("default", "Live Ninja", "Fast, warm, and practical", "General"),
            PersonaPreset(CUSTOM_PERSONA_ID, "Custom", "Write your own system instructions"),
        )

        const val CUSTOM_PERSONA_ID = "custom"
    }
}

private fun JsonObject.toStringMap(): Map<String, String> =
    entries.mapNotNull { (key, value) ->
        value.jsonPrimitive.contentOrNull?.let { key to it }
    }.toMap()
