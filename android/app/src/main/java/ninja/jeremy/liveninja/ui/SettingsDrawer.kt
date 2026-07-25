package ninja.jeremy.liveninja.ui

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxScope
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.requiredWidth
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.KeyboardArrowLeft
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.rotate
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import androidx.compose.ui.zIndex
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.ui.screens.SettingsScreen
import ninja.jeremy.liveninja.ui.settings.SettingsSection

internal const val SETTINGS_OPEN_BAR_TAG = "settings-open-bar"
internal const val SETTINGS_CLOSE_BAR_TAG = "settings-close-bar"

internal enum class SettingsEdge {
    OPEN,
    CLOSE,
}

/**
 * Matching M30 edge controls. fillMaxHeight(0.4f) is deliberately on the
 * control itself, not an approximate fixed dp size, so it tracks phones,
 * tablets, split-screen, and orientation changes exactly.
 */
@Composable
internal fun BoxScope.SettingsEdgeBar(
    edge: SettingsEdge,
    onClick: () -> Unit,
    focusRequester: FocusRequester,
) {
    val opening = edge == SettingsEdge.OPEN
    val visibleLabel = stringResource(
        if (opening) R.string.settings_edge_visible else R.string.settings_edge_close_visible,
    )
    val accessibleLabel = stringResource(
        if (opening) R.string.settings_edge_open else R.string.settings_edge_close,
    )
    val shape = if (opening) {
        RoundedCornerShape(topStart = 16.dp, bottomStart = 16.dp)
    } else {
        RoundedCornerShape(topEnd = 16.dp, bottomEnd = 16.dp)
    }

    Surface(
        onClick = onClick,
        shape = shape,
        color = MaterialTheme.colorScheme.surfaceContainerHigh,
        contentColor = MaterialTheme.colorScheme.onSurface,
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant),
        tonalElevation = 3.dp,
        modifier = Modifier
            .align(if (opening) Alignment.CenterEnd else Alignment.CenterStart)
            .fillMaxHeight(0.4f)
            .width(48.dp)
            .focusRequester(focusRequester)
            .zIndex(2f)
            .testTag(if (opening) SETTINGS_OPEN_BAR_TAG else SETTINGS_CLOSE_BAR_TAG)
            .semantics(mergeDescendants = true) {
                contentDescription = accessibleLabel
                role = Role.Button
            },
    ) {
        Column(
            horizontalAlignment = Alignment.CenterHorizontally,
            verticalArrangement = Arrangement.Center,
        ) {
            Icon(
                imageVector = if (opening) {
                    Icons.Filled.Settings
                } else {
                    Icons.AutoMirrored.Filled.KeyboardArrowLeft
                },
                contentDescription = null,
            )
            Text(
                text = visibleLabel,
                style = MaterialTheme.typography.labelLarge,
                maxLines = 1,
                modifier = Modifier
                    .padding(top = 44.dp)
                    .requiredWidth(96.dp)
                    .rotate(-90f),
            )
        }
    }
}

/** Full-window modal settings surface; the close bar is first in traversal. */
@Composable
internal fun SettingsModal(
    expandedSection: SettingsSection?,
    onExpandedSectionChange: (SettingsSection?) -> Unit,
    onClose: () -> Unit,
    onOpenLogViewer: () -> Unit,
) {
    val closeFocusRequester = remember { FocusRequester() }

    Dialog(
        onDismissRequest = onClose,
        properties = DialogProperties(
            dismissOnBackPress = true,
            dismissOnClickOutside = true,
            usePlatformDefaultWidth = false,
            decorFitsSystemWindows = false,
        ),
    ) {
        Surface(
            modifier = Modifier.fillMaxSize(),
            color = MaterialTheme.colorScheme.background,
        ) {
            Box(Modifier.fillMaxSize()) {
                // Emitted first so keyboard/TalkBack traversal begins here.
                SettingsEdgeBar(
                    edge = SettingsEdge.CLOSE,
                    onClick = onClose,
                    focusRequester = closeFocusRequester,
                )
                SettingsScreen(
                    modifier = Modifier
                        .fillMaxSize()
                        .padding(start = 48.dp),
                    onOpenLogViewer = onOpenLogViewer,
                    expandedSection = expandedSection,
                    onExpandedSectionChange = onExpandedSectionChange,
                )
            }
        }
    }

    LaunchedEffect(Unit) {
        closeFocusRequester.requestFocus()
    }
}
