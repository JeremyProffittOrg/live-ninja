@file:OptIn(ExperimentalMaterial3Api::class)

package ninja.jeremy.liveninja.ui.screens

import android.content.Intent
import android.net.Uri
import android.os.Build
import android.provider.Settings
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.verticalScroll
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
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.platform.LocalContext
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
import kotlin.math.roundToInt
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.ui.settings.SettingsNotice
import ninja.jeremy.liveninja.ui.settings.SettingsViewModel
import ninja.jeremy.liveninja.ui.state.DiagnosticsConfig
import ninja.jeremy.liveninja.ui.state.SettingsDocument
import ninja.jeremy.liveninja.ui.theme.LocalLiveNinjaColors

/**
 * Settings tab — schema-driven form over contracts/settings.schema.json
 * (mockups/android/09-settings.html). Every enumerable field is a populated
 * control (combobox/radio/slider/segmented/switch); the single free-text field
 * is the custom-persona system instructions, the schema's one justified case.
 */
@Composable
fun SettingsScreen(
    modifier: Modifier = Modifier,
    onOpenLogViewer: () -> Unit = {},
) {
    val viewModel: SettingsViewModel = hiltViewModel()
    val state by viewModel.state.collectAsState()
    val doc = state.doc
    val snackbarHostState = remember { SnackbarHostState() }
    val context = LocalContext.current
    val coroutineScope = rememberCoroutineScope()

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

    var exportingLogs by remember { mutableStateOf(false) }
    var confirmClearLogs by remember { mutableStateOf(false) }

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

    var confirmSignOut by remember { mutableStateOf(false) }
    var confirmSignOutEverywhere by remember { mutableStateOf(false) }

    Scaffold(
        modifier = modifier.fillMaxSize(),
        snackbarHost = { SnackbarHost(snackbarHostState) },
    ) { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding)
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 16.dp, vertical = 8.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            // ---------- Wake word ----------
            SectionHeader(stringResource(R.string.settings_section_wake))

            // Wake-word combobox, populated from the shared catalog (never free text).
            var wakeExpanded by remember { mutableStateOf(false) }
            val selectedWake = state.wakeOptions.firstOrNull { it.id == doc.wakeWord }
            ExposedDropdownMenuBox(
                expanded = wakeExpanded,
                onExpandedChange = { wakeExpanded = it },
            ) {
                OutlinedTextField(
                    value = selectedWake?.label ?: doc.wakeWord,
                    onValueChange = {},
                    readOnly = true,
                    label = { Text(stringResource(R.string.settings_wake_word_label)) },
                    supportingText = {
                        if (state.wakeCatalogOffline) {
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
                    state.wakeOptions.forEach { option ->
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
                                viewModel.setWakeWord(option.id)
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
                        description = if (state.porcupineAvailable) {
                            stringResource(R.string.settings_engine_porcupine_desc)
                        } else {
                            stringResource(R.string.settings_engine_porcupine_unavailable)
                        },
                        enabled = state.porcupineAvailable,
                    ),
                ),
                selected = doc.wakeEngine,
                onSelect = viewModel::setWakeEngine,
            )

            // Sensitivity slider (bounded control — never a text field).
            val sensitivityPercent = (doc.sensitivity * 100).roundToInt()
            Text(
                stringResource(R.string.settings_sensitivity_label, sensitivityPercent),
                style = MaterialTheme.typography.bodyMedium,
            )
            Slider(
                value = doc.sensitivity,
                onValueChange = { viewModel.setSensitivity(it) },
                valueRange = 0f..1f,
                modifier = Modifier
                    .fillMaxWidth()
                    .semantics {
                        contentDescription = "Wake word sensitivity"
                        stateDescription = "$sensitivityPercent percent"
                    },
            )
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
                value = state.customPhrase,
                onValueChange = viewModel::setCustomPhrase,
                label = { Text(stringResource(R.string.settings_custom_wake_label)) },
                supportingText = {
                    Text(
                        if (state.customPhrase.isNotBlank() && !state.customPhraseValid) {
                            stringResource(R.string.settings_custom_wake_invalid)
                        } else {
                            stringResource(R.string.settings_custom_wake_hint)
                        },
                    )
                },
                singleLine = true,
                enabled = !state.customRequestInProgress,
                modifier = Modifier.fillMaxWidth(),
            )
            Button(
                onClick = viewModel::requestCustomWakeWord,
                enabled = state.customPhraseValid && !state.customRequestInProgress,
                modifier = Modifier.heightIn(min = 48.dp),
            ) { Text(stringResource(R.string.settings_custom_wake_submit)) }

            state.customJob?.let { job ->
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
                                onClick = viewModel::clearCustomJob,
                                modifier = Modifier.heightIn(min = 48.dp),
                            ) { Text(stringResource(R.string.settings_custom_wake_dismiss)) }
                            if (job.ready) {
                                Button(
                                    onClick = viewModel::useCustomWakeWord,
                                    modifier = Modifier
                                        .padding(start = 8.dp)
                                        .heightIn(min = 48.dp),
                                ) { Text(stringResource(R.string.settings_custom_wake_use)) }
                            }
                        }
                    }
                }
            }

            HorizontalDivider(Modifier.padding(vertical = 8.dp))

            // ---------- Conversation ----------
            SectionHeader(stringResource(R.string.settings_section_conversation))

            // Persona select (IDs only; server resolves instructions).
            var personaExpanded by remember { mutableStateOf(false) }
            val selectedPersona =
                state.personaPresets.firstOrNull { it.id == doc.personaPresetId }
                    ?: state.personaPresets.first()
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
                    state.personaPresets.forEach { preset ->
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
                                viewModel.setPersona(preset.id)
                                personaExpanded = false
                            },
                        )
                    }
                }
            }

            // Custom system instructions — the schema's sole justified free-text
            // field, revealed only when persona == custom (progressive disclosure).
            if (doc.personaPresetId == "custom") {
                val instructions = doc.personaSystemInstructions.orEmpty()
                OutlinedTextField(
                    value = instructions,
                    onValueChange = viewModel::setCustomInstructions,
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
                    val selected = doc.displayVoice == voice
                    val voiceLabel = voice.replaceFirstChar { it.uppercaseChar() }
                    Row(
                        modifier = Modifier
                            .fillMaxWidth()
                            .heightIn(min = 48.dp)
                            .selectable(
                                selected = selected,
                                role = Role.RadioButton,
                                onClick = { viewModel.setVoice(voice) },
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
                            onClick = viewModel::onVoicePreviewRequested,
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
                selected = doc.turnDetection,
                onSelect = viewModel::setTurnDetection,
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
                selected = doc.voiceEngineDefault,
                onSelect = viewModel::setVoiceEngine,
            )

            // Gemini voice picker (M13, D4) — progressive disclosure: only
            // when the Gemini engine is selected. Populated from the
            // `geminiVoices` catalog (GET /api/v1/realtime/voices); writes the
            // top-level `geminiVoice` settings key.
            if (doc.voiceEngineDefault == SettingsViewModel.GEMINI_ENGINE) {
                var geminiVoiceExpanded by remember { mutableStateOf(false) }
                val selectedGeminiVoice = doc.geminiVoice
                    .ifEmpty { SettingsDocument.DEFAULT_GEMINI_VOICE }
                val selectedGeminiOption =
                    state.geminiVoices.firstOrNull { it.id == selectedGeminiVoice }
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
                                if (state.geminiVoices.isEmpty()) {
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
                        state.geminiVoices.forEach { option ->
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
                                    viewModel.setGeminiVoice(option.id)
                                    geminiVoiceExpanded = false
                                },
                            )
                        }
                    }
                }
            }

            HorizontalDivider(Modifier.padding(vertical = 8.dp))

            // ---------- Audio ----------
            SectionHeader(stringResource(R.string.settings_section_audio))
            var micExpanded by remember { mutableStateOf(false) }
            val selectedMic = state.micDevices.firstOrNull { it.id == doc.micDeviceId }
                ?: state.micDevices.firstOrNull()
            ExposedDropdownMenuBox(
                expanded = micExpanded,
                onExpandedChange = {
                    if (it) viewModel.refreshMicDevices()
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
                    state.micDevices.forEach { device ->
                        DropdownMenuItem(
                            text = { Text(device.label) },
                            onClick = {
                                viewModel.setMicDevice(device.id)
                                micExpanded = false
                            },
                        )
                    }
                }
            }

            HorizontalDivider(Modifier.padding(vertical = 8.dp))

            // ---------- Voice & Screen (01-platform §B-iv) ----------
            SectionHeader(stringResource(R.string.settings_section_voice_screen))
            LabeledSwitchRow(
                label = stringResource(R.string.settings_locked_sessions_label),
                description = stringResource(R.string.settings_locked_sessions_desc),
                checked = doc.lockedSessions,
                onCheckedChange = viewModel::setLockedSessions,
            )
            LabeledSwitchRow(
                label = stringResource(R.string.settings_wake_screen_label),
                description = stringResource(R.string.settings_wake_screen_desc),
                checked = doc.wakeScreenOnWake,
                onCheckedChange = viewModel::setWakeScreenOnWake,
            )
            LabeledSwitchRow(
                label = stringResource(R.string.settings_keep_screen_on_label),
                description = stringResource(R.string.settings_keep_screen_on_desc),
                checked = doc.keepScreenOn,
                onCheckedChange = viewModel::setKeepScreenOn,
            )

            // Battery-optimization health card + action row (01-platform §C).
            BatteryHealthCard(
                ignored = state.batteryOptimizationIgnored,
                onExempt = { batteryLauncher.launch(viewModel.batteryExemptionIntent()) },
                onRecheck = viewModel::refreshBatteryStatus,
            )

            // Per-OEM guidance (M8.4): OEM battery/sleep layers on top of Android's
            // own Doze exemption above — Samsung (the owner's phone) gets concrete
            // steps, every other manufacturer gets a generic pointer at the card above.
            OemGuidanceCard(
                onOpenAppInfo = {
                    val intent = Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS)
                        .setData(Uri.fromParts("package", context.packageName, null))
                    appInfoLauncher.launch(intent)
                },
            )

            HorizontalDivider(Modifier.padding(vertical = 8.dp))

            // ---------- Appearance ----------
            SectionHeader(stringResource(R.string.settings_section_appearance))
            val isHal = doc.appStyle == "hal9000"
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
                selected = doc.appStyle,
                onSelect = viewModel::setAppStyle,
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
                        selected = doc.theme == value,
                        onClick = { viewModel.setTheme(value) },
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

            HorizontalDivider(Modifier.padding(vertical = 8.dp))

            // ---------- Privacy ----------
            SectionHeader(stringResource(R.string.settings_section_privacy))
            LabeledSwitchRow(
                label = stringResource(R.string.settings_store_transcripts_label),
                description = stringResource(R.string.settings_store_transcripts_desc),
                checked = doc.storeTranscripts,
                onCheckedChange = viewModel::setStoreTranscripts,
            )
            LabeledSwitchRow(
                label = stringResource(R.string.settings_store_audio_label),
                description = stringResource(R.string.settings_store_audio_desc),
                checked = doc.storeAudio,
                onCheckedChange = viewModel::setStoreAudio,
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
                selected = doc.retentionDays.toString(),
                onSelect = { viewModel.setRetentionDays(it.toInt()) },
            )

            HorizontalDivider(Modifier.padding(vertical = 8.dp))

            // ---------- Diagnostics (04-logging §A5) ----------
            SectionHeader(stringResource(R.string.settings_section_diagnostics))
            val diagnostics = doc.diagnostics
            LabeledSwitchRow(
                label = stringResource(R.string.settings_diagnostics_master_label),
                description = stringResource(R.string.settings_diagnostics_master_desc),
                checked = diagnostics.enabled,
                onCheckedChange = viewModel::setDiagnosticsEnabled,
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
                    onSelect = viewModel::setDiagnosticsMinLevel,
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
                        onClick = { viewModel.setAllDiagnosticsCategories(true) },
                        modifier = Modifier.heightIn(min = 48.dp),
                    ) { Text(stringResource(R.string.settings_diagnostics_select_all)) }
                    TextButton(
                        onClick = { viewModel.setAllDiagnosticsCategories(false) },
                        modifier = Modifier.heightIn(min = 48.dp),
                    ) { Text(stringResource(R.string.settings_diagnostics_select_none)) }
                }
                DiagnosticsCategories(
                    categories = diagnostics.categories,
                    onToggle = viewModel::setDiagnosticsCategory,
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
                                val intent = viewModel.exportLogs()
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

            HorizontalDivider(Modifier.padding(vertical = 8.dp))

            // ---------- Account ----------
            SectionHeader(stringResource(R.string.settings_section_account))
            Text(
                if (state.signedIn) {
                    stringResource(R.string.settings_account_signed_in)
                } else {
                    stringResource(R.string.settings_account_signed_out)
                },
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            OutlinedButton(
                onClick = { confirmSignOut = true },
                enabled = state.accountActionsAvailable && !state.signOutInProgress,
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = 48.dp),
            ) { Text(stringResource(R.string.settings_sign_out)) }
            TextButton(
                onClick = { confirmSignOutEverywhere = true },
                enabled = state.accountActionsAvailable && !state.signOutInProgress,
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = 48.dp),
            ) { Text(stringResource(R.string.settings_sign_out_everywhere)) }
            if (!state.accountActionsAvailable) {
                Text(
                    stringResource(R.string.settings_account_unavailable),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }

            Text(
                stringResource(R.string.settings_version_caption, doc.version),
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(top = 8.dp, bottom = 24.dp),
            )
        }
    }

    if (confirmSignOut) {
        ConfirmDialog(
            title = stringResource(R.string.settings_sign_out_confirm_title),
            body = stringResource(R.string.settings_sign_out_confirm_body),
            confirmLabel = stringResource(R.string.settings_sign_out),
            onConfirm = {
                confirmSignOut = false
                viewModel.signOut()
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
                viewModel.signOutEverywhere()
            },
            onDismiss = { confirmSignOutEverywhere = false },
        )
    }
    if (confirmClearLogs) {
        ConfirmDialog(
            title = stringResource(R.string.settings_diagnostics_clear_confirm_title),
            body = stringResource(R.string.settings_diagnostics_clear_confirm_body),
            confirmLabel = stringResource(R.string.settings_diagnostics_clear),
            onConfirm = {
                confirmClearLogs = false
                viewModel.clearLogs()
            },
            onDismiss = { confirmClearLogs = false },
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
