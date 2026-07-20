package ninja.jeremy.liveninja.ui.onboarding

import android.Manifest
import android.app.Activity
import android.content.Intent
import android.net.Uri
import android.provider.Settings
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Assistant
import androidx.compose.material.icons.filled.BatteryFull
import androidx.compose.material.icons.filled.CheckCircle
import androidx.compose.material.icons.filled.GraphicEq
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.PictureInPicture
import androidx.compose.material.icons.filled.Shield
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.ui.state.WakeWordOption

/**
 * First-run onboarding wizard (plan.md M4 "Onboarding wizard" task; mockups
 * 01-onboarding, 02-login-lwa, 03-set-default-assistant, 04-permissions +
 * wake-word pick). Six steps: welcome → sign-in → mic (prominent disclosure +
 * consent logging) → notifications → assistant role (OEM-aware fallback
 * walkthrough) → wake-word pick.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun OnboardingScreen(
    onFinished: () -> Unit,
    viewModel: OnboardingViewModel = hiltViewModel(),
) {
    val state by viewModel.state.collectAsState()
    val context = LocalContext.current
    val lifecycleOwner = LocalLifecycleOwner.current

    // Re-check permissions/role whenever the user returns from system settings.
    DisposableEffect(lifecycleOwner) {
        val observer = LifecycleEventObserver { _, event ->
            if (event == Lifecycle.Event.ON_RESUME) viewModel.refreshStatuses()
        }
        lifecycleOwner.lifecycle.addObserver(observer)
        onDispose { lifecycleOwner.lifecycle.removeObserver(observer) }
    }

    val micLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted -> viewModel.onMicPermissionResult(granted) }

    val notificationLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted -> viewModel.onNotificationsResult(granted) }

    val roleLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { viewModel.onRoleRequestReturned() }

    val overlayLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { viewModel.onOverlayReturned() }

    val batteryLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { viewModel.refreshStatuses() }

    val stepIndex = OnboardingStep.entries.indexOf(state.step)
    val stepCount = OnboardingStep.entries.size

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Text(
                        stringResource(
                            R.string.onboarding_step_progress,
                            stepIndex + 1,
                            stepCount,
                        ),
                        style = MaterialTheme.typography.titleMedium,
                    )
                },
                navigationIcon = {
                    if (state.step != OnboardingStep.WELCOME) {
                        IconButton(
                            onClick = { viewModel.back() },
                            modifier = Modifier.size(48.dp),
                        ) {
                            Icon(
                                Icons.AutoMirrored.Filled.ArrowBack,
                                contentDescription = stringResource(R.string.onboarding_back),
                            )
                        }
                    }
                },
            )
        },
    ) { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding),
        ) {
            LinearProgressIndicator(
                progress = { (stepIndex + 1f) / stepCount },
                modifier = Modifier
                    .fillMaxWidth()
                    .semantics {
                        contentDescription = "Setup progress: step ${stepIndex + 1} of $stepCount"
                    },
            )
            Column(
                modifier = Modifier
                    .fillMaxSize()
                    .verticalScroll(rememberScrollState())
                    .padding(24.dp),
                verticalArrangement = Arrangement.spacedBy(16.dp),
            ) {
                when (state.step) {
                    OnboardingStep.WELCOME -> WelcomeStep(onNext = viewModel::next)

                    OnboardingStep.SIGN_IN -> SignInStep(
                        signInAvailable = state.signInAvailable,
                        signedIn = state.signedIn,
                        onSignIn = { (context as? Activity)?.let(viewModel::beginSignIn) },
                        onNext = viewModel::next,
                    )

                    OnboardingStep.MIC_PERMISSION -> {
                        LaunchedEffect(Unit) { viewModel.onMicDisclosureShown() }
                        MicPermissionStep(
                            granted = state.micGranted,
                            onGrant = { micLauncher.launch(Manifest.permission.RECORD_AUDIO) },
                            onNext = viewModel::next,
                        )
                    }

                    OnboardingStep.NOTIFICATIONS -> NotificationStep(
                        granted = state.notificationsGranted,
                        requestable = state.notificationsRequestable,
                        onGrant = {
                            notificationLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
                        },
                        onNext = viewModel::next,
                    )

                    OnboardingStep.ASSISTANT_ROLE -> AssistantRoleStep(
                        roleHeld = state.roleHeld,
                        requestAttempted = state.roleRequestAttempted,
                        overlayGranted = state.overlayGranted,
                        oemBucket = viewModel.oemBucket,
                        onRequestRole = {
                            val roleIntent = viewModel.assistantRoleRequestIntent()
                            if (roleIntent != null) {
                                roleLauncher.launch(roleIntent)
                            } else {
                                // OEM blocks the role dialog — guided settings
                                // walkthrough + isRoleHeld polling instead.
                                viewModel.openAssistantSettings(context)
                            }
                        },
                        onOpenSettings = { viewModel.openAssistantSettings(context) },
                        onRequestOverlay = {
                            overlayLauncher.launch(
                                Intent(
                                    Settings.ACTION_MANAGE_OVERLAY_PERMISSION,
                                    Uri.parse("package:${context.packageName}"),
                                ),
                            )
                        },
                        onNext = viewModel::next,
                        onSkip = viewModel::onRoleSkipped,
                    )

                    OnboardingStep.BATTERY -> BatteryStep(
                        ignored = state.batteryOptimizationIgnored,
                        onExempt = { batteryLauncher.launch(viewModel.batteryExemptionIntent()) },
                        onNext = viewModel::next,
                    )

                    OnboardingStep.WAKE_WORD -> WakeWordStep(
                        options = state.wakeWordOptions,
                        selectedId = state.selectedWakeWordId,
                        onSelect = viewModel::selectWakeWord,
                        onFinish = {
                            viewModel.finish()
                            onFinished()
                        },
                    )
                }
            }
        }
    }
}

@Composable
private fun WelcomeStep(onNext: () -> Unit) {
    Column(
        modifier = Modifier.fillMaxWidth(),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        Icon(
            Icons.Filled.GraphicEq,
            contentDescription = null,
            modifier = Modifier
                .size(96.dp)
                .padding(top = 24.dp),
            tint = MaterialTheme.colorScheme.primary,
        )
        Text(
            stringResource(R.string.onboarding_welcome_title),
            style = MaterialTheme.typography.headlineMedium,
            textAlign = TextAlign.Center,
        )
        Text(
            stringResource(R.string.onboarding_welcome_body),
            style = MaterialTheme.typography.bodyLarge,
            textAlign = TextAlign.Center,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Card(modifier = Modifier.fillMaxWidth()) {
            Column(
                Modifier.padding(16.dp),
                verticalArrangement = Arrangement.spacedBy(4.dp),
            ) {
                Text(
                    stringResource(R.string.onboarding_welcome_wake_caption),
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Text(
                    stringResource(R.string.onboarding_welcome_wake_phrase),
                    style = MaterialTheme.typography.titleLarge,
                )
            }
        }
        Spacer(Modifier.weight(1f, fill = false))
        Button(
            onClick = onNext,
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 48.dp),
        ) {
            Text(stringResource(R.string.onboarding_get_started))
        }
    }
}

@Composable
private fun SignInStep(
    signInAvailable: Boolean,
    signedIn: Boolean,
    onSignIn: () -> Unit,
    onNext: () -> Unit,
) {
    StepHeader(
        icon = Icons.Filled.Shield,
        title = stringResource(R.string.onboarding_signin_title),
        body = stringResource(R.string.onboarding_signin_body),
    )
    if (signedIn) {
        StatusCard(text = stringResource(R.string.onboarding_signin_done))
        Button(
            onClick = onNext,
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_continue)) }
    } else {
        Button(
            onClick = onSignIn,
            enabled = signInAvailable,
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_signin_button)) }
        if (!signInAvailable) {
            Text(
                stringResource(R.string.onboarding_signin_unavailable),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Text(
            stringResource(R.string.onboarding_signin_privacy),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        TextButton(
            onClick = onNext,
            modifier = Modifier.heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_skip_for_now)) }
    }
}

@Composable
private fun MicPermissionStep(
    granted: Boolean,
    onGrant: () -> Unit,
    onNext: () -> Unit,
) {
    StepHeader(
        icon = Icons.Filled.Mic,
        title = stringResource(R.string.onboarding_mic_title),
        body = stringResource(R.string.onboarding_mic_body),
    )
    // Prominent disclosure (Google Play policy): shown BEFORE the runtime prompt,
    // and its display + the user's decision are both written to the consent log.
    Card(
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.secondaryContainer,
        ),
    ) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(
                stringResource(R.string.onboarding_mic_disclosure_title),
                style = MaterialTheme.typography.titleSmall,
            )
            Text(
                stringResource(R.string.onboarding_mic_disclosure_body),
                style = MaterialTheme.typography.bodyMedium,
            )
        }
    }
    // Persistent green-mic-indicator expectation-setting (01-platform §C).
    Card(modifier = Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(
                stringResource(R.string.onboarding_mic_indicator_title),
                style = MaterialTheme.typography.titleSmall,
            )
            Text(
                stringResource(R.string.onboarding_mic_indicator_body),
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
    if (granted) {
        StatusCard(text = stringResource(R.string.onboarding_mic_granted))
        Button(
            onClick = onNext,
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_continue)) }
    } else {
        Button(
            onClick = onGrant,
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_mic_grant)) }
        TextButton(
            onClick = onNext,
            modifier = Modifier.heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_mic_not_now)) }
    }
}

@Composable
private fun NotificationStep(
    granted: Boolean,
    requestable: Boolean,
    onGrant: () -> Unit,
    onNext: () -> Unit,
) {
    StepHeader(
        icon = Icons.Filled.Notifications,
        title = stringResource(R.string.onboarding_notif_title),
        body = stringResource(R.string.onboarding_notif_body),
    )
    if (granted || !requestable) {
        StatusCard(text = stringResource(R.string.onboarding_notif_granted))
        Button(
            onClick = onNext,
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_continue)) }
    } else {
        Button(
            onClick = onGrant,
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_notif_grant)) }
        TextButton(
            onClick = onNext,
            modifier = Modifier.heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_skip_for_now)) }
    }
}

@Composable
private fun AssistantRoleStep(
    roleHeld: Boolean,
    requestAttempted: Boolean,
    overlayGranted: Boolean,
    oemBucket: OemBucket,
    onRequestRole: () -> Unit,
    onOpenSettings: () -> Unit,
    onRequestOverlay: () -> Unit,
    onNext: () -> Unit,
    onSkip: () -> Unit,
) {
    StepHeader(
        icon = Icons.Filled.Assistant,
        title = stringResource(R.string.onboarding_role_title),
        body = stringResource(R.string.onboarding_role_body),
    )
    if (roleHeld) {
        StatusCard(text = stringResource(R.string.onboarding_role_held))
    } else {
        Button(
            onClick = onRequestRole,
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_role_set_button)) }

        // OEM fallback walkthrough — revealed after the first attempt (or on
        // OEMs where the role dialog isn't offered at all).
        if (requestAttempted) {
            Card(modifier = Modifier.fillMaxWidth()) {
                Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text(
                        stringResource(R.string.onboarding_role_walkthrough_title),
                        style = MaterialTheme.typography.titleSmall,
                    )
                    val pathRes = when (oemBucket) {
                        OemBucket.SAMSUNG -> R.string.onboarding_role_path_samsung
                        OemBucket.XIAOMI -> R.string.onboarding_role_path_xiaomi
                        OemBucket.ONEPLUS_OPPO -> R.string.onboarding_role_path_oneplus
                        OemBucket.AOSP_PIXEL -> R.string.onboarding_role_path_aosp
                    }
                    Text(stringResource(pathRes), style = MaterialTheme.typography.bodyMedium)
                    Text(
                        stringResource(R.string.onboarding_role_walkthrough_hint),
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    OutlinedButton(
                        onClick = onOpenSettings,
                        modifier = Modifier
                            .fillMaxWidth()
                            .heightIn(min = 48.dp),
                    ) { Text(stringResource(R.string.onboarding_role_open_settings)) }
                }
            }
        }
    }

    // Optional on-screen bubble permission (live overlay while a session runs).
    Card(modifier = Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Row(
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                Icon(
                    Icons.Filled.PictureInPicture,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.primary,
                )
                Text(
                    stringResource(R.string.onboarding_overlay_title),
                    style = MaterialTheme.typography.titleSmall,
                )
            }
            Text(
                stringResource(R.string.onboarding_overlay_body),
                style = MaterialTheme.typography.bodyMedium,
            )
            if (overlayGranted) {
                Text(
                    stringResource(R.string.onboarding_overlay_granted),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.primary,
                )
            } else {
                OutlinedButton(
                    onClick = onRequestOverlay,
                    modifier = Modifier
                        .fillMaxWidth()
                        .heightIn(min = 48.dp),
                ) { Text(stringResource(R.string.onboarding_overlay_allow)) }
            }
        }
    }

    if (roleHeld) {
        Button(
            onClick = onNext,
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_continue)) }
    } else {
        TextButton(
            onClick = onSkip,
            modifier = Modifier.heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_role_skip)) }
    }
}

@Composable
private fun BatteryStep(
    ignored: Boolean,
    onExempt: () -> Unit,
    onNext: () -> Unit,
) {
    StepHeader(
        icon = Icons.Filled.BatteryFull,
        title = stringResource(R.string.onboarding_battery_title),
        body = stringResource(R.string.onboarding_battery_body),
    )
    if (ignored) {
        StatusCard(text = stringResource(R.string.onboarding_battery_done))
        Button(
            onClick = onNext,
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_continue)) }
    } else {
        Button(
            onClick = onExempt,
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_battery_grant)) }
        TextButton(
            onClick = onNext,
            modifier = Modifier.heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.onboarding_skip_for_now)) }
    }
}

@Composable
private fun WakeWordStep(
    options: List<WakeWordOption>,
    selectedId: String,
    onSelect: (String) -> Unit,
    onFinish: () -> Unit,
) {
    StepHeader(
        icon = Icons.Filled.GraphicEq,
        title = stringResource(R.string.onboarding_wake_title),
        body = stringResource(R.string.onboarding_wake_body),
    )
    Column(
        Modifier
            .fillMaxWidth()
            .selectableGroup(),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        options.forEach { option ->
            val selected = option.id == selectedId
            Card(
                modifier = Modifier
                    .fillMaxWidth()
                    .selectable(
                        selected = selected,
                        role = Role.RadioButton,
                        onClick = { onSelect(option.id) },
                    ),
                colors = if (selected) {
                    CardDefaults.cardColors(
                        containerColor = MaterialTheme.colorScheme.secondaryContainer,
                    )
                } else {
                    CardDefaults.cardColors()
                },
            ) {
                Row(
                    Modifier
                        .fillMaxWidth()
                        .heightIn(min = 56.dp)
                        .padding(horizontal = 16.dp, vertical = 12.dp),
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    RadioButton(selected = selected, onClick = null)
                    Column {
                        Text(option.label, style = MaterialTheme.typography.titleMedium)
                        if (option.description.isNotBlank()) {
                            Text(
                                option.description,
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                    }
                }
            }
        }
    }
    Button(
        onClick = onFinish,
        modifier = Modifier
            .fillMaxWidth()
            .heightIn(min = 48.dp),
    ) { Text(stringResource(R.string.onboarding_finish)) }
}

@Composable
private fun StepHeader(
    icon: androidx.compose.ui.graphics.vector.ImageVector,
    title: String,
    body: String,
) {
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Icon(
            icon,
            contentDescription = null,
            modifier = Modifier.size(48.dp),
            tint = MaterialTheme.colorScheme.primary,
        )
        Text(title, style = MaterialTheme.typography.headlineSmall)
        Text(
            body,
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun StatusCard(text: String) {
    Card(
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.primaryContainer,
        ),
    ) {
        Row(
            Modifier.padding(16.dp),
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Icon(
                Icons.Filled.CheckCircle,
                contentDescription = null,
                tint = MaterialTheme.colorScheme.primary,
            )
            Text(text, style = MaterialTheme.typography.bodyMedium)
        }
    }
}
