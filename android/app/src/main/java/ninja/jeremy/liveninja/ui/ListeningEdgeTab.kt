package ninja.jeremy.liveninja.ui

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.BoxScope
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBars
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.MicOff
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.unit.dp
import androidx.compose.ui.zIndex
import androidx.core.content.ContextCompat
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import dagger.hilt.android.lifecycle.HiltViewModel
import javax.inject.Inject
import kotlinx.coroutines.flow.StateFlow
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.wake.WakePreferences
import ninja.jeremy.liveninja.wake.WakeSwitchAction
import ninja.jeremy.liveninja.wake.WakeSwitchDisplay
import ninja.jeremy.liveninja.wake.WakeWordService
import ninja.jeremy.liveninja.wake.decideWakeSwitchAction
import ninja.jeremy.liveninja.wake.wakeSwitchDisplay

internal const val LISTENING_TAB_TAG = "listening-tab"

/** Exposes the two wake facts the tab needs; the service itself is started from the composable. */
@HiltViewModel
class ListeningTabViewModel @Inject constructor(wakePrefs: WakePreferences) : ViewModel() {
    /** Persisted intent ("the user wants this on"). */
    val serviceEnabled: StateFlow<Boolean> = wakePrefs.serviceEnabledFlow

    /** Observed reality ("a service instance is alive"). */
    val serviceRunning: StateFlow<Boolean> = WakeWordService.runningFlow
}

/**
 * Always-listening on/off tab in the UPPER-LEFT corner, directly above the settings tab
 * (owner 2026-09-15: "I need an enable/disable feature in live ninja as well, in the upper
 * left above settings"). Same size and edge-hinged look as [SettingsEdgeBar], and the same
 * decision logic as the Settings switch ([wakeSwitchDisplay] / [decideWakeSwitchAction]):
 * it reflects whether something is actually listening, and a tap in the paused state resumes.
 *
 * Lives outside the Scaffold like the settings tab so it is reachable from every top-level
 * tab; [SettingsEdgeBar] is pushed down by [SETTINGS_TAB_SIZE] to make room.
 */
@Composable
internal fun BoxScope.ListeningEdgeTab() {
    val viewModel: ListeningTabViewModel = hiltViewModel()
    val enabled by viewModel.serviceEnabled.collectAsStateWithLifecycle()
    val running by viewModel.serviceRunning.collectAsStateWithLifecycle()
    val context = LocalContext.current
    val display = wakeSwitchDisplay(serviceEnabled = enabled, serviceRunning = running)

    val micLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted -> if (granted) WakeWordService.start(context) }

    val label = stringResource(
        when (display) {
            WakeSwitchDisplay.RUNNING -> R.string.listening_tab_on
            WakeSwitchDisplay.PAUSED -> R.string.listening_tab_paused
            WakeSwitchDisplay.OFF -> R.string.listening_tab_off
        },
    )
    val container = when (display) {
        WakeSwitchDisplay.RUNNING -> MaterialTheme.colorScheme.primaryContainer
        WakeSwitchDisplay.PAUSED -> MaterialTheme.colorScheme.tertiaryContainer
        WakeSwitchDisplay.OFF -> MaterialTheme.colorScheme.surfaceContainerHigh
    }
    val content = when (display) {
        WakeSwitchDisplay.RUNNING -> MaterialTheme.colorScheme.onPrimaryContainer
        WakeSwitchDisplay.PAUSED -> MaterialTheme.colorScheme.onTertiaryContainer
        WakeSwitchDisplay.OFF -> MaterialTheme.colorScheme.onSurfaceVariant
    }

    Surface(
        onClick = {
            val action = decideWakeSwitchAction(
                toggledOn = display != WakeSwitchDisplay.RUNNING,
                serviceEnabled = enabled,
                serviceRunning = running,
            )
            when (action) {
                WakeSwitchAction.START -> {
                    val micGranted = ContextCompat.checkSelfPermission(
                        context, Manifest.permission.RECORD_AUDIO,
                    ) == PackageManager.PERMISSION_GRANTED
                    if (micGranted) WakeWordService.start(context) else micLauncher.launch(Manifest.permission.RECORD_AUDIO)
                }
                WakeSwitchAction.STOP -> WakeWordService.stop(context)
            }
        },
        // Rounded on the outer top corner only: with the settings tab hinged right below it
        // (bottom corner rounded there), the pair reads as one tab on the screen edge.
        shape = RoundedCornerShape(topEnd = 16.dp),
        color = container,
        contentColor = content,
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant),
        tonalElevation = 3.dp,
        modifier = Modifier
            .align(Alignment.TopStart)
            .windowInsetsPadding(WindowInsets.statusBars)
            .size(SETTINGS_TAB_SIZE)
            .zIndex(2f)
            .testTag(LISTENING_TAB_TAG)
            .semantics(mergeDescendants = true) {
                contentDescription = label
                role = Role.Switch
            },
    ) {
        Column(
            horizontalAlignment = Alignment.CenterHorizontally,
            verticalArrangement = Arrangement.Center,
        ) {
            Icon(
                imageVector = if (display == WakeSwitchDisplay.RUNNING) Icons.Filled.Mic else Icons.Filled.MicOff,
                contentDescription = null,
            )
        }
    }
}
