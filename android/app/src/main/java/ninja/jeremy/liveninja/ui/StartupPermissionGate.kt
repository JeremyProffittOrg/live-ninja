package ninja.jeremy.liveninja.ui

import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.platform.LocalContext
import java.util.concurrent.ConcurrentHashMap
import ninja.jeremy.liveninja.ui.permissions.AndroidPermissionHistory
import ninja.jeremy.liveninja.ui.permissions.PermissionCoordinator
import ninja.jeremy.liveninja.ui.permissions.PermissionGuideDialog

/** Retained for onboarding's disclosure tracking; these never launch settings automatically. */
enum class SpecialAccess { BATTERY, INSTALL_PACKAGES, FULL_SCREEN_INTENT, OVERLAY }

/** What onboarding offered this process, distinct from actual persisted permission requests. */
object StartupPermissionLedger {
    private val runtime: MutableSet<String> = ConcurrentHashMap.newKeySet()
    private val special: MutableSet<SpecialAccess> = ConcurrentHashMap.newKeySet()
    fun markRuntimeAsked(names: Collection<String>) { runtime.addAll(names) }
    fun markSpecialAsked(access: SpecialAccess) { special.add(access) }
    fun askedRuntime(): Set<String> = runtime.toSet()
    fun askedSpecial(): Set<SpecialAccess> = special.toSet()
    internal fun resetForTest() { runtime.clear(); special.clear() }
}

/**
 * A once-per-install explanation, not a grant gate. No runtime request or system settings
 * screen is opened by entering the signed-in app. Every request needs a feature-specific tap.
 * The guide can also be reopened from Settings; permission grants are always read from Android.
 */
@Composable
fun StartupPermissionGate() {
    val context = LocalContext.current
    val coordinator = remember(context) { PermissionCoordinator(AndroidPermissionHistory(context)) }
    var visible by rememberSaveable { mutableStateOf(false) }
    LaunchedEffect(coordinator) {
        if (coordinator.claimStartupGuide()) visible = true
    }
    if (visible) PermissionGuideDialog(onDismiss = { visible = false })
}
