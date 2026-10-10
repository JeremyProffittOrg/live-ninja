package ninja.jeremy.liveninja.ui.settings

import android.content.ActivityNotFoundException
import android.content.Context
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.annotation.StringRes
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.realtime.CarAudioFailure
import ninja.jeremy.liveninja.realtime.CarAudioPreferences
import ninja.jeremy.liveninja.realtime.CarAudioSessionManager
import ninja.jeremy.liveninja.realtime.RealtimeSessionCoordinator

/*
 * Local, per-phone opt-in for phone-based car audio (full assistant over the
 * car's Bluetooth hands-free call audio; NOT Android Auto projection).
 *
 * The pure decision seam below (display state, toggle decision, permission
 * result decision, [CarAudioModeController]) carries every rule and is what
 * CarAudioSettingsTest exercises. The view model only wires it to the shared
 * singletons: [CarAudioPreferences] (the one source of truth, also read by the
 * coordinator at session start), [RealtimeSessionCoordinator.sessionBusy] for
 * display, and [RealtimeSessionCoordinator.tryChangeCarAudioMode] for the one
 * atomic, idle-only commit. Nothing here starts a session, scans, pairs, or
 * opens system settings.
 */

enum class CarAudioStatus { REMOTE, BUSY, OFF, NEEDS_PERMISSION, UNAVAILABLE, READY }

data class CarAudioControlDisplay(
    val checked: Boolean,
    val toggleEnabled: Boolean,
    val allowPermissionsVisible: Boolean,
    val status: CarAudioStatus,
    val unavailableReason: CarAudioFailure?,
)

fun carAudioControlDisplay(
    enabled: Boolean,
    localControls: Boolean,
    busy: Boolean,
    requestInFlight: Boolean,
    missingPermissions: List<String>,
    readiness: CarAudioFailure?,
): CarAudioControlDisplay {
    val status = when {
        !localControls -> CarAudioStatus.REMOTE
        busy -> CarAudioStatus.BUSY
        !enabled -> CarAudioStatus.OFF
        missingPermissions.isNotEmpty() -> CarAudioStatus.NEEDS_PERMISSION
        readiness != null -> CarAudioStatus.UNAVAILABLE
        else -> CarAudioStatus.READY
    }
    return CarAudioControlDisplay(
        checked = enabled,
        toggleEnabled = localControls && !busy && !requestInFlight,
        allowPermissionsVisible = status == CarAudioStatus.NEEDS_PERMISSION && !requestInFlight,
        status = status,
        unavailableReason = readiness.takeIf { status == CarAudioStatus.UNAVAILABLE },
    )
}

fun missingCarAudioPermissions(required: List<String>, isGranted: (String) -> Boolean): List<String> =
    required.filterNot(isGranted)

sealed interface CarAudioToggleDecision {
    object NoChange : CarAudioToggleDecision
    object RejectRemote : CarAudioToggleDecision
    object RejectBusy : CarAudioToggleDecision
    object Enable : CarAudioToggleDecision
    object Disable : CarAudioToggleDecision
    data class RequestPermissions(val permissions: List<String>) : CarAudioToggleDecision
}

fun decideCarAudioToggle(
    requestedOn: Boolean,
    currentlyEnabled: Boolean,
    localControls: Boolean,
    busy: Boolean,
    requestInFlight: Boolean,
    missingPermissions: List<String>,
): CarAudioToggleDecision = when {
    !localControls -> CarAudioToggleDecision.RejectRemote
    busy -> CarAudioToggleDecision.RejectBusy
    requestInFlight -> CarAudioToggleDecision.NoChange
    requestedOn == currentlyEnabled -> CarAudioToggleDecision.NoChange
    !requestedOn -> CarAudioToggleDecision.Disable
    missingPermissions.isNotEmpty() -> CarAudioToggleDecision.RequestPermissions(missingPermissions)
    else -> CarAudioToggleDecision.Enable
}

enum class CarAudioPermissionOutcome { ENABLE, LEAVE_OFF_DENIED, REJECT_BUSY, REJECT_REMOTE, REFRESH_ONLY }

/** Evaluated when the permission result arrives, against the state at THAT moment. */
fun decideCarAudioPermissionResult(
    pendingEnable: Boolean,
    localControls: Boolean,
    busy: Boolean,
    missingPermissions: List<String>,
): CarAudioPermissionOutcome = when {
    !pendingEnable -> CarAudioPermissionOutcome.REFRESH_ONLY
    !localControls -> CarAudioPermissionOutcome.REJECT_REMOTE
    busy -> CarAudioPermissionOutcome.REJECT_BUSY
    missingPermissions.isNotEmpty() -> CarAudioPermissionOutcome.LEAVE_OFF_DENIED
    else -> CarAudioPermissionOutcome.ENABLE
}

/**
 * Mode-change orchestration over injected reads and one atomic commit. Its
 * only side effect is [tryCommit], which must check idle and write the mode
 * as one indivisible step (in production: the coordinator's session gate) and
 * return false without writing when a session is starting or live. [isBusy]
 * is used only for the early decision; a session that began during a
 * permission prompt or between decision and commit can never switch modes.
 */
class CarAudioModeController(
    private val isBusy: () -> Boolean,
    private val isEnabled: () -> Boolean,
    private val tryCommit: (Boolean) -> Boolean,
    private val missingPermissions: () -> List<String>,
) {
    var permissionRequestInFlight: Boolean = false
        private set
    private var pendingEnable = false

    fun onToggle(requestedOn: Boolean, localControls: Boolean): CarAudioToggleDecision {
        val decision = decideCarAudioToggle(
            requestedOn = requestedOn,
            currentlyEnabled = isEnabled(),
            localControls = localControls,
            busy = isBusy(),
            requestInFlight = permissionRequestInFlight,
            missingPermissions = missingPermissions(),
        )
        return when (decision) {
            CarAudioToggleDecision.Enable ->
                if (tryCommit(true)) decision else CarAudioToggleDecision.RejectBusy
            CarAudioToggleDecision.Disable ->
                if (tryCommit(false)) decision else CarAudioToggleDecision.RejectBusy
            is CarAudioToggleDecision.RequestPermissions -> {
                permissionRequestInFlight = true
                pendingEnable = true
                decision
            }
            else -> decision
        }
    }

    /** Explicit Allow for an already-on mode whose permission was revoked. Never changes the mode. */
    fun onAllowPermissions(localControls: Boolean): List<String>? {
        if (!localControls || permissionRequestInFlight) return null
        val missing = missingPermissions()
        if (missing.isEmpty()) return null
        permissionRequestInFlight = true
        pendingEnable = false
        return missing
    }

    fun onPermissionResult(localControls: Boolean): CarAudioPermissionOutcome {
        val pending = permissionRequestInFlight && pendingEnable
        permissionRequestInFlight = false
        pendingEnable = false
        val outcome = decideCarAudioPermissionResult(pending, localControls, isBusy(), missingPermissions())
        if (outcome == CarAudioPermissionOutcome.ENABLE && !tryCommit(true)) {
            return CarAudioPermissionOutcome.REJECT_BUSY
        }
        return outcome
    }
}

enum class CarAudioNotice(@StringRes val messageRes: Int, val isError: Boolean) {
    ENABLED(R.string.car_audio_msg_enabled, false),
    DISABLED(R.string.car_audio_msg_disabled, false),
    BUSY(R.string.car_audio_msg_busy, true),
    REMOTE(R.string.car_audio_msg_remote, true),
    DENIED(R.string.car_audio_msg_denied, true),
    GRANTED(R.string.car_audio_msg_granted, false),
    STILL_MISSING(R.string.car_audio_msg_still_missing, true),
}

data class CarAudioUiState(
    val enabled: Boolean = false,
    val busy: Boolean = false,
    val missingPermissions: List<String> = emptyList(),
    val readiness: CarAudioFailure? = null,
    val requestInFlight: Boolean = false,
    val message: CarAudioNotice? = null,
)

private data class CarAudioEnvironment(
    val missingPermissions: List<String>,
    val readiness: CarAudioFailure?,
)

private data class CarAudioTransient(
    val requestInFlight: Boolean = false,
    val message: CarAudioNotice? = null,
)

@HiltViewModel
class CarAudioSettingsViewModel @Inject constructor(
    @ApplicationContext private val appContext: Context,
    private val preferences: CarAudioPreferences,
    private val sessionManager: CarAudioSessionManager,
    private val coordinator: RealtimeSessionCoordinator,
) : ViewModel() {
    private val environment = MutableStateFlow(probeEnvironment())
    private val transient = MutableStateFlow(CarAudioTransient())
    private val controller = CarAudioModeController(
        isBusy = { coordinator.sessionBusy.value },
        isEnabled = { preferences.isEnabled },
        // setEnabled runs INSIDE the coordinator's session gate, atomically
        // with its idle check; never a separate check-then-write here.
        tryCommit = { coordinator.tryChangeCarAudioMode(it) },
        missingPermissions = { missingPermissions() },
    )

    val state: StateFlow<CarAudioUiState> = combine(
        preferences.enabled,
        coordinator.sessionBusy,
        environment,
        transient,
    ) { enabled, busy, env, t ->
        CarAudioUiState(enabled, busy, env.missingPermissions, env.readiness, t.requestInFlight, t.message)
    }.stateIn(
        viewModelScope,
        SharingStarted.Eagerly,
        CarAudioUiState(
            enabled = preferences.isEnabled,
            busy = coordinator.sessionBusy.value,
            missingPermissions = environment.value.missingPermissions,
            readiness = environment.value.readiness,
        ),
    )

    init {
        // A session holds call-audio mode while busy; re-probe once it ends.
        viewModelScope.launch {
            coordinator.sessionBusy.collect { busy -> if (!busy) refresh() }
        }
    }

    /** Re-reads actual permissions and readiness. Never prompts. */
    fun refresh() {
        environment.value = probeEnvironment()
    }

    /** Returns permissions to request from an explicit user toggle, or null. */
    fun onToggle(requestedOn: Boolean, localControls: Boolean): List<String>? {
        refresh()
        val decision = controller.onToggle(requestedOn, localControls)
        val notice = when (decision) {
            CarAudioToggleDecision.Enable -> CarAudioNotice.ENABLED
            CarAudioToggleDecision.Disable -> CarAudioNotice.DISABLED
            CarAudioToggleDecision.RejectBusy -> CarAudioNotice.BUSY
            CarAudioToggleDecision.RejectRemote -> CarAudioNotice.REMOTE
            CarAudioToggleDecision.NoChange -> transient.value.message
            is CarAudioToggleDecision.RequestPermissions -> null
        }
        transient.value = CarAudioTransient(controller.permissionRequestInFlight, notice)
        return (decision as? CarAudioToggleDecision.RequestPermissions)?.permissions
    }

    fun onAllowPermissions(localControls: Boolean): List<String>? {
        refresh()
        val request = controller.onAllowPermissions(localControls)
        transient.value = CarAudioTransient(
            controller.permissionRequestInFlight,
            if (!localControls) CarAudioNotice.REMOTE else null,
        )
        return request
    }

    fun onPermissionResult(localControls: Boolean) {
        refresh()
        val outcome = controller.onPermissionResult(localControls)
        val notice = when (outcome) {
            CarAudioPermissionOutcome.ENABLE -> CarAudioNotice.ENABLED
            CarAudioPermissionOutcome.LEAVE_OFF_DENIED -> CarAudioNotice.DENIED
            CarAudioPermissionOutcome.REJECT_BUSY -> CarAudioNotice.BUSY
            CarAudioPermissionOutcome.REJECT_REMOTE -> CarAudioNotice.REMOTE
            CarAudioPermissionOutcome.REFRESH_ONLY ->
                if (environment.value.missingPermissions.isEmpty()) CarAudioNotice.GRANTED
                else CarAudioNotice.STILL_MISSING
        }
        transient.value = CarAudioTransient(controller.permissionRequestInFlight, notice)
        refresh()
    }

    private fun missingPermissions(): List<String> =
        missingCarAudioPermissions(CarAudioPreferences.requiredRuntimePermissions()) { permission ->
            ContextCompat.checkSelfPermission(appContext, permission) == PackageManager.PERMISSION_GRANTED
        }

    private fun probeEnvironment(): CarAudioEnvironment {
        val readiness = try {
            sessionManager.readiness()
        } catch (e: RuntimeException) {
            CarAudioFailure.ROUTE_FAILED
        }
        return CarAudioEnvironment(missingPermissions(), readiness)
    }
}

@StringRes
internal fun carAudioStatusRes(display: CarAudioControlDisplay): Int = when (display.status) {
    CarAudioStatus.REMOTE -> R.string.car_audio_status_remote
    CarAudioStatus.BUSY -> R.string.car_audio_status_busy
    CarAudioStatus.OFF -> R.string.car_audio_status_off
    CarAudioStatus.NEEDS_PERMISSION -> R.string.car_audio_status_needs_permission
    CarAudioStatus.READY -> R.string.car_audio_status_ready
    CarAudioStatus.UNAVAILABLE -> when (display.unavailableReason) {
        CarAudioFailure.NO_DEVICE -> R.string.car_audio_status_no_device
        CarAudioFailure.CALL_ACTIVE -> R.string.car_audio_status_call_active
        else -> R.string.car_audio_status_unavailable
    }
}

@Composable
private fun RefreshCarAudioOnResume(onResume: () -> Unit) {
    val lifecycleOwner = LocalLifecycleOwner.current
    val latest by rememberUpdatedState(onResume)
    DisposableEffect(lifecycleOwner) {
        val observer = LifecycleEventObserver { _, event ->
            if (event == Lifecycle.Event.ON_RESUME) latest()
        }
        lifecycleOwner.lifecycle.addObserver(observer)
        onDispose { lifecycleOwner.lifecycle.removeObserver(observer) }
    }
}

/** Settings → Microphone: local car-audio opt-in, gated to this phone. */
@Composable
fun CarAudioSettings(
    localControlsEnabled: Boolean,
    modifier: Modifier = Modifier,
    viewModel: CarAudioSettingsViewModel = hiltViewModel(),
) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    val latestLocalControls by rememberUpdatedState(localControlsEnabled)
    val permissionLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestMultiplePermissions(),
    ) {
        // The grant map is not trusted: the view model re-checks the actual
        // permissions, the viewed scope and the session state right now.
        viewModel.onPermissionResult(latestLocalControls)
    }
    RefreshCarAudioOnResume(viewModel::refresh)

    val display = carAudioControlDisplay(
        enabled = state.enabled,
        localControls = localControlsEnabled,
        busy = state.busy,
        requestInFlight = state.requestInFlight,
        missingPermissions = state.missingPermissions,
        readiness = state.readiness,
    )
    val requestPermissions: (List<String>?) -> Unit = { permissions ->
        if (!permissions.isNullOrEmpty()) {
            try {
                permissionLauncher.launch(permissions.toTypedArray())
            } catch (e: ActivityNotFoundException) {
                viewModel.onPermissionResult(latestLocalControls)
            }
        }
    }
    val warning = display.status == CarAudioStatus.NEEDS_PERMISSION ||
        display.status == CarAudioStatus.UNAVAILABLE

    Column(
        modifier = modifier.fillMaxWidth(),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        Text(
            stringResource(R.string.car_audio_title),
            style = MaterialTheme.typography.titleSmall,
            color = MaterialTheme.colorScheme.primary,
            modifier = Modifier.padding(top = 8.dp),
        )
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 48.dp)
                .toggleable(
                    value = display.checked,
                    enabled = display.toggleEnabled,
                    role = Role.Switch,
                    onValueChange = { on ->
                        requestPermissions(viewModel.onToggle(on, localControlsEnabled))
                    },
                )
                .alpha(if (display.toggleEnabled) 1f else 0.5f),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(Modifier.weight(1f)) {
                Text(
                    stringResource(R.string.car_audio_switch_label),
                    style = MaterialTheme.typography.bodyLarge,
                )
                Text(
                    stringResource(R.string.car_audio_switch_desc),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Spacer(Modifier.width(8.dp))
            Switch(checked = display.checked, onCheckedChange = null, enabled = display.toggleEnabled)
        }
        listOf(
            R.string.car_audio_explainer_route,
            R.string.car_audio_explainer_dashboard,
            R.string.car_audio_explainer_interruptions,
            R.string.car_audio_explainer_permissions,
        ).forEach { res ->
            Text(
                stringResource(res),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Text(
            stringResource(carAudioStatusRes(display)),
            style = MaterialTheme.typography.bodyMedium,
            color = if (warning) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite },
        )
        if (display.allowPermissionsVisible) {
            OutlinedButton(
                onClick = { requestPermissions(viewModel.onAllowPermissions(localControlsEnabled)) },
                modifier = Modifier.heightIn(min = 48.dp),
            ) { Text(stringResource(R.string.car_audio_allow)) }
        }
        state.message?.let { notice ->
            Text(
                stringResource(notice.messageRes),
                style = MaterialTheme.typography.bodySmall,
                color = if (notice.isError) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.primary,
                modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite },
            )
        }
    }
}

/**
 * Compact, honest mode indicator for the conversation screen. Shows only the
 * selected mode and whether it can start; it never claims audio is routed.
 */
@Composable
fun CarAudioModeIndicator(
    modifier: Modifier = Modifier,
    viewModel: CarAudioSettingsViewModel = hiltViewModel(),
) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    RefreshCarAudioOnResume(viewModel::refresh)
    if (!state.enabled) return
    val display = carAudioControlDisplay(
        enabled = true,
        localControls = true,
        busy = state.busy,
        requestInFlight = false,
        missingPermissions = state.missingPermissions,
        readiness = state.readiness,
    )
    val res = when (display.status) {
        CarAudioStatus.NEEDS_PERMISSION -> R.string.car_audio_indicator_needs_permission
        CarAudioStatus.UNAVAILABLE ->
            if (display.unavailableReason == CarAudioFailure.NO_DEVICE) R.string.car_audio_indicator_no_device
            else R.string.car_audio_indicator_unavailable
        else -> R.string.car_audio_indicator_on
    }
    Surface(
        color = MaterialTheme.colorScheme.secondaryContainer,
        shape = RoundedCornerShape(8.dp),
        modifier = modifier.fillMaxWidth(),
    ) {
        Text(
            stringResource(res),
            style = MaterialTheme.typography.labelMedium,
            modifier = Modifier
                .padding(horizontal = 12.dp, vertical = 6.dp)
                .semantics { liveRegion = LiveRegionMode.Polite },
        )
    }
}
