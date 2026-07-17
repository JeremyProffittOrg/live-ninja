@file:OptIn(ExperimentalMaterial3Api::class)

package ninja.jeremy.liveninja.ui.screens

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.PlayCircleOutline
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
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
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.semantics.stateDescription
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import kotlin.math.roundToInt
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.ui.settings.SettingsNotice
import ninja.jeremy.liveninja.ui.settings.SettingsViewModel
import ninja.jeremy.liveninja.ui.state.SettingsDocument

/**
 * Settings tab — schema-driven form over contracts/settings.schema.json
 * (mockups/android/09-settings.html). Every enumerable field is a populated
 * control (combobox/radio/slider/segmented/switch); the single free-text field
 * is the custom-persona system instructions, the schema's one justified case.
 */
@Composable
fun SettingsScreen(modifier: Modifier = Modifier) {
    val viewModel: SettingsViewModel = hiltViewModel()
    val state by viewModel.state.collectAsState()
    val doc = state.doc
    val snackbarHostState = remember { SnackbarHostState() }
    val context = LocalContext.current

    LaunchedEffect(Unit) {
        viewModel.notices.collect { notice ->
            val messageRes = when (notice) {
                SettingsNotice.VOICE_PREVIEW_UNAVAILABLE -> R.string.settings_voice_preview_unavailable
                SettingsNotice.SIGNED_OUT -> R.string.settings_signed_out
                SettingsNotice.SIGNED_OUT_EVERYWHERE -> R.string.settings_signed_out_everywhere
                SettingsNotice.SIGN_OUT_FAILED -> R.string.settings_sign_out_failed
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

            // ---------- Appearance ----------
            SectionHeader(stringResource(R.string.settings_section_appearance))
            val themeChoices = listOf(
                "light" to stringResource(R.string.settings_theme_light),
                "dark" to stringResource(R.string.settings_theme_dark),
                "system" to stringResource(R.string.settings_theme_system),
            )
            SingleChoiceSegmentedButtonRow(modifier = Modifier.fillMaxWidth()) {
                themeChoices.forEachIndexed { index, (value, label) ->
                    SegmentedButton(
                        selected = doc.theme == value,
                        onClick = { viewModel.setTheme(value) },
                        shape = SegmentedButtonDefaults.itemShape(
                            index = index,
                            count = themeChoices.size,
                        ),
                        modifier = Modifier.heightIn(min = 44.dp),
                    ) { Text(label) }
                }
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
