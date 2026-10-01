package ninja.jeremy.liveninja.ui

import android.Manifest
import android.app.NotificationManager
import android.content.ActivityNotFoundException
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
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicBoolean
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/** Special-access grants Android will not batch into the runtime sheet. Order = ask order. */
enum class SpecialAccess {
    /** Samsung One UI kills the wake service otherwise. */
    BATTERY,

    /** "Install unknown apps", needed by the self-updater. */
    INSTALL_PACKAGES,

    /** API 34+: wake-word full-screen intent over the keyguard. */
    FULL_SCREEN_INTENT,

    /** Floating session bubble (LiveOverlayController uses SYSTEM_ALERT_WINDOW). */
    OVERLAY,
}

/** Current state of each special grant, read from the platform. */
data class SpecialAccessStatus(
    val batteryOptimizationIgnored: Boolean,
    val canInstallPackages: Boolean,
    val canUseFullScreenIntent: Boolean,
    val canDrawOverlays: Boolean,
)

/** Android-free builder for the start-up permission sequence (unit-tested). */
object StartupPermissionPlan {
    /** The app uses SYSTEM_ALERT_WINDOW (ui/overlay/LiveOverlayController), so overlay is asked. */
    const val APP_USES_OVERLAY = true

    /** Every runtime permission the manifest declares, for [sdkInt], in ask order. */
    fun runtimePermissions(sdkInt: Int): List<String> = buildList {
        add(Manifest.permission.RECORD_AUDIO)
        add(Manifest.permission.CAMERA)
        if (sdkInt >= 33) add(Manifest.permission.POST_NOTIFICATIONS)
        add(Manifest.permission.ACCESS_COARSE_LOCATION)
        add(Manifest.permission.ACCESS_FINE_LOCATION)
    }

    /**
     * Runtime permissions to put in the single system sheet: missing ones not
     * already asked in this process (onboarding counts). Fine location always
     * travels with coarse, because API 31+ rejects a fine-only request.
     */
    fun runtimeToRequest(
        sdkInt: Int,
        isGranted: (String) -> Boolean,
        alreadyAsked: Set<String>,
    ): List<String> {
        val wanted = runtimePermissions(sdkInt).filter { !isGranted(it) && it !in alreadyAsked }
        if (Manifest.permission.ACCESS_FINE_LOCATION in wanted &&
            Manifest.permission.ACCESS_COARSE_LOCATION !in wanted &&
            !isGranted(Manifest.permission.ACCESS_COARSE_LOCATION)
        ) {
            return runtimePermissions(sdkInt).filter { it in wanted || it == Manifest.permission.ACCESS_COARSE_LOCATION }
        }
        return wanted
    }

    /** Special-access screens still to show, in order, after the runtime sheet. */
    fun specialToShow(
        sdkInt: Int,
        status: SpecialAccessStatus,
        alreadyAsked: Set<SpecialAccess>,
        appUsesOverlay: Boolean = APP_USES_OVERLAY,
    ): List<SpecialAccess> = SpecialAccess.entries.filter { access ->
        if (access in alreadyAsked) return@filter false
        when (access) {
            SpecialAccess.BATTERY -> !status.batteryOptimizationIgnored
            SpecialAccess.INSTALL_PACKAGES -> !status.canInstallPackages
            SpecialAccess.FULL_SCREEN_INTENT -> sdkInt >= 34 && !status.canUseFullScreenIntent
            SpecialAccess.OVERLAY -> appUsesOverlay && !status.canDrawOverlays
        }
    }
}

/**
 * Process-wide record of what was already asked since this app start, shared
 * by onboarding and [StartupPermissionGate] so nothing is asked twice and the
 * gate runs at most once per process.
 */
object StartupPermissionLedger {
    private val runtime: MutableSet<String> = ConcurrentHashMap.newKeySet()
    private val special: MutableSet<SpecialAccess> = ConcurrentHashMap.newKeySet()
    private val gateRan = AtomicBoolean(false)

    fun markRuntimeAsked(names: Collection<String>) {
        runtime.addAll(names)
    }

    fun markSpecialAsked(access: SpecialAccess) {
        special.add(access)
    }

    fun askedRuntime(): Set<String> = runtime.toSet()

    fun askedSpecial(): Set<SpecialAccess> = special.toSet()

    /** True exactly once per process: the caller owns this app start's gate run. */
    fun claimGateRun(): Boolean = gateRan.compareAndSet(false, true)

    internal fun resetForTest() {
        runtime.clear()
        special.clear()
        gateRan.set(false)
    }
}

/**
 * Asks for every permission the app uses, once per app start, when the
 * signed-in UI starts: one runtime sheet first, then one short screen per
 * missing special grant, each with Continue / Skip. Anything onboarding
 * already asked in this process is left out.
 */
@Composable
fun StartupPermissionGate() {
    val context = LocalContext.current
    // Comma-joined SpecialAccess names: survives rotation (a String is Bundle-saveable).
    var pending by rememberSaveable { mutableStateOf("") }

    fun queueSpecial() {
        pending = StartupPermissionPlan.specialToShow(
            sdkInt = Build.VERSION.SDK_INT,
            status = specialAccessStatus(context),
            alreadyAsked = StartupPermissionLedger.askedSpecial(),
        ).joinToString(",") { it.name }
    }

    val runtimeLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestMultiplePermissions(),
    ) { queueSpecial() }

    LaunchedEffect(Unit) {
        if (!StartupPermissionLedger.claimGateRun()) return@LaunchedEffect
        val missing = StartupPermissionPlan.runtimeToRequest(
            sdkInt = Build.VERSION.SDK_INT,
            isGranted = { isGranted(context, it) },
            alreadyAsked = StartupPermissionLedger.askedRuntime(),
        )
        if (missing.isNotEmpty()) {
            StartupPermissionLedger.markRuntimeAsked(missing)
            runtimeLauncher.launch(missing.toTypedArray())
        } else {
            queueSpecial()
        }
    }

    val current = pending.split(',').firstOrNull { it.isNotEmpty() }
        ?.let { name -> SpecialAccess.entries.firstOrNull { it.name == name } }
        ?: return

    fun advance() {
        StartupPermissionLedger.markSpecialAsked(current)
        pending = pending.split(',').filter { it.isNotEmpty() && it != current.name }.joinToString(",")
    }

    val (title, body) = when (current) {
        SpecialAccess.BATTERY -> R.string.perm_battery_title to R.string.perm_battery_body
        SpecialAccess.INSTALL_PACKAGES -> R.string.perm_install_title to R.string.perm_install_body
        SpecialAccess.FULL_SCREEN_INTENT -> R.string.perm_fullscreen_title to R.string.perm_fullscreen_body
        SpecialAccess.OVERLAY -> R.string.perm_overlay_title to R.string.perm_overlay_body
    }
    SpecialPermissionDialog(
        title = stringResource(title),
        body = stringResource(body),
        onContinue = {
            openSpecialSettings(context, current)
            advance()
        },
        onSkip = { advance() },
    )
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

/** Kept for existing callers/tests: the runtime permission list for [sdkInt]. */
internal fun runtimePermissionNames(sdkInt: Int): List<String> =
    StartupPermissionPlan.runtimePermissions(sdkInt)

private fun isGranted(context: Context, permission: String): Boolean =
    ContextCompat.checkSelfPermission(context, permission) == PackageManager.PERMISSION_GRANTED

private fun specialAccessStatus(context: Context): SpecialAccessStatus {
    val pm = context.getSystemService(Context.POWER_SERVICE) as PowerManager
    val canFullScreen = if (Build.VERSION.SDK_INT >= 34) {
        context.getSystemService(NotificationManager::class.java)?.canUseFullScreenIntent() == true
    } else {
        true
    }
    return SpecialAccessStatus(
        batteryOptimizationIgnored = pm.isIgnoringBatteryOptimizations(context.packageName),
        canInstallPackages = context.packageManager.canRequestPackageInstalls(),
        canUseFullScreenIntent = canFullScreen,
        canDrawOverlays = Settings.canDrawOverlays(context),
    )
}

private fun openSpecialSettings(context: Context, access: SpecialAccess) {
    val packageUri = Uri.parse("package:${context.packageName}")
    val action = when (access) {
        SpecialAccess.BATTERY -> Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS
        SpecialAccess.INSTALL_PACKAGES -> Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES
        SpecialAccess.FULL_SCREEN_INTENT -> Settings.ACTION_MANAGE_APP_USE_FULL_SCREEN_INTENT
        SpecialAccess.OVERLAY -> Settings.ACTION_MANAGE_OVERLAY_PERMISSION
    }
    try {
        context.startActivity(Intent(action, packageUri).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
    } catch (e: ActivityNotFoundException) {
        // Some OEM builds ship no handler for a given screen; the grant stays optional.
        LNLog.w(LogCategory.GENERAL, "StartupPermissionGate", "no settings screen for $access", e)
    }
}
