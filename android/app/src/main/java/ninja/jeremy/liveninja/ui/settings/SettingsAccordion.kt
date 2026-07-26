package ninja.jeremy.liveninja.ui.settings

import androidx.annotation.StringRes
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.KeyboardArrowDown
import androidx.compose.material.icons.filled.KeyboardArrowUp
import androidx.compose.material3.Card
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.input.key.KeyEvent
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.semantics.stateDescription
import androidx.compose.ui.unit.dp
import ninja.jeremy.liveninja.R

/**
 * M30's shared web/Android settings order. The enum order is deliberate: it
 * drives visual order and hardware-key header navigation.
 */
enum class SettingsSection(
    @StringRes val titleRes: Int,
    @StringRes val descriptionRes: Int,
    /** Backend section id; null means actions-only rather than configurable. */
    val apiId: String?,
) {
    ABOUT_YOU(
        R.string.settings_section_about_you,
        R.string.settings_section_about_you_desc,
        "aboutYou",
    ),
    WAKE_WORD(
        R.string.settings_section_wake,
        R.string.settings_section_wake_desc,
        "wakeWord",
    ),
    PERSONA(
        R.string.settings_section_persona,
        R.string.settings_section_persona_desc,
        "persona",
    ),
    VOICE_ENGINE(
        R.string.settings_section_voice_engine,
        R.string.settings_section_voice_engine_desc,
        "voiceEngine",
    ),
    TURN_DETECTION(
        R.string.settings_section_turn_detection,
        R.string.settings_section_turn_detection_desc,
        "turnDetection",
    ),
    APPEARANCE(
        R.string.settings_section_appearance,
        R.string.settings_section_appearance_desc,
        "appearance",
    ),
    MICROPHONE(
        R.string.settings_section_microphone,
        R.string.settings_section_microphone_desc,
        "microphone",
    ),
    PRIVACY(
        R.string.settings_section_privacy,
        R.string.settings_section_privacy_desc,
        "privacy",
    ),
    ACCOUNT(
        R.string.settings_section_account,
        R.string.settings_section_account_desc,
        null,
    ),
}

/** M30's initial current-page state. */
val DEFAULT_EXPANDED_SETTINGS_SECTION: SettingsSection = SettingsSection.ABOUT_YOU

/** About you is the initial panel; activating the open panel collapses all. */
fun toggledSettingsSection(
    current: SettingsSection?,
    activated: SettingsSection,
): SettingsSection? = if (current == activated) null else activated

/**
 * Pending profile suggestions always take the user to their counted work.
 * Otherwise drawer open/close preserves the current-page accordion state.
 */
fun settingsSectionOnOpen(
    current: SettingsSection?,
    hasPendingProfileSuggestions: Boolean,
): SettingsSection? =
    if (hasPendingProfileSuggestions) SettingsSection.ABOUT_YOU else current

/** Directional movement supported while an accordion header owns focus. */
enum class SettingsHeaderMove {
    NEXT,
    PREVIOUS,
    FIRST,
    LAST,
}

/** Resolve wrapped header navigation independently of Compose key dispatch. */
fun targetSettingsHeaderIndex(
    currentIndex: Int,
    sectionCount: Int,
    move: SettingsHeaderMove,
): Int {
    require(sectionCount > 0) { "sectionCount must be positive" }
    require(currentIndex in 0 until sectionCount) {
        "currentIndex must identify a settings section"
    }
    return when (move) {
        SettingsHeaderMove.NEXT -> (currentIndex + 1) % sectionCount
        SettingsHeaderMove.PREVIOUS -> (currentIndex - 1 + sectionCount) % sectionCount
        SettingsHeaderMove.FIRST -> 0
        SettingsHeaderMove.LAST -> sectionCount - 1
    }
}

/**
 * One accessible accordion header and its conditionally-composed panel.
 *
 * Omitting collapsed content is intentional: its controls cannot receive
 * focus or be announced, and expensive settings sections retain M22.1's lazy
 * composition win. The header exposes button role plus an explicit expanded
 * state description; [onHeaderKeyEvent] supplies wrapped Up/Down/Home/End
 * navigation from the owning list.
 */
@Composable
internal fun SettingsAccordionCard(
    title: String,
    description: String,
    expanded: Boolean,
    expandedStateDescription: String,
    collapsedStateDescription: String,
    expandActionLabel: String,
    collapseActionLabel: String,
    focusRequester: FocusRequester,
    onToggle: () -> Unit,
    onHeaderKeyEvent: (KeyEvent) -> Boolean,
    modifier: Modifier = Modifier,
    content: @Composable () -> Unit,
) {
    Card(modifier = modifier.fillMaxWidth()) {
        Column {
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = 64.dp)
                    .focusRequester(focusRequester)
                    .onPreviewKeyEvent(onHeaderKeyEvent)
                    .clickable(
                        role = Role.Button,
                        onClickLabel = if (expanded) {
                            collapseActionLabel
                        } else {
                            expandActionLabel
                        },
                        onClick = onToggle,
                    )
                    .semantics(mergeDescendants = true) {
                        heading()
                        role = Role.Button
                        stateDescription = if (expanded) {
                            expandedStateDescription
                        } else {
                            collapsedStateDescription
                        }
                    }
                    .padding(horizontal = 16.dp, vertical = 12.dp),
                horizontalArrangement = Arrangement.spacedBy(16.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Column(
                    modifier = Modifier.weight(1f),
                    verticalArrangement = Arrangement.spacedBy(2.dp),
                ) {
                    Text(title, style = MaterialTheme.typography.titleMedium)
                    Text(
                        description,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                Icon(
                    imageVector = if (expanded) {
                        Icons.Filled.KeyboardArrowUp
                    } else {
                        Icons.Filled.KeyboardArrowDown
                    },
                    contentDescription = null,
                )
            }

            if (expanded) {
                HorizontalDivider()
                Column(
                    modifier = Modifier.padding(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    content()
                }
            }
        }
    }
}
