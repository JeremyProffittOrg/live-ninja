package ninja.jeremy.liveninja.ui.screens

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.clickable
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Build
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.MicOff
import androidx.compose.material.icons.filled.Stop
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.FilledIconButton
import androidx.compose.material3.FilledTonalIconButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButtonDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.hilt.navigation.compose.hiltViewModel
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.ui.conversation.ConversationError
import ninja.jeremy.liveninja.ui.conversation.ConversationUiState
import ninja.jeremy.liveninja.ui.conversation.ConversationViewModel
import ninja.jeremy.liveninja.ui.conversation.MicUiState
import ninja.jeremy.liveninja.ui.conversation.TranscriptTurn
import ninja.jeremy.liveninja.ui.state.TranscriptRole
import ninja.jeremy.liveninja.ui.theme.HalOrb
import ninja.jeremy.liveninja.ui.theme.OrbState

/**
 * Conversation tab (mockups/android/05-home-idle + 06-conversation): live
 * transcript bubbles, mic state indicator, push-to-talk/interrupt control,
 * and the barge-in visual. The view model is activity-scoped so the session
 * survives tab switches and drives the background overlay bubble.
 */
@Composable
fun ConversationScreen(modifier: Modifier = Modifier) {
    val context = LocalContext.current
    val activity = context as ComponentActivity
    val viewModel: ConversationViewModel = hiltViewModel(activity)
    val state by viewModel.state.collectAsState()

    val micPermissionLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted -> viewModel.onMicPermissionResult(granted) }

    fun startOrRequestMic() {
        val granted = ContextCompat.checkSelfPermission(
            context, Manifest.permission.RECORD_AUDIO,
        ) == PackageManager.PERMISSION_GRANTED
        if (granted) {
            viewModel.startSession()
        } else {
            viewModel.onRequestingMicPermission()
            micPermissionLauncher.launch(Manifest.permission.RECORD_AUDIO)
        }
    }

    Column(modifier = modifier.fillMaxSize()) {
        MicStateBanner(state)

        if (state.transcript.isEmpty() && !sessionLive(state.micState)) {
            IdleHero(
                state = state,
                onTapToTalk = ::startOrRequestMic,
                onAcknowledgeError = viewModel::acknowledgeError,
                modifier = Modifier.weight(1f),
            )
        } else {
            Column(
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth(),
            ) {
                // Session-live orb: shrinks to a compact indicator pinned above
                // the transcript (mockup 06 pattern).
                HalOrb(
                    state = micToOrbState(state.micState),
                    modifier = Modifier
                        .padding(top = 12.dp, bottom = 4.dp)
                        .size(104.dp)
                        .align(Alignment.CenterHorizontally),
                )
                TranscriptList(
                    turns = state.transcript,
                    modifier = Modifier
                        .weight(1f)
                        .fillMaxWidth(),
                )
            }
        }

        if (state.bargeInFlash) {
            BargeInVisual()
        }

        ControlBar(
            state = state,
            onPrimary = {
                when (state.micState) {
                    MicUiState.IDLE, MicUiState.ERROR -> startOrRequestMic()
                    MicUiState.LISTENING, MicUiState.SPEAKING -> viewModel.interruptAndListen()
                    else -> Unit
                }
            },
            onMute = viewModel::toggleMute,
            onStop = viewModel::endSession,
        )
    }
}

private fun sessionLive(micState: MicUiState): Boolean =
    micState in setOf(MicUiState.CONNECTING, MicUiState.LISTENING, MicUiState.SPEAKING, MicUiState.ENDING)

/**
 * Map the conversation mic state onto a HAL orb visual state (03-theme
 * Placement). This mapping intentionally lives in the screen, not the view
 * model, so the theme's presentation concerns stay out of the domain layer.
 */
private fun micToOrbState(micState: MicUiState): OrbState = when (micState) {
    MicUiState.IDLE -> OrbState.IDLE
    MicUiState.REQUESTING_MIC -> OrbState.IDLE
    MicUiState.CONNECTING -> OrbState.THINKING
    MicUiState.LISTENING -> OrbState.LISTENING
    MicUiState.SPEAKING -> OrbState.SPEAKING
    MicUiState.ENDING -> OrbState.IDLE
    MicUiState.ERROR -> OrbState.ERROR
}

@Composable
private fun MicStateBanner(state: ConversationUiState) {
    val label = when (state.micState) {
        MicUiState.IDLE -> stringResource(R.string.conversation_state_idle)
        MicUiState.REQUESTING_MIC -> stringResource(R.string.conversation_state_requesting_mic)
        MicUiState.CONNECTING -> stringResource(R.string.conversation_state_connecting)
        MicUiState.LISTENING ->
            if (state.micMuted) {
                stringResource(R.string.conversation_state_muted)
            } else {
                stringResource(R.string.conversation_state_listening)
            }
        MicUiState.SPEAKING -> stringResource(R.string.conversation_state_speaking)
        MicUiState.ENDING -> stringResource(R.string.conversation_state_ending)
        MicUiState.ERROR -> stringResource(R.string.conversation_state_error)
    }
    val color = when (state.micState) {
        MicUiState.LISTENING -> MaterialTheme.colorScheme.primaryContainer
        MicUiState.SPEAKING -> MaterialTheme.colorScheme.tertiaryContainer
        MicUiState.ERROR -> MaterialTheme.colorScheme.errorContainer
        else -> MaterialTheme.colorScheme.surfaceVariant
    }
    Surface(color = color) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 16.dp, vertical = 8.dp)
                // Announce state transitions to TalkBack without stealing focus.
                .semantics { liveRegion = LiveRegionMode.Polite },
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(label, style = MaterialTheme.typography.labelLarge)
            if (sessionLive(state.micState)) {
                val minutes = state.sessionSeconds / 60
                val seconds = state.sessionSeconds % 60
                Text(
                    stringResource(R.string.conversation_session_timer, minutes, seconds),
                    style = MaterialTheme.typography.labelMedium,
                )
            }
        }
    }
}

@Composable
private fun IdleHero(
    state: ConversationUiState,
    onTapToTalk: () -> Unit,
    onAcknowledgeError: () -> Unit,
    modifier: Modifier = Modifier,
) {
    Column(
        modifier = modifier
            .fillMaxWidth()
            .padding(24.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        when {
            state.micState == MicUiState.ERROR && state.error != null -> {
                val (title, body) = when (state.error) {
                    ConversationError.ENGINE_NOT_WIRED ->
                        stringResource(R.string.conversation_error_not_wired_title) to
                            stringResource(R.string.conversation_error_not_wired_body)
                    ConversationError.MIC_DENIED ->
                        stringResource(R.string.conversation_error_mic_denied_title) to
                            stringResource(R.string.conversation_error_mic_denied_body)
                    ConversationError.SESSION_FAILED ->
                        stringResource(R.string.conversation_error_session_title) to
                            (state.errorDetail
                                ?: stringResource(R.string.conversation_error_session_body))
                }
                Card(
                    colors = CardDefaults.cardColors(
                        containerColor = MaterialTheme.colorScheme.errorContainer,
                    ),
                ) {
                    Column(
                        Modifier.padding(16.dp),
                        verticalArrangement = Arrangement.spacedBy(8.dp),
                    ) {
                        Text(title, style = MaterialTheme.typography.titleMedium)
                        Text(body, style = MaterialTheme.typography.bodyMedium)
                        Button(
                            onClick = onAcknowledgeError,
                            modifier = Modifier.heightIn(min = 48.dp),
                        ) { Text(stringResource(R.string.conversation_error_dismiss)) }
                    }
                }
            }

            state.micState == MicUiState.CONNECTING || state.micState == MicUiState.REQUESTING_MIC -> {
                CircularProgressIndicator(modifier = Modifier.size(48.dp))
                Text(
                    stringResource(R.string.conversation_state_connecting),
                    style = MaterialTheme.typography.bodyLarge,
                    modifier = Modifier.padding(top = 16.dp),
                )
            }

            else -> {
                // Persistent HAL orb (200dp) as the idle tap-to-talk affordance;
                // the eye is decorative, the whole 200dp circle is the tap target
                // (well over the 48dp minimum) and the caption below labels it.
                HalOrb(
                    state = micToOrbState(state.micState),
                    modifier = Modifier
                        .size(200.dp)
                        .clip(CircleShape)
                        .clickable(
                            onClickLabel = "Start a live voice conversation",
                            role = Role.Button,
                            onClick = onTapToTalk,
                        )
                        .semantics {
                            contentDescription = "Tap to talk. Starts a live voice conversation."
                            role = Role.Button
                        },
                )
                Text(
                    stringResource(R.string.conversation_tap_to_talk),
                    style = MaterialTheme.typography.headlineSmall,
                    textAlign = TextAlign.Center,
                    modifier = Modifier.padding(top = 20.dp),
                )
                Text(
                    stringResource(R.string.conversation_idle_wake_caption, state.wakePhraseLabel),
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    textAlign = TextAlign.Center,
                    modifier = Modifier.padding(top = 8.dp),
                )
                Text(
                    stringResource(R.string.conversation_idle_privacy_caption),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    textAlign = TextAlign.Center,
                    modifier = Modifier.padding(top = 4.dp),
                )
            }
        }
    }
}

@Composable
private fun TranscriptList(
    turns: List<TranscriptTurn>,
    modifier: Modifier = Modifier,
) {
    val listState = rememberLazyListState()
    // Follow the newest turn as text streams in.
    LaunchedEffect(turns.size, turns.lastOrNull()?.text?.length) {
        if (turns.isNotEmpty()) {
            listState.animateScrollToItem(turns.lastIndex)
        }
    }
    LazyColumn(
        state = listState,
        modifier = modifier.semantics { contentDescription = "Conversation transcript" },
        contentPadding = PaddingValues(16.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        items(turns, key = { it.id + (it.toolName ?: "") }) { turn ->
            if (turn.isToolCall) {
                ToolCallChip(turn)
            } else {
                TranscriptBubble(turn)
            }
        }
    }
}

@Composable
private fun TranscriptBubble(turn: TranscriptTurn) {
    val isUser = turn.role == TranscriptRole.USER
    Row(
        modifier = Modifier.fillMaxWidth(),
        horizontalArrangement = if (isUser) Arrangement.End else Arrangement.Start,
    ) {
        Surface(
            color = if (isUser) {
                MaterialTheme.colorScheme.primaryContainer
            } else {
                MaterialTheme.colorScheme.surfaceVariant
            },
            shape = RoundedCornerShape(
                topStart = 16.dp,
                topEnd = 16.dp,
                bottomStart = if (isUser) 16.dp else 4.dp,
                bottomEnd = if (isUser) 4.dp else 16.dp,
            ),
            modifier = Modifier
                .widthIn(max = 320.dp)
                .semantics {
                    contentDescription =
                        (if (isUser) "You said: " else "Live Ninja said: ") + turn.text
                },
        ) {
            Column(Modifier.padding(horizontal = 14.dp, vertical = 10.dp)) {
                Text(
                    if (isUser) {
                        stringResource(R.string.conversation_role_you)
                    } else {
                        stringResource(R.string.conversation_role_assistant)
                    },
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Text(turn.text, style = MaterialTheme.typography.bodyLarge)
            }
        }
    }
}

@Composable
private fun ToolCallChip(turn: TranscriptTurn) {
    Card(
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.secondaryContainer,
        ),
    ) {
        Row(
            Modifier.padding(12.dp),
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Icon(
                Icons.Filled.Build,
                contentDescription = null,
                modifier = Modifier.size(20.dp),
                tint = MaterialTheme.colorScheme.onSecondaryContainer,
            )
            Column {
                Text(
                    stringResource(R.string.conversation_tool_call, turn.toolName.orEmpty()),
                    style = MaterialTheme.typography.labelMedium,
                )
                turn.toolSummary?.takeIf { it.isNotBlank() }?.let {
                    Text(it, style = MaterialTheme.typography.bodySmall)
                }
            }
        }
    }
}

@Composable
private fun BargeInVisual() {
    Surface(color = MaterialTheme.colorScheme.tertiaryContainer) {
        Text(
            stringResource(R.string.conversation_barge_in),
            style = MaterialTheme.typography.labelLarge,
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 16.dp, vertical = 8.dp)
                .semantics { liveRegion = LiveRegionMode.Polite },
            textAlign = TextAlign.Center,
        )
    }
}

@Composable
private fun ControlBar(
    state: ConversationUiState,
    onPrimary: () -> Unit,
    onMute: () -> Unit,
    onStop: () -> Unit,
) {
    val live = sessionLive(state.micState)
    Surface(color = MaterialTheme.colorScheme.surfaceContainerHigh) {
        Column(Modifier.fillMaxWidth()) {
            if (live) {
                Text(
                    stringResource(R.string.conversation_interrupt_hint),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    textAlign = TextAlign.Center,
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(top = 8.dp),
                )
            }
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(horizontal = 24.dp, vertical = 12.dp),
                horizontalArrangement = Arrangement.SpaceEvenly,
                verticalAlignment = Alignment.CenterVertically,
            ) {
                if (live) {
                    // Mute toggle.
                    FilledTonalIconButton(
                        onClick = onMute,
                        modifier = Modifier
                            .size(56.dp)
                            .semantics {
                                contentDescription =
                                    if (state.micMuted) "Unmute microphone" else "Mute microphone"
                            },
                    ) {
                        Icon(
                            if (state.micMuted) Icons.Filled.MicOff else Icons.Filled.Mic,
                            contentDescription = null,
                        )
                    }
                }

                // Primary: tap-to-talk when idle, push-to-talk/interrupt when live.
                FilledIconButton(
                    onClick = onPrimary,
                    enabled = state.micState != MicUiState.CONNECTING &&
                        state.micState != MicUiState.ENDING &&
                        state.micState != MicUiState.REQUESTING_MIC,
                    modifier = Modifier
                        .size(72.dp)
                        .semantics {
                            contentDescription = if (live) {
                                "Push to talk. Interrupts the assistant and listens."
                            } else {
                                "Tap to talk. Starts a live voice conversation."
                            }
                        },
                    shape = CircleShape,
                    colors = IconButtonDefaults.filledIconButtonColors(
                        containerColor = when (state.micState) {
                            MicUiState.SPEAKING -> MaterialTheme.colorScheme.tertiary
                            else -> MaterialTheme.colorScheme.primary
                        },
                    ),
                ) {
                    Icon(
                        Icons.Filled.Mic,
                        contentDescription = null,
                        modifier = Modifier.size(32.dp),
                    )
                }

                if (live) {
                    // End session.
                    FilledTonalIconButton(
                        onClick = onStop,
                        modifier = Modifier
                            .size(56.dp)
                            .semantics { contentDescription = "Stop and end the session" },
                        colors = IconButtonDefaults.filledTonalIconButtonColors(
                            containerColor = MaterialTheme.colorScheme.errorContainer,
                            contentColor = MaterialTheme.colorScheme.onErrorContainer,
                        ),
                    ) {
                        Icon(Icons.Filled.Stop, contentDescription = null)
                    }
                }
            }
        }
    }
}
