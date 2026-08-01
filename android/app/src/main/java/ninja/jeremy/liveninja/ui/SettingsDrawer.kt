package ninja.jeremy.liveninja.ui

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxScope
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBars
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.KeyboardArrowLeft
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
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
 * Width of the settings tab, and the gutter any screen must reserve in the
 * corner it occupies so the tab never paints over content. [ConversationScreen]
 * uses it for exactly that.
 */
internal val SETTINGS_TAB_SIZE = 48.dp

/**
 * The settings tab: a small square in the UPPER-LEFT corner (owner 2026-08-01,
 * "settings needs to be a tab in the upper left side"), mirroring the web
 * client's 40px corner tab.
 *
 * It replaces a bar that was vertically centred on the RIGHT edge and 40% of
 * the screen tall. On a tablet that bar sat directly on top of the transcript's
 * right-hand column and clipped every user bubble behind it — the reported
 * "conversation is cut off". A corner tab can only ever overlap one corner, and
 * the screen underneath reserves exactly that corner ([SETTINGS_TAB_SIZE]).
 *
 * 48dp square keeps the tap target at the Material minimum even though the tab
 * no longer spans any fraction of the screen, and the label is now carried by
 * contentDescription alone — there is no room to draw a rotated word in a
 * square this size, and the gear is the universally understood glyph.
 */
@Composable
internal fun BoxScope.SettingsEdgeBar(
    edge: SettingsEdge,
    onClick: () -> Unit,
    focusRequester: FocusRequester,
) {
    val opening = edge == SettingsEdge.OPEN
    val accessibleLabel = stringResource(
        if (opening) R.string.settings_edge_open else R.string.settings_edge_close,
    )
    // Rounded on the two inner corners only, so it reads as a tab hinged on
    // the screen edge rather than a floating button.
    val shape = if (opening) {
        RoundedCornerShape(bottomEnd = 16.dp)
    } else {
        RoundedCornerShape(bottomStart = 16.dp)
    }

    Surface(
        onClick = onClick,
        shape = shape,
        color = MaterialTheme.colorScheme.surfaceContainerHigh,
        contentColor = MaterialTheme.colorScheme.onSurface,
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant),
        tonalElevation = 3.dp,
        modifier = Modifier
            // The close tab mirrors to the opposite corner so it can never
            // land on top of the opener it stands in for.
            .align(if (opening) Alignment.TopStart else Alignment.TopEnd)
            // The tab is drawn OUTSIDE the Scaffold (it has to float over the
            // screen), so nothing has applied the status-bar inset for it —
            // without this it lands under the system clock. Must precede
            // size(), so the inset pushes the 48dp square down rather than
            // eating into it.
            .windowInsetsPadding(WindowInsets.statusBars)
            .size(SETTINGS_TAB_SIZE)
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
                    // The close tab is in the upper RIGHT corner now, so that
                    // is the side the settings form keeps clear of.
                    modifier = Modifier
                        .fillMaxSize()
                        .padding(end = SETTINGS_TAB_SIZE),
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
