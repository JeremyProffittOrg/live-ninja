package ninja.jeremy.liveninja.ui.screens

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.LazyListScope
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.repeatOnLifecycle
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonPrimitive
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.net.*
import ninja.jeremy.liveninja.ui.SETTINGS_TAB_SIZE
import ninja.jeremy.liveninja.ui.jobs.*

@Composable
fun GhostExplorerScreen(onBack: () -> Unit) {
    val vm: GhostExplorerViewModel = hiltViewModel()
    val state by vm.state.collectAsStateWithLifecycle()
    val lifecycle = LocalLifecycleOwner.current.lifecycle
    LaunchedEffect(vm, lifecycle) { lifecycle.repeatOnLifecycle(Lifecycle.State.STARTED) {
        vm.onForeground()
        while (isActive) { delay(15_000); vm.poll() }
    } }
    GhostExplorerContent(state, vm::handle, onBack)
}

@Composable
fun GhostExplorerContent(state: GhostExplorerState, onEvent: (GhostExplorerEvent) -> Unit, onBack: () -> Unit) {
    androidx.activity.compose.BackHandler { if (state.nodeId != null || state.archive.scope != null) onEvent(GhostExplorerEvent.Back) else onBack() }
    Scaffold(Modifier.fillMaxSize().testTag("ghost-explorer")) { padding ->
        Column(Modifier.fillMaxSize().padding(padding)) {
            Row(Modifier.fillMaxWidth().heightIn(min = SETTINGS_TAB_SIZE * 2).padding(start = SETTINGS_TAB_SIZE + 8.dp, end = 8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                TextButton(onClick = { if (state.nodeId != null || state.archive.scope != null) onEvent(GhostExplorerEvent.Back) else onBack() }) { Text(stringResource(R.string.ghost_back)) }
                Text(stringResource(R.string.ghost_title), style = MaterialTheme.typography.titleLarge, modifier = Modifier.weight(1f).padding(top = 12.dp).semantics { heading() })
                TextButton(onClick = { onEvent(GhostExplorerEvent.Refresh) }, enabled = !state.loading && !state.sessionsLoading && !state.archive.loading && !state.denied) { Text(stringResource(R.string.jobs_refresh)) }
            }
            HorizontalDivider()
            LazyColumn(Modifier.fillMaxSize().testTag("ghost-content"), contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                item("boundary") {
                    Text(stringResource(R.string.ghost_owner_boundary), style = MaterialTheme.typography.bodySmall)
                    if (state.loading || state.sessionsLoading || state.archive.loading) LinearProgressIndicator(Modifier.fillMaxWidth().padding(top = 12.dp))
                }
                val error = state.archive.errorCode ?: state.errorCode
                if (error != null) item("error") { Text(stringResource(ghostErrorLabel(error)), color = MaterialTheme.colorScheme.error) }
                if (!state.denied) when {
                    state.archive.scope != null -> archiveItems(state.archive, onEvent)
                    state.nodeId != null -> {
                        item("selected-node") { Text(state.nodeId, style = MaterialTheme.typography.headlineSmall); Text(stringResource(R.string.ghost_choose_session)) }
                        items(state.sessions, key = { it.session_id }) { session ->
                            OutlinedButton(onClick = { onEvent(GhostExplorerEvent.Session(session.session_id)) }, modifier = Modifier.fillMaxWidth()) {
                                Column(Modifier.fillMaxWidth()) { Text(session.session_id); session.date?.let { Text(stringResource(R.string.ghost_upload_partition, it), style = MaterialTheme.typography.bodySmall) } }
                            }
                        }
                        if (state.sessionsCursor != null) item("more-sessions") { Button(onClick = { onEvent(GhostExplorerEvent.MoreSessions) }, enabled = !state.sessionsLoading) { Text(stringResource(R.string.ghost_more_sessions)) } }
                        if (!state.sessionsLoading && state.sessions.isEmpty() && state.errorCode == null) item("no-sessions") { Text(stringResource(R.string.ghost_empty_archive)) }
                    }
                    else -> {
                        item("nodes-title") { Text(stringResource(R.string.ghost_nodes), style = MaterialTheme.typography.titleLarge, modifier = Modifier.semantics { heading() }) }
                        items(state.nodes, key = { "node-${it.node_id}" }) { node ->
                            OutlinedButton(onClick = { onEvent(GhostExplorerEvent.Node(node.node_id)) }, modifier = Modifier.fillMaxWidth()) {
                                Column(Modifier.fillMaxWidth()) { Text(node.node_id); Text(node.state ?: node.status ?: stringResource(R.string.jobs_unknown), style = MaterialTheme.typography.bodySmall) }
                            }
                        }
                        item("jobs-title") {
                            Text(stringResource(R.string.ghost_scheduled_jobs), style = MaterialTheme.typography.titleLarge, modifier = Modifier.semantics { heading() })
                            Text(stringResource(R.string.ghost_loaded_jobs, state.jobs.size), style = MaterialTheme.typography.bodySmall)
                            Text(stringResource(R.string.ghost_run_limit, state.runHistoryLimit ?: 10), style = MaterialTheme.typography.bodySmall)
                        }
                        items(state.jobs, key = { "scheduled-${it.event_id}" }) { GhostScheduledCard(it) }
                        if (state.loaded && state.nodes.isEmpty() && state.jobs.isEmpty()) item("empty-inventory") { Text(stringResource(R.string.ghost_no_inventory)) }
                    }
                }
            }
        }
    }
}

@Composable private fun GhostScheduledCard(job: GhostScheduledEventDto) {
    var expanded by rememberSaveable(job.event_id) { mutableStateOf(false) }
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(job.repo?.takeIf(String::isNotBlank) ?: job.event_id, style = MaterialTheme.typography.titleMedium)
            Text(job.event_id, style = MaterialTheme.typography.bodySmall)
            Text("${job.node.orEmpty()} · ${job.last_run_status ?: stringResource(R.string.jobs_unknown)}")
            Text(stringResource(if (job.archived == true) R.string.ghost_archived else if (job.enabled == true) R.string.ghost_enabled else R.string.ghost_disabled))
            job.next_run?.let { Text(stringResource(R.string.ghost_provider_next, providerValue(it)), style = MaterialTheme.typography.bodySmall) }
            TextButton(onClick = { expanded = !expanded }) { Text(stringResource(R.string.ghost_inspect_job)) }
            if (expanded) SelectionContainer { Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text(stringResource(R.string.ghost_no_binding))
                job.prompt?.let { Text(it) }
                job.cron?.let { Text("${stringResource(R.string.ghost_schedule)}: $it · ${job.timezone.orEmpty()}") }
                job.run_at?.let { Text("${stringResource(R.string.ghost_schedule)}: ${providerValue(it)}") }
                job.runs.orEmpty().forEach { run ->
                    HorizontalDivider(); Text("${run.run_id} · ${run.status ?: stringResource(R.string.jobs_unknown)}")
                    run.ts?.let { Text(providerValue(it), style = MaterialTheme.typography.bodySmall) }
                    run.summary?.let { Text(it) }
                }
            } }
        }
    }
}

private fun LazyListScope.archiveItems(archive: GhostArchiveState, onEvent: (GhostExplorerEvent) -> Unit) {
    item("archive-title") {
        Text(stringResource(R.string.ghost_archive_title), style = MaterialTheme.typography.headlineSmall, modifier = Modifier.semantics { heading() })
        Text("${archive.scope?.nodeId}\n${archive.scope?.providerSessionId}", style = MaterialTheme.typography.bodySmall)
        Text(stringResource(R.string.ghost_retention), style = MaterialTheme.typography.bodySmall, modifier = Modifier.padding(vertical = 8.dp))
        OutlinedButton(onClick = { onEvent(GhostExplorerEvent.Rescan) }, enabled = !archive.loading) { Text(stringResource(R.string.ghost_rescan)) }
        archive.gaps.forEach { Text(stringResource(R.string.ghost_gap, it), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
    }
    archive.events.forEach { event ->
        item("event-${event.id}") {
            Card(Modifier.fillMaxWidth().testTag("ghost-event-${event.id}")) {
                Column(Modifier.padding(12.dp)) {
                    Text(event.kind.replace('_', ' '), style = MaterialTheme.typography.labelLarge)
                    Text(event.timestamp ?: stringResource(R.string.ghost_time_unknown), style = MaterialTheme.typography.bodySmall)
                    event.name?.let { Text(it, style = MaterialTheme.typography.labelMedium) }
                    event.call_id?.let { Text(stringResource(R.string.ghost_call_id, it), style = MaterialTheme.typography.bodySmall) }
                    if (event.part > 0 || event.more) Text(stringResource(if (event.more) R.string.ghost_fragment_more else R.string.ghost_fragment_end, event.part + 1), style = MaterialTheme.typography.bodySmall)
                }
            }
        }
        itemsIndexed(historyTextChunks(event.text), key = { index, _ -> "ghost-${event.id}-part-$index" }) { _, chunk -> SelectionContainer { Text(chunk, modifier = Modifier.fillMaxWidth().padding(horizontal = 8.dp)) } }
    }
    if (archive.nextCursor != null) item("more-events") { Button(onClick = { onEvent(GhostExplorerEvent.MoreEvents) }, enabled = !archive.loading && !archive.rescanRequired) { Text(stringResource(R.string.ghost_more_history)) } }
    if (archive.loaded && !archive.loading && archive.nextCursor == null && archive.errorCode == null && !archive.rescanRequired) item("loaded-end") { Text(stringResource(if (archive.events.isEmpty()) R.string.ghost_empty_archive else R.string.ghost_loaded_end), style = MaterialTheme.typography.bodySmall) }
}

private fun providerValue(value: JsonElement) = (value as? JsonPrimitive)?.content ?: value.toString()
internal fun ghostErrorLabel(code: Int) = when (code) {
    401, 403 -> R.string.ghost_access_denied
    409 -> R.string.ghost_changed_archive
    410 -> R.string.ghost_missing_archive
    413 -> R.string.ghost_large_archive
    422 -> R.string.ghost_invalid_archive
    503 -> R.string.ghost_unavailable
    else -> R.string.ghost_read_error
}
