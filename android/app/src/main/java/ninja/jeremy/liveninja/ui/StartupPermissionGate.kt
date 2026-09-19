package ninja.jeremy.liveninja.ui

import android.Manifest
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.core.content.ContextCompat
import ninja.jeremy.liveninja.R

/**
 * Asks for every permission the app actually uses, once per process, when
 * the signed-in UI starts. Runtime grants first (one system sheet), then
 * the special settings screens that Android will not batch.
 */
@Composable
fun StartupPermissionGate() {
    val context = LocalContext.current
    var special by rememberSaveable { mutableStateOf<String?>(null) }
    var runtimeAsked by rememberSaveable { mutableStateOf(false) }

    val runtimeLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestMultiplePermissions(),
    ) { special = nextSpecial(context) }

    LaunchedEffect(Unit) {
        if (!runtimeAsked) {
            runtimeAsked = true
            val missing = missingRuntime(context)
            if (missing.isNotEmpty()) {
                runtimeLauncher.launch(missing)
            } else {
                special = nextSpecial(context)
            }
        }
    }

    when (special) {
        "battery" -> SpecialPermissionDialog(
            title = stringResource(R.string.perm_battery_title),
            body = stringResource(R.string.perm_battery_body),
            onContinue = {
                context.startActivity(batteryIntent(context))
                special = nextSpecial(context, skip = "battery")
            },
            onSkip = { special = nextSpecial(context, skip = "battery") },
        )
        "overlay" -> SpecialPermissionDialog(
            title = stringResource(R.string.perm_overlay_title),
            body = stringResource(R.string.perm_overlay_body),
            onContinue = {
                context.startActivity(overlayIntent(context))
                special = nextSpecial(context, skip = "overlay")
            },
            onSkip = { special = nextSpecial(context, skip = "overlay") },
        )
        "install" -> SpecialPermissionDialog(
            title = stringResource(R.string.perm_install_title),
            body = stringResource(R.string.perm_install_body),
            onContinue = {
                context.startActivity(installIntent(context))
                special = nextSpecial(context, skip = "install")
            },
            onSkip = { special = nextSpecial(context, skip = "install") },
        )
    }
}

@Composable
private fun SpecialPermissionDialog(
    title: String,
    body: String,
    onContinue: () -> Unit,
    onSkip: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onSkip,
        title = { Text(title) },
        text = { Text(body) },
        confirmButton = {
            TextButton(onClick = onContinue) {
                Text(stringResource(R.string.perm_continue))
            }
        },
        dismissButton = {
            TextButton(onClick = onSkip) {
                Text(stringResource(R.string.perm_later))
            }
        },
    )
}

internal fun runtimePermissionNames(sdkInt: Int): List<String> = buildList {
    add(Manifest.permission.RECORD_AUDIO)
    add(Manifest.permission.CAMERA)
    if (sdkInt >= 33) add(Manifest.permission.POST_NOTIFICATIONS)
}

internal fun missingRuntime(context: Context): Array<String> =
    runtimePermissionNames(Build.VERSION.SDK_INT).filter {
        ContextCompat.checkSelfPermission(context, it) != PackageManager.PERMISSION_GRANTED
    }.toTypedArray()

private fun nextSpecial(context: Context, skip: String? = null): String? {
    val order = listOf("battery", "overlay", "install")
    for (id in order) {
        if (id == skip) continue
        val missing = when (id) {
            "battery" -> !isIgnoringBattery(context)
            "overlay" -> !Settings.canDrawOverlays(context)
            "install" -> !context.packageManager.canRequestPackageInstalls()
            else -> false
        }
        if (missing) return id
    }
    return null
}

private fun isIgnoringBattery(context: Context): Boolean {
    val pm = context.getSystemService(Context.POWER_SERVICE) as PowerManager
    return pm.isIgnoringBatteryOptimizations(context.packageName)
}

private fun batteryIntent(context: Context): Intent =
    Intent(
        Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
        Uri.parse("package:${context.packageName}"),
    ).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)

private fun overlayIntent(context: Context): Intent =
    Intent(
        Settings.ACTION_MANAGE_OVERLAY_PERMISSION,
        Uri.parse("package:${context.packageName}"),
    ).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)

private fun installIntent(context: Context): Intent =
    Intent(
        Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES,
        Uri.parse("package:${context.packageName}"),
    ).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
