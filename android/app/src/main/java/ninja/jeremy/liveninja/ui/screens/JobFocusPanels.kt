package ninja.jeremy.liveninja.ui.screens

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyListScope
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.unit.dp
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.ui.jobs.*

/** Stable entry/chunk keys keep the reader in place when older or newer pages arrive. */
internal fun LazyListScope.jobTimeline(history: JobHistoryState, onEvent: (JobsEvent) -> Unit) {
    item("timeline-heading") {
        Text(stringResource(R.string.jobs_timeline), style = MaterialTheme.typography.titleLarge, modifier = Modifier.padding(top = 12.dp).semantics { heading() })
        Text(stringResource(R.string.jobs_provider_history_boundary), style = MaterialTheme.typography.bodySmall, modifier = Modifier.padding(vertical = 8.dp))
        history.retentionMessage?.let { Text(it, style = MaterialTheme.typography.bodySmall) }
        if (history.scope == null) Text(stringResource(R.string.jobs_history_not_connected))
        else Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            OutlinedButton(onClick = { onEvent(JobsEvent.OlderHistory) }, enabled = !history.loading && history.olderCursor != null) { Text(stringResource(R.string.jobs_history_older)) }
            OutlinedButton(onClick = { onEvent(JobsEvent.LatestHistory) }, enabled = !history.loading) { Text(stringResource(if (history.hasMoreNewer) R.string.jobs_history_more_updates else R.string.jobs_history_latest)) }
        }
        if (history.loading) LinearProgressIndicator(Modifier.fillMaxWidth())
        if (history.hasMoreNewer) Text(stringResource(R.string.jobs_history_backlog), style = MaterialTheme.typography.bodySmall)
        if (history.failed) Text(stringResource(R.string.jobs_history_read_error), color = MaterialTheme.colorScheme.error, modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite })
    }
    history.entries.forEach { entry ->
        item("history-heading-${entry.id}") {
            Card(Modifier.fillMaxWidth().testTag("history-${entry.id}")) {
                Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Text(stringResource(if (entry.role == "user") R.string.jobs_history_you else R.string.jobs_history_event), style = MaterialTheme.typography.labelLarge)
                    Text("${entry.kind.replace('_', ' ')} · ${jobDateLabel(entry.createdAt)}", style = MaterialTheme.typography.bodySmall)
                    entry.status?.let { Text(it.replace('_', ' '), style = MaterialTheme.typography.labelMedium) }
                }
            }
        }
        itemsIndexed(historyTextChunks(entry.text), key = { index, _ -> "history-${entry.id}-chunk-$index" }) { _, chunk ->
            SelectionContainer { Text(chunk, style = MaterialTheme.typography.bodyMedium, modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp)) }
        }
    }
    if (history.entries.isNotEmpty()) item("timeline-latest-control") {
        TextButton(onClick = { onEvent(JobsEvent.LatestHistory) }, enabled = !history.loading) { Text(stringResource(if (history.hasMoreNewer) R.string.jobs_history_more_updates else R.string.jobs_history_latest)) }
    }
}

internal fun voiceOperationLabel(operation: String): Int = when (operation) {
    "job_create" -> R.string.jobs_new
    "job_start" -> R.string.jobs_run_now
    "job_cancel" -> R.string.jobs_cancel_job
    "job_pause" -> R.string.jobs_pause
    "job_resume" -> R.string.jobs_resume
    "job_retry" -> R.string.jobs_retry
    "job_command" -> R.string.jobs_review_note
    else -> R.string.jobs_unknown
}

@Composable
internal fun JobsVoiceReviewDialog(proposal: JobsVoiceProposal, state: JobsUiState, onEvent: (JobsEvent) -> Unit) {
    AlertDialog(
        modifier = Modifier.testTag("jobs-voice-review"),
        onDismissRequest = { if (!state.busy) onEvent(JobsEvent.DismissVoice) },
        title = { Text(stringResource(R.string.jobs_voice_review_title), modifier = Modifier.semantics { heading() }) },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                Text(stringResource(R.string.jobs_voice_review_boundary))
                Text(stringResource(voiceOperationLabel(proposal.operation)), style = MaterialTheme.typography.titleMedium)
                val kind = proposal.input?.kind ?: proposal.run?.kind ?: proposal.job?.kind
                Text("${stringResource(R.string.jobs_field_type)}: ${stringResource(when (kind) { "reminder" -> R.string.jobs_reminder; "review" -> R.string.jobs_review; else -> R.string.jobs_unknown })}")
                SelectionContainer { Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text(proposal.input?.title ?: proposal.run?.title ?: proposal.job?.title.orEmpty(), style = MaterialTheme.typography.titleMedium)
                    Text(proposal.text ?: proposal.input?.instructions ?: proposal.run?.instructions ?: proposal.job?.instructions.orEmpty())
                    (proposal.input?.schedule ?: proposal.job?.schedule)?.let { Text(scheduleLabel(it)) }
                    proposal.jobId?.let { Text(stringResource(R.string.jobs_proposal_job_version, it, proposal.expectedVersion), style = MaterialTheme.typography.bodySmall) }
                    proposal.runId?.let { Text(stringResource(R.string.jobs_run_id, it), style = MaterialTheme.typography.bodySmall) }
                } }
                if (proposal.operation == "job_command") Text(stringResource(R.string.jobs_note_boundary))
                state.voiceError?.let { Text(stringResource(it), color = MaterialTheme.colorScheme.error, modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite }) }
                if (state.busy) LinearProgressIndicator(Modifier.fillMaxWidth())
            }
        },
        confirmButton = { Button(onClick = { onEvent(JobsEvent.ConfirmVoice) }, enabled = !state.busy && !state.voiceStale, modifier = Modifier.testTag("jobs-voice-confirm")) { Text(stringResource(R.string.jobs_confirm)) } },
        dismissButton = { TextButton(onClick = { onEvent(JobsEvent.DismissVoice) }, enabled = !state.busy) { Text(stringResource(R.string.jobs_go_back)) } },
    )
}
