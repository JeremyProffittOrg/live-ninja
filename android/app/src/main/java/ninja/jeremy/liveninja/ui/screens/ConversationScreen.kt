package ninja.jeremy.liveninja.ui.screens

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
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
import androidx.compose.material3.FilterChip
import androidx.compose.material3.FilledIconButton
import androidx.compose.material3.FilledTonalIconButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButtonDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.clip
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.pluralStringResource
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
import ninja.jeremy.liveninja.realtime.badgeText
import ninja.jeremy.liveninja.ui.SETTINGS_TAB_SIZE
import ninja.jeremy.liveninja.wake.WakeWordService
import ninja.jeremy.liveninja.ui.conversation.ConversationError
import ninja.jeremy.liveninja.ui.conversation.ConversationUiState
import ninja.jeremy.liveninja.ui.conversation.ConversationViewModel
import ninja.jeremy.liveninja.ui.conversation.MicUiState
import ninja.jeremy.liveninja.ui.conversation.PeerPresence
import ninja.jeremy.liveninja.ui.conversation.TranscriptTurn
import ninja.jeremy.liveninja.ui.state.TranscriptRole
import ninja.jeremy.liveninja.ui.theme.HalOrb
import ninja.jeremy.liveninja.ui.theme.OrbState

/** Test tags for the top-row controls added on 2026-08-01 (owner request). */
const val CONVERSATION_STATE_PILL_TAG = "conversation-state-pill"
const val CONVERSATION_COST_BADGE_TAG = "conversation-cost-badge"
const val CONVERSATION_NEW_BUTTON_TAG = "conversation-new"

/** Test tag for the cross-device roster added on 2026-08-02 (§6 WS-5 M5.1). */
const val CONVERSATION_PEER_ROSTER_TAG = "conversation-peer-roster"

/** Test tag for one mic-pickup chip ("low" | "medium" | "high"). */
fun micPickupChipTag(level: String): String = "conversation-mic-pickup-$level"

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
        ConversationUtilityBar(
            micEagerness = state.micEagerness,
            sessionLive = sessionLive(state.micState),
            onNewConversation = viewModel::startNewConversation,
            onSetMicEagerness = viewModel::setMicEagerness,
        )
        state.sessionWarning?.let { warning ->
            SessionWarningBanner(
                message = warning,
                onDismiss = viewModel::dismissSessionWarning,
            )
        }

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

@Composable
private fun SessionWarningBanner(
    message: String,
    onDismiss: () -> Unit,
) {
    Surface(color = MaterialTheme.colorScheme.tertiaryContainer) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 16.dp, vertical = 8.dp)
                .semantics { liveRegion = LiveRegionMode.Polite },
            horizontalArrangement = Arrangement.spacedBy(12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(
                message,
                style = MaterialTheme.typography.bodyMedium,
                modifier = Modifier.weight(1f),
            )
            TextButton(onClick = onDismiss) {
                Text(stringResource(R.string.conversation_warning_dismiss))
            }
        }
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

/**
 * The account's other live devices, under the cost badge — the same corner the
 * web client puts its roster in (§6 WS-5 M5.1).
 *
 * This is the visible half of the presence registry. Without it the presence
 * traffic is real but invisible, and a user who hears one device answer has no
 * way to tell that the other one deliberately stayed quiet from "the other one
 * is broken". That distinction is the entire point of the turn-taking rail.
 *
 * An empty roster draws NOTHING — not "no other devices". A user with a single
 * device should never be shown an empty fleet panel.
 *
 * The list is a live region only in the aggregate: the per-line text changes on
 * every peer transition, and announcing each one would talk over the assistant.
 * TalkBack gets one sentence for the whole block instead.
 */
@Composable
private fun PeerRoster(peers: List<PeerPresence>) {
    if (peers.isEmpty()) return

    val rosterA11y = pluralStringResource(
        R.plurals.conversation_peer_roster_a11y,
        peers.size,
        peers.size,
    )
    Column(
        horizontalAlignment = Alignment.End,
        modifier = Modifier
            .testTag(CONVERSATION_PEER_ROSTER_TAG)
            .semantics { contentDescription = rosterA11y },
    ) {
        peers.forEach { peer ->
            val stateWord = when (peer.state) {
                "connecting" -> stringResource(R.string.conversation_peer_state_connecting)
                "listening" -> stringResource(R.string.conversation_peer_state_listening)
                "thinking" -> stringResource(R.string.conversation_peer_state_thinking)
                "speaking" -> stringResource(R.string.conversation_peer_state_speaking)
                // Anything else — including a state a future web build invents —
                // reads as ready rather than leaking a protocol token to the user.
                else -> stringResource(R.string.conversation_peer_state_idle)
            }
            val persona = peer.persona.ifBlank {
                stringResource(R.string.conversation_peer_default_persona)
            }
            Text(
                stringResource(R.string.conversation_peer_line, persona, stateWord),
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

/**
 * Top bar of the conversation screen — deliberately the same three things, in
 * the same two corners, as the web client's rail top row (owner 2026-08-01:
 * "I also need conversation cost in the upper right and a listening bubble
 * like in the website on the app").
 *
 *   start gutter — [SETTINGS_TAB_SIZE], reserved for the settings tab that
 *                  sits in the upper-LEFT corner. Without it the tab paints
 *                  over this row instead of beside it.
 *   leading      — the state pill (web's .state-pill): a rounded "bubble"
 *                  carrying a dot and the current state word.
 *   trailing     — the running cost estimate above the session timer.
 *
 * The cost shows whenever there IS an estimate, not only while the session is
 * live: the number the user most wants after a conversation is what the
 * conversation cost, and blanking it the instant the session ends is what made
 * it look like the app had no cost display at all. It is still absent (rather
 * than "$0.000") when no estimate exists — showing a zero for an engine that
 * reports no usage would be a lie, not a zero.
 */
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
    val pillColor = when (state.micState) {
        MicUiState.LISTENING -> MaterialTheme.colorScheme.primaryContainer
        MicUiState.SPEAKING -> MaterialTheme.colorScheme.tertiaryContainer
        MicUiState.ERROR -> MaterialTheme.colorScheme.errorContainer
        else -> MaterialTheme.colorScheme.surfaceVariant
    }
    val pillInk = when (state.micState) {
        MicUiState.LISTENING -> MaterialTheme.colorScheme.onPrimaryContainer
        MicUiState.SPEAKING -> MaterialTheme.colorScheme.onTertiaryContainer
        MicUiState.ERROR -> MaterialTheme.colorScheme.onErrorContainer
        else -> MaterialTheme.colorScheme.onSurfaceVariant
    }
    // The dot pulses while a session is live, at web's cadence: slow when
    // listening, fast when speaking. It is decoration, not the signal — the
    // word beside it is (house a11y rule: never state by colour or motion
    // alone) — so it is driven by the same infinite transition either way and
    // simply holds still when nothing is live.
    val pulse = rememberInfiniteTransition(label = "state-dot")
    val dotAlpha by pulse.animateFloat(
        initialValue = 1f,
        targetValue = when (state.micState) {
            MicUiState.LISTENING -> 0.25f
            MicUiState.SPEAKING -> 0.25f
            else -> 1f
        },
        animationSpec = infiniteRepeatable(
            animation = tween(
                durationMillis = if (state.micState == MicUiState.SPEAKING) 350 else 1000,
            ),
            repeatMode = RepeatMode.Reverse,
        ),
        label = "state-dot-alpha",
    )

    Surface(color = MaterialTheme.colorScheme.surface) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                // Upper-LEFT corner belongs to the settings tab (SettingsDrawer
                // draws it over this screen); this is the gutter that keeps the
                // pill out from under it.
                .padding(start = SETTINGS_TAB_SIZE + 8.dp, top = 8.dp, end = 16.dp, bottom = 8.dp)
                // Announce state transitions to TalkBack without stealing focus.
                .semantics { liveRegion = LiveRegionMode.Polite },
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Surface(
                color = pillColor,
                contentColor = pillInk,
                shape = CircleShape,
                modifier = Modifier.testTag(CONVERSATION_STATE_PILL_TAG),
            ) {
                Row(
                    modifier = Modifier.padding(horizontal = 12.dp, vertical = 6.dp),
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Box(
                        Modifier
                            .size(9.dp)
                            .alpha(dotAlpha)
                            .background(pillInk, CircleShape),
                    )
                    Text(label, style = MaterialTheme.typography.labelLarge)
                }
            }

            Column(horizontalAlignment = Alignment.End) {
                state.sessionCost?.takeIf { it.hasData }?.let { cost ->
                    // Never signal by position alone: spell out what the number
                    // is for a screen reader, including that it is an estimate
                    // and not a bill. Resolved outside semantics{}, which is not
                    // a composable scope.
                    val costA11y = stringResource(
                        R.string.conversation_cost_badge_a11y,
                        cost.badgeText(),
                        cost.textTokens,
                        cost.audioTokens,
                    )
                    Text(
                        cost.badgeText(),
                        style = MaterialTheme.typography.labelMedium,
                        modifier = Modifier
                            .testTag(CONVERSATION_COST_BADGE_TAG)
                            .semantics { contentDescription = costA11y },
                    )
                }
                if (sessionLive(state.micState)) {
                    val minutes = state.sessionSeconds / 60
                    val seconds = state.sessionSeconds % 60
                    Text(
                        stringResource(R.string.conversation_session_timer, minutes, seconds),
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                PeerRoster(state.peers)
            }
        }
    }
}

/**
 * New conversation + mic pickup, on one line under the state row (owner
 * 2026-08-01: "I also don't see the audio settings (mic sensitivity) or the new
 * conversation button anywhere"). Both existed on web and had no equivalent
 * here; this is web's rail cluster condensed to the two controls that were
 * missing.
 *
 * Mic pickup is persisted, not applied live: the server reads `micEagerness`
 * out of the settings document when it mints the next session, so while one is
 * up the caption says exactly that rather than implying the change already
 * landed.
 */
@Composable
private fun ConversationUtilityBar(
    micEagerness: String,
    sessionLive: Boolean,
    onNewConversation: () -> Unit,
    onSetMicEagerness: (String) -> Unit,
) {
    val levels = listOf(
        "low" to R.string.conversation_mic_pickup_low,
        "medium" to R.string.conversation_mic_pickup_medium,
        "high" to R.string.conversation_mic_pickup_high,
    )
    val groupLabel = stringResource(R.string.conversation_mic_pickup_label)
    Column(Modifier.fillMaxWidth()) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .padding(start = SETTINGS_TAB_SIZE + 8.dp, end = 16.dp, bottom = 4.dp),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            OutlinedButton(
                onClick = onNewConversation,
                modifier = Modifier
                    .heightIn(min = 48.dp)
                    .testTag(CONVERSATION_NEW_BUTTON_TAG),
            ) { Text(stringResource(R.string.conversation_new)) }

            Row(
                horizontalArrangement = Arrangement.spacedBy(6.dp),
                verticalAlignment = Alignment.CenterVertically,
                modifier = Modifier.semantics { contentDescription = groupLabel },
            ) {
                levels.forEach { (value, labelRes) ->
                    // "auto" is the untouched default and has no chip: with none
                    // selected the server keeps the API's own eagerness. Tapping
                    // the selected chip again returns to it.
                    val selected = micEagerness == value
                    FilterChip(
                        selected = selected,
                        onClick = { onSetMicEagerness(if (selected) "auto" else value) },
                        label = { Text(stringResource(labelRes)) },
                        modifier = Modifier.testTag(micPickupChipTag(value)),
                    )
                }
            }
        }
        if (sessionLive) {
            Text(
                stringResource(R.string.conversation_mic_pickup_next_session),
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(
                    start = SETTINGS_TAB_SIZE + 8.dp,
                    end = 16.dp,
                    bottom = 4.dp,
                ),
            )
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
    val context = LocalContext.current
    val wakeRunning by WakeWordService.runningFlow.collectAsStateWithLifecycle()
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
                val orbCd = stringResource(R.string.conversation_orb_cd)
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
                            contentDescription = orbCd
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
                // Stop listening, reachable from where the user actually is. The wake FGS
                // notification has a Stop action, but it is PRIORITY_LOW/CATEGORY_SERVICE and
                // is not visible while the app is open — which left Settings as the only
                // in-app way to stop the microphone. Shown only when something really is
                // listening (runningFlow, not the persisted intent), so it never offers to
                // turn off something that is already off.
                if (wakeRunning) {
                    Text(
                        stringResource(R.string.conversation_listening_on),
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        textAlign = TextAlign.Center,
                        modifier = Modifier.padding(top = 16.dp),
                    )
                    OutlinedButton(
                        onClick = { WakeWordService.stop(context) },
                        modifier = Modifier
                            .padding(top = 8.dp)
                            .heightIn(min = 48.dp),
                    ) { Text(stringResource(R.string.conversation_listening_stop)) }
                }
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
    val micIdleCd = stringResource(R.string.conversation_mic_button_cd)
    val micLiveCd = stringResource(R.string.conversation_mic_button_live_cd)
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
                            contentDescription = if (live) micLiveCd else micIdleCd
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
