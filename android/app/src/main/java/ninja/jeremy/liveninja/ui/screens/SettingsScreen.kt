@file:OptIn(ExperimentalMaterial3Api::class)

package ninja.jeremy.liveninja.ui.screens

import android.content.Intent
import android.net.Uri
import android.os.Build
import android.provider.Settings
import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.selection.toggleable
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.KeyboardArrowRight
import androidx.compose.material.icons.filled.PlayCircleOutline
import androidx.compose.material.icons.outlined.CheckCircle
import androidx.compose.material.icons.outlined.Description
import androidx.compose.material.icons.outlined.WarningAmber
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExposedDropdownMenuBox
import androidx.compose.material3.ExposedDropdownMenuDefaults
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.MenuAnchorType
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SegmentedButton
import androidx.compose.material3.SegmentedButtonDefaults
import androidx.compose.material3.SingleChoiceSegmentedButtonRow
import androidx.compose.material3.Slider
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.platform.LocalContext
import androidx.core.content.ContextCompat
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.semantics.stateDescription
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import kotlin.math.roundToInt
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.wake.WakeSwitchAction
import ninja.jeremy.liveninja.wake.WakeSwitchDisplay
import ninja.jeremy.liveninja.wake.WakeWordService
import ninja.jeremy.liveninja.wake.decideWakeSwitchAction
import ninja.jeremy.liveninja.wake.wakeSwitchDisplay
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.ui.settings.CustomWakeJob
import ninja.jeremy.liveninja.ui.settings.resolveWakePhrase
import ninja.jeremy.liveninja.ui.settings.GeminiVoiceOption
import ninja.jeremy.liveninja.ui.settings.MicDeviceOption
import ninja.jeremy.liveninja.ui.settings.PersonaPreset
import ninja.jeremy.liveninja.ui.settings.SettingsNotice
import ninja.jeremy.liveninja.ui.settings.SettingsViewModel
import ninja.jeremy.liveninja.ui.state.DiagnosticsConfig
import ninja.jeremy.liveninja.ui.state.SettingsDocument
import ninja.jeremy.liveninja.ui.state.WakeWordOption
import ninja.jeremy.liveninja.ui.theme.LocalLiveNinjaColors

/**
 * Settings tab — schema-driven form over contracts/settings.schema.json
 * (mockups/android/09-settings.html). Every enumerable field is a populated
 * control (combobox/radio/slider/segmented/switch); the single free-text field
 * is the custom-persona system instructions, the schema's one justified case.
 *
 * M22.1: this screen is a LazyColumn of one item per settings group (Wake,
 * Conversation, Audio, Voice & Screen, Appearance, Privacy, Diagnostics,
 * Account) instead of a ~1300-line Column in a single composable. Two things
 * were measured on-device (Tab S9 FE) driving that split: (1) a single giant
 * composable forced the whole screen to JIT-compile and compose on first
 * frame (55 dropped frames / 705ms) — LazyColumn only composes the items that
 * are actually visible; (2) every field lived in one recomposition scope, so
 * moving the sensitivity slider recomposed the Account section too. Each
 * section below takes only the primitive fields it reads (never the whole
 * `doc`/`state` object) so a change in one group can't force recomposition of
 * an unrelated one. Per-section local UI state (export-in-progress, confirm
 * dialogs) that used to live at the top of this file now lives inside the
 * section that owns it, for the same reason — and so it isn't allocated until
 * that item actually composes.
 *
 * Organization note: this app already keeps every screen (ConversationScreen,
 * HistoryScreen, MemoryScreen, FilesScreen) as one file with private section
 * composables rather than splitting into sibling files under ui/screens/<x>/
 * — the ui/<feature>/ sibling directories in this codebase hold ViewModels
 * and Repositories, not composables. This file follows that existing
 * convention: one file, many small private composables.
 */
@Composable
fun SettingsScreen(
    modifier: Modifier = Modifier,
    onOpenLogViewer: () -> Unit = {},
) {
    val viewModel: SettingsViewModel = hiltViewModel()
    val state by viewModel.state.collectAsStateWithLifecycle()
    val wakeServiceEnabled by viewModel.wakeServiceEnabled.collectAsStateWithLifecycle()
    val wakeServiceRunning by viewModel.wakeServiceRunning.collectAsStateWithLifecycle()
    val doc = state.doc
    val snackbarHostState = remember { SnackbarHostState() }
    val context = LocalContext.current

    // Re-check the battery-optimization exemption whenever the user returns from
    // the system prompt (the result arrives out-of-band on ON_RESUME).
    val lifecycleOwner = LocalLifecycleOwner.current
    DisposableEffect(lifecycleOwner) {
        val observer = LifecycleEventObserver { _, event ->
            if (event == Lifecycle.Event.ON_RESUME) viewModel.refreshBatteryStatus()
        }
        lifecycleOwner.lifecycle.addObserver(observer)
        onDispose { lifecycleOwner.lifecycle.removeObserver(observer) }
    }
    val batteryLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { viewModel.refreshBatteryStatus() }
    // The system App Info page also exposes its own battery toggle (and, on
    // Samsung, the "Sleeping apps" list) — re-check on return same as above.
    val appInfoLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { viewModel.refreshBatteryStatus() }

    LaunchedEffect(Unit) {
        viewModel.notices.collect { notice ->
            val messageRes = when (notice) {
                SettingsNotice.VOICE_PREVIEW_UNAVAILABLE -> R.string.settings_voice_preview_unavailable
                SettingsNotice.SIGNED_OUT -> R.string.settings_signed_out
                SettingsNotice.SIGNED_OUT_EVERYWHERE -> R.string.settings_signed_out_everywhere
                SettingsNotice.SIGN_OUT_FAILED -> R.string.settings_sign_out_failed
                SettingsNotice.WAKE_MODEL_READY -> R.string.settings_wake_model_ready
                SettingsNotice.WAKE_MODEL_SIGNED_OUT -> R.string.settings_wake_model_signed_out
                SettingsNotice.WAKE_MODEL_FAILED -> R.string.settings_wake_model_failed
                SettingsNotice.WAKE_TRAIN_REQUESTED -> R.string.settings_wake_train_requested
                SettingsNotice.WAKE_TRAIN_READY -> R.string.settings_wake_train_ready
                SettingsNotice.WAKE_TRAIN_FAILED -> R.string.settings_wake_train_failed
                SettingsNotice.WAKE_TRAIN_LIMIT -> R.string.settings_wake_train_limit
                SettingsNotice.WAKE_TRAIN_INVALID -> R.string.settings_wake_train_invalid
                SettingsNotice.WAKE_TRAIN_REQUEST_FAILED -> R.string.settings_wake_train_request_failed
            }
            snackbarHostState.showSnackbar(context.getString(messageRes))
        }
    }

    Scaffold(
        modifier = modifier.fillMaxSize(),
        snackbarHost = { SnackbarHost(snackbarHostState) },
    ) { padding ->
        LazyColumn(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding),
            contentPadding = PaddingValues(horizontal = 16.dp, vertical = 8.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            item(key = "wake") {
                WakeSection(
                    wakeServiceEnabled = wakeServiceEnabled,
                    wakeServiceRunning = wakeServiceRunning,
                    wakeWord = doc.wakeWord,
                    wakeOptions = state.wakeOptions,
                    wakeCatalogOffline = state.wakeCatalogOffline,
                    wakeEngine = doc.wakeEngine,
                    porcupineAvailable = state.porcupineAvailable,
                    sensitivity = doc.sensitivity,
                    activeWakeWordId = state.activeWakeWordId,
                    customPhrase = state.customPhrase,
                    customPhraseValid = state.customPhraseValid,
                    customRequestInProgress = state.customRequestInProgress,
                    customJob = state.customJob,
                    onSetWakeWord = viewModel::setWakeWord,
                    onSetWakeEngine = viewModel::setWakeEngine,
                    onSetSensitivity = viewModel::setSensitivity,
                    onSetCustomPhrase = viewModel::setCustomPhrase,
                    onRequestCustomWakeWord = viewModel::requestCustomWakeWord,
                    onUseCustomWakeWord = viewModel::useCustomWakeWord,
                    onClearCustomJob = viewModel::clearCustomJob,
                )
            }
            item(key = "divider_wake") { HorizontalDivider(Modifier.padding(vertical = 8.dp)) }

            item(key = "conversation") {
                ConversationSection(
                    personaPresetId = doc.personaPresetId,
                    personaSystemInstructions = doc.personaSystemInstructions,
                    personaPresets = state.personaPresets,
                    displayVoice = doc.displayVoice,
                    turnDetection = doc.turnDetection,
                    voiceEngineDefault = doc.voiceEngineDefault,
                    geminiVoice = doc.geminiVoice,
                    geminiVoices = state.geminiVoices,
                    onSetPersona = viewModel::setPersona,
                    onSetCustomInstructions = viewModel::setCustomInstructions,
                    onSetVoice = viewModel::setVoice,
                    onVoicePreviewRequested = viewModel::onVoicePreviewRequested,
                    onSetTurnDetection = viewModel::setTurnDetection,
                    onSetVoiceEngine = viewModel::setVoiceEngine,
                    onSetGeminiVoice = viewModel::setGeminiVoice,
                )
            }
            item(key = "divider_conversation") { HorizontalDivider(Modifier.padding(vertical = 8.dp)) }

            item(key = "audio") {
                AudioSection(
                    micDeviceId = doc.micDeviceId,
                    micDevices = state.micDevices,
                    onSetMicDevice = viewModel::setMicDevice,
                    onExpandMicDevices = viewModel::refreshMicDevices,
                )
            }
            item(key = "divider_audio") { HorizontalDivider(Modifier.padding(vertical = 8.dp)) }

            item(key = "voice_screen") {
                VoiceScreenSection(
                    lockedSessions = doc.lockedSessions,
                    wakeScreenOnWake = doc.wakeScreenOnWake,
                    keepScreenOn = doc.keepScreenOn,
                    batteryOptimizationIgnored = state.batteryOptimizationIgnored,
                    onSetLockedSessions = viewModel::setLockedSessions,
                    onSetWakeScreenOnWake = viewModel::setWakeScreenOnWake,
                    onSetKeepScreenOn = viewModel::setKeepScreenOn,
                    onExempt = { batteryLauncher.launch(viewModel.batteryExemptionIntent()) },
                    onRecheck = viewModel::refreshBatteryStatus,
                    onOpenAppInfo = {
                        val intent = Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS)
                            .setData(Uri.fromParts("package", context.packageName, null))
                        appInfoLauncher.launch(intent)
                    },
                )
            }
            item(key = "divider_voice_screen") { HorizontalDivider(Modifier.padding(vertical = 8.dp)) }

            item(key = "appearance") {
                AppearanceSection(
                    appStyle = doc.appStyle,
                    theme = doc.theme,
                    onSetAppStyle = viewModel::setAppStyle,
                    onSetTheme = viewModel::setTheme,
                )
            }
            item(key = "divider_appearance") { HorizontalDivider(Modifier.padding(vertical = 8.dp)) }

            item(key = "privacy") {
                PrivacySection(
                    storeTranscripts = doc.storeTranscripts,
                    storeAudio = doc.storeAudio,
                    retentionDays = doc.retentionDays,
                    onSetStoreTranscripts = viewModel::setStoreTranscripts,
                    onSetStoreAudio = viewModel::setStoreAudio,
                    onSetRetentionDays = viewModel::setRetentionDays,
                )
            }
            item(key = "divider_privacy") { HorizontalDivider(Modifier.padding(vertical = 8.dp)) }

            item(key = "diagnostics") {
                DiagnosticsSection(
                    diagnostics = doc.diagnostics,
                    onSetDiagnosticsEnabled = viewModel::setDiagnosticsEnabled,
                    onSetDiagnosticsMinLevel = viewModel::setDiagnosticsMinLevel,
                    onSetDiagnosticsCategory = viewModel::setDiagnosticsCategory,
                    onSetAllDiagnosticsCategories = viewModel::setAllDiagnosticsCategories,
                    onOpenLogViewer = onOpenLogViewer,
                    onExportLogs = viewModel::exportLogs,
                    onClearLogs = viewModel::clearLogs,
                    snackbarHostState = snackbarHostState,
                )
            }
            item(key = "divider_diagnostics") { HorizontalDivider(Modifier.padding(vertical = 8.dp)) }

            item(key = "account") {
                AccountSection(
                    signedIn = state.signedIn,
                    accountActionsAvailable = state.accountActionsAvailable,
                    signOutInProgress = state.signOutInProgress,
                    onSignOut = viewModel::signOut,
                    onSignOutEverywhere = viewModel::signOutEverywhere,
                )
            }

            item(key = "version") {
                Text(
                    stringResource(R.string.settings_version_caption, doc.version),
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 8.dp, bottom = 24.dp),
                )
            }
        }
    }
}

/**
 * Wake-word group: the always-listening switch (bound to the service's own
 * observable running state, never a local mirror), the wake-word combobox,
 * the engine radio group, the sensitivity slider, and the custom wake-phrase
 * training form + status card (M6 FR-K03).
 */
@Composable
private fun WakeSection(
    wakeServiceEnabled: Boolean,
    wakeServiceRunning: Boolean,
    wakeWord: String,
    wakeOptions: List<WakeWordOption>,
    wakeCatalogOffline: Boolean,
    wakeEngine: String,
    porcupineAvailable: Boolean,
    sensitivity: Float,
    activeWakeWordId: String,
    customPhrase: String,
    customPhraseValid: Boolean,
    customRequestInProgress: Boolean,
    customJob: CustomWakeJob?,
    onSetWakeWord: (String) -> Unit,
    onSetWakeEngine: (String) -> Unit,
    onSetSensitivity: (Float) -> Unit,
    onSetCustomPhrase: (String) -> Unit,
    onRequestCustomWakeWord: () -> Unit,
    onUseCustomWakeWord: () -> Unit,
    onClearCustomJob: () -> Unit,
) {
    SectionHeader(stringResource(R.string.settings_section_wake))

    // Always-listening switch. This is the ONLY entry point that starts the wake
    // service for the first time: WakeBootReceiver and MainActivity's tap-to-resume
    // path are both gated on WakePreferences.serviceEnabled, and that flag is only
    // ever set from inside the service — so without this control a fresh install
    // could never reach a running wake service at all.
    val context = LocalContext.current
    val micGranted = ContextCompat.checkSelfPermission(
        context,
        Manifest.permission.RECORD_AUDIO,
    ) == PackageManager.PERMISSION_GRANTED
    val micLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted -> if (granted) WakeWordService.start(context) }
    // WS-5 M21.4: the switch shows whether listening is actually happening, not
    // just whether it was requested. After a reboot those differ — the flag stays
    // true while nothing is listening — and a switch that reads ON while the
    // assistant is deaf is worse than one that reads OFF. The decision itself lives
    // in wake/WakeSwitchState.kt so that WakeSwitchStateTest guards the shipped
    // logic rather than a copy of it — do not re-inline it here.
    val switchDisplay = wakeSwitchDisplay(
        serviceEnabled = wakeServiceEnabled,
        serviceRunning = wakeServiceRunning,
    )
    val wakePaused = switchDisplay == WakeSwitchDisplay.PAUSED
    val startListening: () -> Unit = {
        if (micGranted) WakeWordService.start(context)
        else micLauncher.launch(Manifest.permission.RECORD_AUDIO)
    }
    LabeledSwitchRow(
        label = stringResource(R.string.settings_wake_service_label),
        description = stringResource(R.string.settings_wake_service_desc),
        checked = switchDisplay == WakeSwitchDisplay.RUNNING,
        onCheckedChange = { enable ->
            // In the paused state the switch already reads OFF, so a tap means
            // "resume", never "stop" — and a start is never gated on the persisted
            // serviceEnabled flag (that gate was the M21.0 fresh-install deadlock).
            val action = decideWakeSwitchAction(
                toggledOn = enable,
                serviceEnabled = wakeServiceEnabled,
                serviceRunning = wakeServiceRunning,
            )
            when (action) {
                WakeSwitchAction.START -> startListening()
                WakeSwitchAction.STOP -> WakeWordService.stop(context)
            }
        },
    )
    if (wakePaused) {
        Text(
            stringResource(R.string.settings_wake_service_paused),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.error,
        )
        OutlinedButton(onClick = startListening) {
            Text(stringResource(R.string.settings_wake_service_resume))
        }
    }
    if (!micGranted) {
        Text(
            stringResource(R.string.settings_wake_service_needs_mic),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }

    // Wake-word combobox, populated from the shared catalog (never free text).
    var wakeExpanded by remember { mutableStateOf(false) }
    val selectedWake = wakeOptions.firstOrNull { it.id == wakeWord }
    ExposedDropdownMenuBox(
        expanded = wakeExpanded,
        onExpandedChange = { wakeExpanded = it },
    ) {
        OutlinedTextField(
            value = selectedWake?.label ?: wakeWord,
            onValueChange = {},
            readOnly = true,
            label = { Text(stringResource(R.string.settings_wake_word_label)) },
            supportingText = {
                if (wakeCatalogOffline) {
                    Text(stringResource(R.string.settings_wake_catalog_offline))
                }
            },
            trailingIcon = {
                ExposedDropdownMenuDefaults.TrailingIcon(expanded = wakeExpanded)
            },
            modifier = Modifier
                .fillMaxWidth()
                .menuAnchor(MenuAnchorType.PrimaryNotEditable),
        )
        ExposedDropdownMenu(
            expanded = wakeExpanded,
            onDismissRequest = { wakeExpanded = false },
        ) {
            wakeOptions.forEach { option ->
                DropdownMenuItem(
                    text = {
                        Column {
                            Text(option.label)
                            if (option.description.isNotBlank()) {
                                Text(
                                    option.description,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            }
                        }
                    },
                    onClick = {
                        onSetWakeWord(option.id)
                        wakeExpanded = false
                    },
                )
            }
        }
    }

    // Wake engine radio group.
    LabeledRadioGroup(
        label = stringResource(R.string.settings_wake_engine_label),
        options = listOf(
            RadioOption(
                value = "openwakeword",
                label = stringResource(R.string.settings_engine_openwakeword),
                description = stringResource(R.string.settings_engine_openwakeword_desc),
                enabled = true,
            ),
            RadioOption(
                value = "porcupine",
                label = stringResource(R.string.settings_engine_porcupine),
                description = if (porcupineAvailable) {
                    stringResource(R.string.settings_engine_porcupine_desc)
                } else {
                    stringResource(R.string.settings_engine_porcupine_unavailable)
                },
                enabled = porcupineAvailable,
            ),
        ),
        selected = wakeEngine,
        onSelect = onSetWakeEngine,
    )

    // Sensitivity slider (bounded control — never a text field).
    val sensitivityPercent = (sensitivity * 100).roundToInt()
    Text(
        stringResource(R.string.settings_sensitivity_label, sensitivityPercent),
        style = MaterialTheme.typography.bodyMedium,
    )
    Slider(
        value = sensitivity,
        onValueChange = onSetSensitivity,
        valueRange = 0f..1f,
        modifier = Modifier
            .fillMaxWidth()
            .semantics {
                contentDescription = "Wake word sensitivity"
                stateDescription = "$sensitivityPercent percent"
            },
    )
    // WS-5 M21.3: never let the picker imply a phrase works when no model
    // backs it. activeWakeWordId comes from the loaded head model. The comparison
    // lives in ui/settings/WakePhraseResolution.kt so WakePhraseResolutionTest guards
    // this exact behaviour — do not re-inline it here.
    val phrase = resolveWakePhrase(selectedId = wakeWord, activeId = activeWakeWordId)
    if (phrase.mismatched) {
        Text(
            stringResource(
                R.string.settings_wake_phrase_unavailable,
                wakeOptions.firstOrNull { it.id == phrase.activeId }?.label
                    ?: phrase.activeId,
            ),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.error,
        )
    }

    Text(
        stringResource(R.string.settings_sensitivity_hint),
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )

    // ---------- Custom wake phrase (M6 FR-K03) ----------
    // The phrase field is genuinely free text (a novel phrase the user
    // invents) — the training pipeline turns it into a catalog entry
    // that then appears in the combobox above. Training is server-side
    // (openWakeWord on AWS Batch); status is polled + emailed.
    Text(
        stringResource(R.string.settings_custom_wake_title),
        style = MaterialTheme.typography.titleSmall,
        modifier = Modifier.padding(top = 8.dp),
    )
    Text(
        stringResource(R.string.settings_custom_wake_body),
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
    OutlinedTextField(
        value = customPhrase,
        onValueChange = onSetCustomPhrase,
        label = { Text(stringResource(R.string.settings_custom_wake_label)) },
        supportingText = {
            Text(
                if (customPhrase.isNotBlank() && !customPhraseValid) {
                    stringResource(R.string.settings_custom_wake_invalid)
                } else {
                    stringResource(R.string.settings_custom_wake_hint)
                },
            )
        },
        singleLine = true,
        enabled = !customRequestInProgress,
        modifier = Modifier.fillMaxWidth(),
    )
    Button(
        onClick = onRequestCustomWakeWord,
        enabled = customPhraseValid && !customRequestInProgress,
        modifier = Modifier.heightIn(min = 48.dp),
    ) { Text(stringResource(R.string.settings_custom_wake_submit)) }

    customJob?.let { job ->
        CustomWakeJobCard(job = job, onDismiss = onClearCustomJob, onUse = onUseCustomWakeWord)
    }
}

/** Status card for an in-flight or ready custom wake-word training job (M6 FR-K03). */
@Composable
private fun CustomWakeJobCard(
    job: CustomWakeJob,
    onDismiss: () -> Unit,
    onUse: () -> Unit,
) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp)) {
            Text(
                "“${job.phrase}”",
                style = MaterialTheme.typography.bodyLarge,
            )
            val statusText = when (job.status) {
                "pending" -> stringResource(R.string.settings_custom_wake_status_pending)
                "training" -> stringResource(R.string.settings_custom_wake_status_training)
                "ready" -> stringResource(R.string.settings_custom_wake_status_ready)
                "failed" -> stringResource(R.string.settings_custom_wake_status_failed)
                else -> job.status
            }
            Row(verticalAlignment = Alignment.CenterVertically) {
                if (job.inFlight) {
                    CircularProgressIndicator(
                        modifier = Modifier
                            .padding(end = 8.dp)
                            .size(16.dp),
                        strokeWidth = 2.dp,
                    )
                }
                Text(
                    statusText,
                    style = MaterialTheme.typography.bodyMedium,
                    color = if (job.status == "failed") {
                        MaterialTheme.colorScheme.error
                    } else {
                        MaterialTheme.colorScheme.onSurfaceVariant
                    },
                )
            }
            job.error?.takeIf { it.isNotBlank() }?.let { message ->
                Text(
                    message,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.error,
                    modifier = Modifier.padding(top = 4.dp),
                )
            }
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(top = 8.dp),
                horizontalArrangement = Arrangement.End,
            ) {
                TextButton(
                    onClick = onDismiss,
                    modifier = Modifier.heightIn(min = 48.dp),
                ) { Text(stringResource(R.string.settings_custom_wake_dismiss)) }
                if (job.ready) {
                    Button(
                        onClick = onUse,
                        modifier = Modifier
                            .padding(start = 8.dp)
                            .heightIn(min = 48.dp),
                    ) { Text(stringResource(R.string.settings_custom_wake_use)) }
                }
            }
        }
    }
}

/**
 * Conversation group: persona picker (+ progressive-disclosure custom system
 * instructions), voice radio group, turn-detection radio, voice-engine
 * picker, and the Gemini voice picker revealed only for that engine.
 */
@Composable
private fun ConversationSection(
    personaPresetId: String,
    personaSystemInstructions: String?,
    personaPresets: List<PersonaPreset>,
    displayVoice: String,
    turnDetection: String,
    voiceEngineDefault: String,
    geminiVoice: String,
    geminiVoices: List<GeminiVoiceOption>,
    onSetPersona: (String) -> Unit,
    onSetCustomInstructions: (String) -> Unit,
    onSetVoice: (String) -> Unit,
    onVoicePreviewRequested: () -> Unit,
    onSetTurnDetection: (String) -> Unit,
    onSetVoiceEngine: (String) -> Unit,
    onSetGeminiVoice: (String) -> Unit,
) {
    SectionHeader(stringResource(R.string.settings_section_conversation))

    // Persona select (IDs only; server resolves instructions).
    var personaExpanded by remember { mutableStateOf(false) }
    val selectedPersona =
        personaPresets.firstOrNull { it.id == personaPresetId } ?: personaPresets.first()
    ExposedDropdownMenuBox(
        expanded = personaExpanded,
        onExpandedChange = { personaExpanded = it },
    ) {
        OutlinedTextField(
            value = selectedPersona.label,
            onValueChange = {},
            readOnly = true,
            label = { Text(stringResource(R.string.settings_persona_label)) },
            supportingText = { Text(selectedPersona.description) },
            trailingIcon = {
                ExposedDropdownMenuDefaults.TrailingIcon(expanded = personaExpanded)
            },
            modifier = Modifier
                .fillMaxWidth()
                .menuAnchor(MenuAnchorType.PrimaryNotEditable),
        )
        ExposedDropdownMenu(
            expanded = personaExpanded,
            onDismissRequest = { personaExpanded = false },
        ) {
            personaPresets.forEach { preset ->
                DropdownMenuItem(
                    text = {
                        Column {
                            Text(preset.label)
                            Text(
                                preset.description,
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                    },
                    onClick = {
                        onSetPersona(preset.id)
                        personaExpanded = false
                    },
                )
            }
        }
    }

    // Custom system instructions — the schema's sole justified free-text
    // field, revealed only when persona == custom (progressive disclosure).
    if (personaPresetId == "custom") {
        val instructions = personaSystemInstructions.orEmpty()
        OutlinedTextField(
            value = instructions,
            onValueChange = onSetCustomInstructions,
            label = { Text(stringResource(R.string.settings_custom_instructions_label)) },
            supportingText = {
                Text(
                    stringResource(
                        R.string.settings_custom_instructions_counter,
                        instructions.length,
                        SettingsViewModel.CUSTOM_INSTRUCTIONS_MAX,
                    ),
                )
            },
            minLines = 3,
            modifier = Modifier.fillMaxWidth(),
        )
    }

    // Voice radio group with per-voice preview affordance.
    Text(
        stringResource(R.string.settings_voice_label),
        style = MaterialTheme.typography.bodyMedium,
    )
    Column(Modifier.selectableGroup()) {
        SettingsDocument.VOICES.forEach { voice ->
            val selected = displayVoice == voice
            val voiceLabel = voice.replaceFirstChar { it.uppercaseChar() }
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = 48.dp)
                    .selectable(
                        selected = selected,
                        role = Role.RadioButton,
                        onClick = { onSetVoice(voice) },
                    ),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                RadioButton(selected = selected, onClick = null)
                Text(
                    voiceLabel,
                    style = MaterialTheme.typography.bodyLarge,
                    modifier = Modifier.padding(start = 8.dp),
                )
                if (voice == SettingsDocument.DEFAULT_VOICE) {
                    Text(
                        stringResource(R.string.settings_voice_default_badge),
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.primary,
                        modifier = Modifier.padding(start = 8.dp),
                    )
                }
                Spacer(Modifier.weight(1f))
                // Preview: rendered in the disabled style but still
                // focusable/tappable so it explains itself when touched
                // (tooltip-equivalent that TalkBack users can reach).
                // Becomes a real player once bundled samples/backend TTS
                // preview exist (no samples ship today).
                IconButton(
                    onClick = onVoicePreviewRequested,
                    modifier = Modifier
                        .size(48.dp)
                        .alpha(0.38f)
                        .semantics {
                            contentDescription =
                                "Preview voice $voiceLabel. Not available yet — " +
                                    "previews arrive with the backend voice preview service."
                        },
                ) {
                    Icon(Icons.Filled.PlayCircleOutline, contentDescription = null)
                }
            }
        }
    }

    // Turn detection radio.
    LabeledRadioGroup(
        label = stringResource(R.string.settings_turn_detection_label),
        options = listOf(
            RadioOption(
                value = "semantic_vad",
                label = stringResource(R.string.settings_turn_semantic),
                description = stringResource(R.string.settings_turn_semantic_desc),
                enabled = true,
            ),
            RadioOption(
                value = "server_vad",
                label = stringResource(R.string.settings_turn_server),
                description = stringResource(R.string.settings_turn_server_desc),
                enabled = true,
            ),
        ),
        selected = turnDetection,
        onSelect = onSetTurnDetection,
    )

    // Voice engine picker (M12 FR-VE-04). Sets voiceEngine.default —
    // the engine this device uses; all engines share tools, memory,
    // and transcripts, differing only in cost, latency, and quality.
    LabeledRadioGroup(
        label = stringResource(R.string.settings_voice_engine_label),
        options = listOf(
            RadioOption(
                value = "openai-realtime",
                label = stringResource(R.string.settings_engine_openai),
                description = stringResource(R.string.settings_engine_openai_desc),
                enabled = true,
            ),
            RadioOption(
                value = "openai-realtime-mini",
                label = stringResource(R.string.settings_engine_openai_mini),
                description = stringResource(R.string.settings_engine_openai_mini_desc),
                enabled = true,
            ),
            RadioOption(
                value = "nova-sonic",
                label = stringResource(R.string.settings_engine_nova),
                description = stringResource(R.string.settings_engine_nova_desc),
                enabled = true,
            ),
            RadioOption(
                value = SettingsViewModel.GEMINI_ENGINE,
                label = stringResource(R.string.settings_engine_gemini),
                description = stringResource(R.string.settings_engine_gemini_desc),
                enabled = true,
            ),
        ),
        selected = voiceEngineDefault,
        onSelect = onSetVoiceEngine,
    )

    // Gemini voice picker (M13, D4) — progressive disclosure: only
    // when the Gemini engine is selected. Populated from the
    // `geminiVoices` catalog (GET /api/v1/realtime/voices); writes the
    // top-level `geminiVoice` settings key.
    if (voiceEngineDefault == SettingsViewModel.GEMINI_ENGINE) {
        var geminiVoiceExpanded by remember { mutableStateOf(false) }
        val selectedGeminiVoice = geminiVoice.ifEmpty { SettingsDocument.DEFAULT_GEMINI_VOICE }
        val selectedGeminiOption = geminiVoices.firstOrNull { it.id == selectedGeminiVoice }
        ExposedDropdownMenuBox(
            expanded = geminiVoiceExpanded,
            onExpandedChange = { geminiVoiceExpanded = it },
        ) {
            OutlinedTextField(
                value = selectedGeminiOption?.label ?: selectedGeminiVoice,
                onValueChange = {},
                readOnly = true,
                label = { Text(stringResource(R.string.settings_gemini_voice_label)) },
                supportingText = {
                    Text(
                        if (geminiVoices.isEmpty()) {
                            stringResource(R.string.settings_gemini_voice_offline)
                        } else {
                            selectedGeminiOption?.description.orEmpty()
                        },
                    )
                },
                trailingIcon = {
                    ExposedDropdownMenuDefaults.TrailingIcon(expanded = geminiVoiceExpanded)
                },
                modifier = Modifier
                    .fillMaxWidth()
                    .menuAnchor(MenuAnchorType.PrimaryNotEditable),
            )
            ExposedDropdownMenu(
                expanded = geminiVoiceExpanded,
                onDismissRequest = { geminiVoiceExpanded = false },
            ) {
                geminiVoices.forEach { option ->
                    DropdownMenuItem(
                        text = {
                            Column {
                                Row(verticalAlignment = Alignment.CenterVertically) {
                                    Text(option.label)
                                    if (option.default) {
                                        Text(
                                            stringResource(R.string.settings_voice_default_badge),
                                            style = MaterialTheme.typography.labelSmall,
                                            color = MaterialTheme.colorScheme.primary,
                                            modifier = Modifier.padding(start = 8.dp),
                                        )
                                    }
                                }
                                if (option.description.isNotBlank()) {
                                    Text(
                                        option.description,
                                        style = MaterialTheme.typography.bodySmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    )
                                }
                            }
                        },
                        onClick = {
                            onSetGeminiVoice(option.id)
                            geminiVoiceExpanded = false
                        },
                    )
                }
            }
        }
    }
}

/** Audio group: input-device combobox (populated from [MicDeviceOption], never typed). */
@Composable
private fun AudioSection(
    micDeviceId: String?,
    micDevices: List<MicDeviceOption>,
    onSetMicDevice: (String?) -> Unit,
    onExpandMicDevices: () -> Unit,
) {
    SectionHeader(stringResource(R.string.settings_section_audio))
    var micExpanded by remember { mutableStateOf(false) }
    val selectedMic = micDevices.firstOrNull { it.id == micDeviceId } ?: micDevices.firstOrNull()
    ExposedDropdownMenuBox(
        expanded = micExpanded,
        onExpandedChange = {
            if (it) onExpandMicDevices()
            micExpanded = it
        },
    ) {
        OutlinedTextField(
            value = selectedMic?.label ?: stringResource(R.string.settings_mic_system_default),
            onValueChange = {},
            readOnly = true,
            label = { Text(stringResource(R.string.settings_mic_label)) },
            trailingIcon = {
                ExposedDropdownMenuDefaults.TrailingIcon(expanded = micExpanded)
            },
            modifier = Modifier
                .fillMaxWidth()
                .menuAnchor(MenuAnchorType.PrimaryNotEditable),
        )
        ExposedDropdownMenu(
            expanded = micExpanded,
            onDismissRequest = { micExpanded = false },
        ) {
            micDevices.forEach { device ->
                DropdownMenuItem(
                    text = { Text(device.label) },
                    onClick = {
                        onSetMicDevice(device.id)
                        micExpanded = false
                    },
                )
            }
        }
    }
}

/**
 * Voice & Screen group (01-platform §B-iv): lifecycle switches plus the
 * battery-optimization health card and per-OEM guidance card (01-platform
 * §C / M8.4).
 */
@Composable
private fun VoiceScreenSection(
    lockedSessions: Boolean,
    wakeScreenOnWake: Boolean,
    keepScreenOn: Boolean,
    batteryOptimizationIgnored: Boolean,
    onSetLockedSessions: (Boolean) -> Unit,
    onSetWakeScreenOnWake: (Boolean) -> Unit,
    onSetKeepScreenOn: (Boolean) -> Unit,
    onExempt: () -> Unit,
    onRecheck: () -> Unit,
    onOpenAppInfo: () -> Unit,
) {
    SectionHeader(stringResource(R.string.settings_section_voice_screen))
    LabeledSwitchRow(
        label = stringResource(R.string.settings_locked_sessions_label),
        description = stringResource(R.string.settings_locked_sessions_desc),
        checked = lockedSessions,
        onCheckedChange = onSetLockedSessions,
    )
    LabeledSwitchRow(
        label = stringResource(R.string.settings_wake_screen_label),
        description = stringResource(R.string.settings_wake_screen_desc),
        checked = wakeScreenOnWake,
        onCheckedChange = onSetWakeScreenOnWake,
    )
    LabeledSwitchRow(
        label = stringResource(R.string.settings_keep_screen_on_label),
        description = stringResource(R.string.settings_keep_screen_on_desc),
        checked = keepScreenOn,
        onCheckedChange = onSetKeepScreenOn,
    )

    // Battery-optimization health card + action row (01-platform §C).
    BatteryHealthCard(
        ignored = batteryOptimizationIgnored,
        onExempt = onExempt,
        onRecheck = onRecheck,
    )

    // Per-OEM guidance (M8.4): OEM battery/sleep layers on top of Android's
    // own Doze exemption above — Samsung (the owner's phone) gets concrete
    // steps, every other manufacturer gets a generic pointer at the card above.
    OemGuidanceCard(onOpenAppInfo = onOpenAppInfo)
}

/**
 * Appearance group (03-theme): visual style radio (M8.1/M8.2) + light/dark/
 * system segmented row, grayed out (never hidden) while HAL pins dark.
 */
@Composable
private fun AppearanceSection(
    appStyle: String,
    theme: String,
    onSetAppStyle: (String) -> Unit,
    onSetTheme: (String) -> Unit,
) {
    SectionHeader(stringResource(R.string.settings_section_appearance))
    val isHal = appStyle == "hal9000"
    // 4 options -> radio group (UI standard: 2-5 mutually-exclusive
    // options worth seeing at once). Each style ports its own
    // ninja/minimal/terminal/HAL token set from web/static/css/app.css
    // (M8.1); HAL always overrides the light/dark/system control below.
    LabeledRadioGroup(
        label = stringResource(R.string.settings_style_label),
        options = listOf(
            RadioOption(
                value = "hal9000",
                label = stringResource(R.string.settings_style_hal9000),
                description = stringResource(R.string.settings_style_hal9000_desc),
                enabled = true,
            ),
            RadioOption(
                value = "ninja",
                label = stringResource(R.string.settings_style_ninja),
                description = stringResource(R.string.settings_style_ninja_desc),
                enabled = true,
            ),
            RadioOption(
                value = "minimal",
                label = stringResource(R.string.settings_style_minimal),
                description = stringResource(R.string.settings_style_minimal_desc),
                enabled = true,
            ),
            RadioOption(
                value = "terminal",
                label = stringResource(R.string.settings_style_terminal),
                description = stringResource(R.string.settings_style_terminal_desc),
                enabled = true,
            ),
        ),
        selected = appStyle,
        onSelect = onSetAppStyle,
    )

    Spacer(Modifier.height(8.dp))
    Text(stringResource(R.string.settings_theme_label), style = MaterialTheme.typography.bodyMedium)
    val themeChoices = listOf(
        "light" to stringResource(R.string.settings_theme_light),
        "dark" to stringResource(R.string.settings_theme_dark),
        "system" to stringResource(R.string.settings_theme_system),
    )
    // Grayed out (disabled, not hidden) while HAL is selected — HAL pins
    // dark regardless of this setting; the caption below explains why.
    SingleChoiceSegmentedButtonRow(
        modifier = Modifier
            .fillMaxWidth()
            .alpha(if (isHal) 0.5f else 1f),
    ) {
        themeChoices.forEachIndexed { index, (value, label) ->
            SegmentedButton(
                selected = theme == value,
                onClick = { onSetTheme(value) },
                enabled = !isHal,
                shape = SegmentedButtonDefaults.itemShape(
                    index = index,
                    count = themeChoices.size,
                ),
                modifier = Modifier.heightIn(min = 44.dp),
            ) { Text(label) }
        }
    }
    if (isHal) {
        Text(
            stringResource(R.string.settings_theme_hal_note),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

/** Privacy group: storage switches + retention radio group. */
@Composable
private fun PrivacySection(
    storeTranscripts: Boolean,
    storeAudio: Boolean,
    retentionDays: Int,
    onSetStoreTranscripts: (Boolean) -> Unit,
    onSetStoreAudio: (Boolean) -> Unit,
    onSetRetentionDays: (Int) -> Unit,
) {
    SectionHeader(stringResource(R.string.settings_section_privacy))
    LabeledSwitchRow(
        label = stringResource(R.string.settings_store_transcripts_label),
        description = stringResource(R.string.settings_store_transcripts_desc),
        checked = storeTranscripts,
        onCheckedChange = onSetStoreTranscripts,
    )
    LabeledSwitchRow(
        label = stringResource(R.string.settings_store_audio_label),
        description = stringResource(R.string.settings_store_audio_desc),
        checked = storeAudio,
        onCheckedChange = onSetStoreAudio,
    )
    LabeledRadioGroup(
        label = stringResource(R.string.settings_retention_label),
        options = SettingsDocument.RETENTION_CHOICES.map { days ->
            RadioOption(
                value = days.toString(),
                label = when (days) {
                    0 -> stringResource(R.string.settings_retention_none)
                    else -> stringResource(R.string.settings_retention_days, days)
                },
                description = null,
                enabled = true,
            )
        },
        selected = retentionDays.toString(),
        onSelect = { onSetRetentionDays(it.toInt()) },
    )
}

/**
 * Diagnostics group (04-logging §A5): master switch, severity-floor radio,
 * the eight category checkboxes with select-all/none, and the view/export/
 * clear log actions. Export-in-progress and the clear-confirmation dialog
 * are local to this section — nothing outside Diagnostics reads them.
 */
@Composable
private fun DiagnosticsSection(
    diagnostics: DiagnosticsConfig,
    onSetDiagnosticsEnabled: (Boolean) -> Unit,
    onSetDiagnosticsMinLevel: (String) -> Unit,
    onSetDiagnosticsCategory: (String, Boolean) -> Unit,
    onSetAllDiagnosticsCategories: (Boolean) -> Unit,
    onOpenLogViewer: () -> Unit,
    onExportLogs: suspend () -> Intent?,
    onClearLogs: () -> Unit,
    snackbarHostState: SnackbarHostState,
) {
    val context = LocalContext.current
    val coroutineScope = rememberCoroutineScope()
    var exportingLogs by remember { mutableStateOf(false) }
    var confirmClearLogs by remember { mutableStateOf(false) }

    SectionHeader(stringResource(R.string.settings_section_diagnostics))
    LabeledSwitchRow(
        label = stringResource(R.string.settings_diagnostics_master_label),
        description = stringResource(R.string.settings_diagnostics_master_desc),
        checked = diagnostics.enabled,
        onCheckedChange = onSetDiagnosticsEnabled,
    )
    // Everything below only affects capture while logging is enabled.
    if (diagnostics.enabled) {
        LabeledRadioGroup(
            label = stringResource(R.string.settings_diagnostics_level_label),
            options = listOf(
                RadioOption("VERBOSE", stringResource(R.string.settings_diagnostics_level_verbose), null, true),
                RadioOption("DEBUG", stringResource(R.string.settings_diagnostics_level_debug), null, true),
                RadioOption("INFO", stringResource(R.string.settings_diagnostics_level_info), null, true),
                RadioOption("WARN", stringResource(R.string.settings_diagnostics_level_warn), null, true),
                RadioOption("ERROR", stringResource(R.string.settings_diagnostics_level_error), null, true),
            ),
            selected = diagnostics.minLevel,
            onSelect = onSetDiagnosticsMinLevel,
        )

        // Category checkbox group (8) with select all / none.
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .padding(top = 8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(
                stringResource(R.string.settings_diagnostics_categories_label),
                style = MaterialTheme.typography.bodyMedium,
                modifier = Modifier.weight(1f),
            )
            TextButton(
                onClick = { onSetAllDiagnosticsCategories(true) },
                modifier = Modifier.heightIn(min = 48.dp),
            ) { Text(stringResource(R.string.settings_diagnostics_select_all)) }
            TextButton(
                onClick = { onSetAllDiagnosticsCategories(false) },
                modifier = Modifier.heightIn(min = 48.dp),
            ) { Text(stringResource(R.string.settings_diagnostics_select_none)) }
        }
        DiagnosticsCategories(
            categories = diagnostics.categories,
            onToggle = onSetDiagnosticsCategory,
        )
    }

    // View logs (internal route), Export, Clear — available regardless of
    // capture toggle so the user can always inspect/export/clear history.
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .heightIn(min = 48.dp)
            .clickable(onClick = onOpenLogViewer)
            .padding(vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(
            Icons.Outlined.Description,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.primary,
        )
        Text(
            stringResource(R.string.settings_diagnostics_view),
            style = MaterialTheme.typography.bodyLarge,
            modifier = Modifier
                .weight(1f)
                .padding(start = 12.dp),
        )
        Icon(
            Icons.AutoMirrored.Outlined.KeyboardArrowRight,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
    Row(
        modifier = Modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        OutlinedButton(
            onClick = {
                if (!exportingLogs) {
                    exportingLogs = true
                    coroutineScope.launch {
                        val intent = onExportLogs()
                        exportingLogs = false
                        if (intent != null) {
                            context.startActivity(
                                Intent.createChooser(
                                    intent,
                                    context.getString(R.string.settings_diagnostics_share_title),
                                ),
                            )
                        } else {
                            snackbarHostState.showSnackbar(
                                context.getString(R.string.settings_diagnostics_export_empty),
                            )
                        }
                    }
                }
            },
            enabled = !exportingLogs,
            modifier = Modifier
                .weight(1f)
                .heightIn(min = 48.dp),
        ) {
            if (exportingLogs) {
                CircularProgressIndicator(
                    modifier = Modifier.size(18.dp),
                    strokeWidth = 2.dp,
                )
            } else {
                Text(stringResource(R.string.settings_diagnostics_export))
            }
        }
        OutlinedButton(
            onClick = { confirmClearLogs = true },
            modifier = Modifier
                .weight(1f)
                .heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.settings_diagnostics_clear)) }
    }

    if (confirmClearLogs) {
        ConfirmDialog(
            title = stringResource(R.string.settings_diagnostics_clear_confirm_title),
            body = stringResource(R.string.settings_diagnostics_clear_confirm_body),
            confirmLabel = stringResource(R.string.settings_diagnostics_clear),
            onConfirm = {
                confirmClearLogs = false
                onClearLogs()
            },
            onDismiss = { confirmClearLogs = false },
        )
    }
}

/**
 * Account group: signed-in state, sign-out / sign-out-everywhere actions
 * (each behind its own confirmation, local to this section).
 */
@Composable
private fun AccountSection(
    signedIn: Boolean,
    accountActionsAvailable: Boolean,
    signOutInProgress: Boolean,
    onSignOut: () -> Unit,
    onSignOutEverywhere: () -> Unit,
) {
    var confirmSignOut by remember { mutableStateOf(false) }
    var confirmSignOutEverywhere by remember { mutableStateOf(false) }

    SectionHeader(stringResource(R.string.settings_section_account))
    Text(
        if (signedIn) {
            stringResource(R.string.settings_account_signed_in)
        } else {
            stringResource(R.string.settings_account_signed_out)
        },
        style = MaterialTheme.typography.bodyMedium,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
    OutlinedButton(
        onClick = { confirmSignOut = true },
        enabled = accountActionsAvailable && !signOutInProgress,
        modifier = Modifier
            .fillMaxWidth()
            .heightIn(min = 48.dp),
    ) { Text(stringResource(R.string.settings_sign_out)) }
    TextButton(
        onClick = { confirmSignOutEverywhere = true },
        enabled = accountActionsAvailable && !signOutInProgress,
        modifier = Modifier
            .fillMaxWidth()
            .heightIn(min = 48.dp),
    ) { Text(stringResource(R.string.settings_sign_out_everywhere)) }
    if (!accountActionsAvailable) {
        Text(
            stringResource(R.string.settings_account_unavailable),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }

    if (confirmSignOut) {
        ConfirmDialog(
            title = stringResource(R.string.settings_sign_out_confirm_title),
            body = stringResource(R.string.settings_sign_out_confirm_body),
            confirmLabel = stringResource(R.string.settings_sign_out),
            onConfirm = {
                confirmSignOut = false
                onSignOut()
            },
            onDismiss = { confirmSignOut = false },
        )
    }
    if (confirmSignOutEverywhere) {
        ConfirmDialog(
            title = stringResource(R.string.settings_sign_out_everywhere_confirm_title),
            body = stringResource(R.string.settings_sign_out_everywhere_confirm_body),
            confirmLabel = stringResource(R.string.settings_sign_out_everywhere),
            onConfirm = {
                confirmSignOutEverywhere = false
                onSignOutEverywhere()
            },
            onDismiss = { confirmSignOutEverywhere = false },
        )
    }
}

/**
 * Battery-optimization health card (01-platform §C). Signals state with an
 * explicit label + an icon + a non-red status color (warn amber / success
 * green), never color alone — HAL red is reserved for decoration (§D).
 */
@Composable
private fun BatteryHealthCard(
    ignored: Boolean,
    onExempt: () -> Unit,
    onRecheck: () -> Unit,
) {
    val colors = LocalLiveNinjaColors.current
    Card(modifier = Modifier.fillMaxWidth()) {
        Column(
            Modifier.padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Row(
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                Icon(
                    if (ignored) Icons.Outlined.CheckCircle else Icons.Outlined.WarningAmber,
                    contentDescription = null,
                    tint = if (ignored) colors.success else colors.warn,
                )
                Text(
                    stringResource(R.string.settings_battery_title),
                    style = MaterialTheme.typography.titleSmall,
                )
            }
            Text(
                stringResource(
                    if (ignored) R.string.settings_battery_ok else R.string.settings_battery_warn,
                ),
                style = MaterialTheme.typography.bodyMedium,
                color = if (ignored) colors.success else colors.warn,
            )
            if (ignored) {
                OutlinedButton(
                    onClick = onRecheck,
                    modifier = Modifier.heightIn(min = 48.dp),
                ) { Text(stringResource(R.string.settings_battery_recheck)) }
            } else {
                Button(
                    onClick = onExempt,
                    modifier = Modifier
                        .fillMaxWidth()
                        .heightIn(min = 48.dp),
                ) { Text(stringResource(R.string.settings_battery_action)) }
            }
        }
    }
}

/**
 * Per-OEM battery guidance (M8.4): [Build.MANUFACTURER]-gated instructions
 * beyond the Android-standard Doze exemption in [BatteryHealthCard] above —
 * Samsung's One UI (the owner's phone) layers its own "Sleeping apps" /
 * "Never sleeping apps" background-usage limits on top of stock Android's
 * battery optimization, so the exemption alone doesn't guarantee the
 * wake-word FGS survives. Other manufacturers get a generic pointer back at
 * the exemption card instead of invented per-OEM steps this app can't verify.
 */
@Composable
private fun OemGuidanceCard(onOpenAppInfo: () -> Unit) {
    val isSamsung = Build.MANUFACTURER.equals("samsung", ignoreCase = true)
    Card(modifier = Modifier.fillMaxWidth()) {
        Column(
            Modifier.padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Row(
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                Icon(
                    Icons.Outlined.WarningAmber,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.primary,
                )
                Text(
                    stringResource(R.string.settings_oem_title),
                    style = MaterialTheme.typography.titleSmall,
                )
            }
            Text(
                stringResource(
                    if (isSamsung) R.string.settings_oem_samsung_intro else R.string.settings_oem_generic_intro,
                ),
                style = MaterialTheme.typography.bodyMedium,
            )
            if (isSamsung) {
                Text(stringResource(R.string.settings_oem_samsung_step1), style = MaterialTheme.typography.bodyMedium)
                Text(stringResource(R.string.settings_oem_samsung_step2), style = MaterialTheme.typography.bodyMedium)
                Text(stringResource(R.string.settings_oem_samsung_step3), style = MaterialTheme.typography.bodyMedium)
            }
            OutlinedButton(
                onClick = onOpenAppInfo,
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = 48.dp),
            ) { Text(stringResource(R.string.settings_oem_app_info_action)) }
        }
    }
}

/** The eight log-category checkboxes (04-logging §A5), each a real toggleable row. */
@Composable
private fun DiagnosticsCategories(
    categories: Map<String, Boolean>,
    onToggle: (String, Boolean) -> Unit,
) {
    Column(Modifier.fillMaxWidth()) {
        DiagnosticsConfig.CATEGORY_KEYS.forEach { key ->
            val checked = categories[key] ?: true
            val label = stringResource(diagnosticsCategoryLabel(key))
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = 48.dp)
                    .toggleable(
                        value = checked,
                        role = Role.Checkbox,
                        onValueChange = { onToggle(key, it) },
                    ),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Checkbox(checked = checked, onCheckedChange = null)
                Text(
                    label,
                    style = MaterialTheme.typography.bodyLarge,
                    modifier = Modifier.padding(start = 8.dp),
                )
            }
        }
    }
}

@androidx.annotation.StringRes
private fun diagnosticsCategoryLabel(key: String): Int = when (key) {
    "WAKE" -> R.string.settings_diagnostics_cat_wake
    "AUDIO" -> R.string.settings_diagnostics_cat_audio
    "REALTIME" -> R.string.settings_diagnostics_cat_realtime
    "AUTH" -> R.string.settings_diagnostics_cat_auth
    "TOOLS" -> R.string.settings_diagnostics_cat_tools
    "UI" -> R.string.settings_diagnostics_cat_ui
    "NET" -> R.string.settings_diagnostics_cat_net
    else -> R.string.settings_diagnostics_cat_general
}

private data class RadioOption(
    val value: String,
    val label: String,
    val description: String?,
    val enabled: Boolean,
)

@Composable
private fun SectionHeader(text: String) {
    Text(
        text,
        style = MaterialTheme.typography.titleMedium,
        color = MaterialTheme.colorScheme.primary,
        modifier = Modifier.padding(top = 8.dp),
    )
}

@Composable
private fun LabeledRadioGroup(
    label: String,
    options: List<RadioOption>,
    selected: String,
    onSelect: (String) -> Unit,
) {
    Text(label, style = MaterialTheme.typography.bodyMedium)
    Column(Modifier.selectableGroup()) {
        options.forEach { option ->
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = 48.dp)
                    .selectable(
                        selected = option.value == selected,
                        enabled = option.enabled,
                        role = Role.RadioButton,
                        onClick = { onSelect(option.value) },
                    )
                    .alpha(if (option.enabled) 1f else 0.5f),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                RadioButton(
                    selected = option.value == selected,
                    onClick = null,
                    enabled = option.enabled,
                )
                Column(Modifier.padding(start = 8.dp)) {
                    Text(option.label, style = MaterialTheme.typography.bodyLarge)
                    option.description?.let {
                        Text(
                            it,
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
            }
        }
    }
}

@Composable
private fun LabeledSwitchRow(
    label: String,
    description: String,
    checked: Boolean,
    onCheckedChange: (Boolean) -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .heightIn(min = 48.dp)
            .selectable(
                selected = checked,
                role = Role.Switch,
                onClick = { onCheckedChange(!checked) },
            ),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(Modifier.weight(1f)) {
            Text(label, style = MaterialTheme.typography.bodyLarge)
            Text(
                description,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Box(Modifier.width(8.dp))
        Switch(checked = checked, onCheckedChange = null)
    }
}

@Composable
private fun ConfirmDialog(
    title: String,
    body: String,
    confirmLabel: String,
    onConfirm: () -> Unit,
    onDismiss: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = { Text(body) },
        confirmButton = {
            Button(
                onClick = onConfirm,
                modifier = Modifier.heightIn(min = 48.dp),
            ) { Text(confirmLabel) }
        },
        dismissButton = {
            TextButton(
                onClick = onDismiss,
                modifier = Modifier.heightIn(min = 48.dp),
            ) { Text(stringResource(R.string.dialog_cancel)) }
        },
    )
}
