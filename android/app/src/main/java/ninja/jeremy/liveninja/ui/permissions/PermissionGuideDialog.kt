package ninja.jeremy.liveninja.ui.permissions

import android.Manifest
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.ContextWrapper
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.provider.Settings
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.core.app.ActivityCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import ninja.jeremy.liveninja.R

/** Reopens the same optional guide, including recovery after denial or grant revocation. */
@Composable
fun PermissionSettingsButton() {
    var visible by rememberSaveable { mutableStateOf(false) }
    OutlinedButton(
        onClick = { visible = true },
        modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp),
    ) { Text(stringResource(R.string.permission_guide_settings)) }
    if (visible) PermissionGuideDialog(onDismiss = { visible = false })
}

@Composable
fun PermissionGuideDialog(
    onDismiss: () -> Unit,
    viewModel: PermissionGuideViewModel = hiltViewModel(),
) {
    val context = LocalContext.current
    val lifecycleOwner = LocalLifecycleOwner.current
    val coordinator = remember(context) { PermissionCoordinator(AndroidPermissionHistory(context)) }
    var refresh by remember { mutableIntStateOf(0) }
    var pending by rememberSaveable { mutableStateOf<String?>(null) }
    var message by rememberSaveable { mutableStateOf<Int?>(null) }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        pending?.let { viewModel.onResult(PermissionFeature.valueOf(it), granted) }
        pending = null
        message = if (granted) null else R.string.permission_guide_denied
        refresh++
    }
    DisposableEffect(lifecycleOwner) {
        val observer = LifecycleEventObserver { _, event ->
            if (event == Lifecycle.Event.ON_RESUME) refresh++
        }
        lifecycleOwner.lifecycle.addObserver(observer)
        onDispose { lifecycleOwner.lifecycle.removeObserver(observer) }
    }

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(stringResource(R.string.permission_guide_title)) },
        text = {
            Column(
                Modifier.heightIn(max = 520.dp).verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                Text(stringResource(R.string.permission_guide_intro))
                message?.let { Text(stringResource(it), style = MaterialTheme.typography.bodyMedium) }
                PermissionFeature.entries.forEach { feature ->
                    val facts = remember(context, refresh, feature) { permissionFacts(context, feature) }
                    val action = coordinator.action(feature, facts)
                    HorizontalDivider()
                    Text(stringResource(feature.title()), style = MaterialTheme.typography.titleSmall)
                    Text(stringResource(feature.explanation()))
                    Text(
                        stringResource(
                            when {
                                !facts.hardwareAvailable -> R.string.permission_guide_unavailable
                                facts.granted -> R.string.permission_guide_allowed
                                else -> R.string.permission_guide_not_allowed
                            },
                        ),
                        style = MaterialTheme.typography.labelLarge,
                    )
                    if (facts.hardwareAvailable) {
                        OutlinedButton(
                            enabled = pending == null,
                            modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp),
                            onClick = {
                                // Recheck at the tap: settings/one-time grants may have changed.
                                val current = permissionFacts(context, feature)
                                when (coordinator.action(feature, current)) {
                                    PermissionAction.REQUEST -> {
                                        viewModel.onDisclosure(feature)
                                        coordinator.requestStarted(feature)
                                        pending = feature.name
                                        message = null
                                        try {
                                            launcher.launch(feature.runtimePermission())
                                        } catch (_: ActivityNotFoundException) {
                                            pending = null
                                            message = R.string.permission_guide_settings_unavailable
                                        } catch (_: SecurityException) {
                                            pending = null
                                            message = R.string.permission_guide_settings_unavailable
                                        }
                                    }
                                    PermissionAction.OPEN_SETTINGS, PermissionAction.NONE -> {
                                        if (!openPermissionSettings(context, feature)) {
                                            message = R.string.permission_guide_settings_unavailable
                                        }
                                    }
                                }
                                refresh++
                            },
                        ) {
                            Text(stringResource(
                                if (action == PermissionAction.REQUEST) R.string.permission_guide_enable
                                else R.string.permission_guide_open_settings,
                                stringResource(feature.title()),
                            ))
                        }
                    }
                }
                HorizontalDivider()
                Text(stringResource(R.string.permission_guide_updates_title), style = MaterialTheme.typography.titleSmall)
                Text(stringResource(R.string.permission_guide_updates_body))
                Text(stringResource(R.string.permission_guide_handsfree_title), style = MaterialTheme.typography.titleSmall)
                Text(stringResource(R.string.permission_guide_handsfree_body))
                if (Build.VERSION.SDK_INT >= 34) {
                    TextButton(
                        enabled = pending == null,
                        modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp),
                        onClick = {
                            if (!openFullScreenWakeSettings(context)) {
                                message = R.string.permission_guide_settings_unavailable
                            }
                        },
                    ) { Text(stringResource(R.string.permission_guide_fullscreen_settings)) }
                }
            }
        },
        confirmButton = {
            TextButton(onClick = onDismiss) { Text(stringResource(R.string.permission_guide_done)) }
        },
    )
}

internal fun PermissionFeature.runtimePermission(): String = when (this) {
    PermissionFeature.MICROPHONE -> Manifest.permission.RECORD_AUDIO
    PermissionFeature.CAMERA -> Manifest.permission.CAMERA
    PermissionFeature.NOTIFICATIONS -> Manifest.permission.POST_NOTIFICATIONS
    PermissionFeature.APPROXIMATE_LOCATION -> Manifest.permission.ACCESS_COARSE_LOCATION
}

private fun PermissionFeature.title(): Int = when (this) {
    PermissionFeature.MICROPHONE -> R.string.onboarding_mic_title
    PermissionFeature.CAMERA -> R.string.onboarding_camera_title
    PermissionFeature.NOTIFICATIONS -> R.string.onboarding_notif_title
    PermissionFeature.APPROXIMATE_LOCATION -> R.string.permission_guide_location_title
}

private fun PermissionFeature.explanation(): Int = when (this) {
    PermissionFeature.MICROPHONE -> R.string.onboarding_mic_disclosure_body
    PermissionFeature.CAMERA -> R.string.onboarding_camera_disclosure_body
    PermissionFeature.NOTIFICATIONS -> R.string.permission_guide_notifications_body
    PermissionFeature.APPROXIMATE_LOCATION -> R.string.permission_guide_location_body
}

internal fun permissionFacts(context: Context, feature: PermissionFeature): PermissionFacts {
    val permission = feature.runtimePermission()
    val runtimeGranted = ContextCompat.checkSelfPermission(context, permission) == PackageManager.PERMISSION_GRANTED
    val notification = feature == PermissionFeature.NOTIFICATIONS
    return PermissionFacts(
        granted = if (notification) NotificationManagerCompat.from(context).areNotificationsEnabled() &&
            (Build.VERSION.SDK_INT < 33 || runtimeGranted) else runtimeGranted,
        shouldShowRationale = context.activity()?.let {
            ActivityCompat.shouldShowRequestPermissionRationale(it, permission)
        } ?: false,
        runtimeRequestAvailable = !notification || (Build.VERSION.SDK_INT >= 33 && !runtimeGranted),
        hardwareAvailable = feature != PermissionFeature.CAMERA ||
            context.packageManager.hasSystemFeature(PackageManager.FEATURE_CAMERA_ANY),
    )
}

private tailrec fun Context.activity(): Activity? = when (this) {
    is Activity -> this
    is ContextWrapper -> if (baseContext === this) null else baseContext.activity()
    else -> null
}

/** Explicit user navigation only. A missing OEM settings activity never blocks the app. */
internal fun openPermissionSettings(context: Context, feature: PermissionFeature): Boolean {
    val intent = if (feature == PermissionFeature.NOTIFICATIONS) {
        Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS).putExtra(Settings.EXTRA_APP_PACKAGE, context.packageName)
    } else {
        Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:${context.packageName}"))
    }
    return openSettings(context, intent)
}

internal fun openFullScreenWakeSettings(context: Context): Boolean {
    if (Build.VERSION.SDK_INT < 34) return false
    return openSettings(context, Intent(
        Settings.ACTION_MANAGE_APP_USE_FULL_SCREEN_INTENT,
        Uri.parse("package:${context.packageName}"),
    ))
}

private fun openSettings(context: Context, intent: Intent): Boolean = try {
    context.startActivity(intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
    true
} catch (_: ActivityNotFoundException) {
    false
} catch (_: SecurityException) {
    false
}
