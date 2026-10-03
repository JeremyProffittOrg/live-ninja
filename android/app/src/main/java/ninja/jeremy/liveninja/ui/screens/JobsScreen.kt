@file:OptIn(ExperimentalMaterial3Api::class, ExperimentalLayoutApi::class)

package ninja.jeremy.liveninja.ui.screens

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.ArrowBack
import androidx.compose.material.icons.outlined.Add
import androidx.compose.material.icons.outlined.Close
import androidx.compose.material.icons.outlined.Refresh
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DatePicker
import androidx.compose.material3.DatePickerDialog
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExposedDropdownMenuBox
import androidx.compose.material3.ExposedDropdownMenuDefaults
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.MenuAnchorType
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TimeInput
import androidx.compose.material3.rememberDatePickerState
import androidx.compose.material3.rememberTimePickerState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.repeatOnLifecycle
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.net.JobDto
import ninja.jeremy.liveninja.net.JobRunDto
import ninja.jeremy.liveninja.net.JobScheduleDto
import ninja.jeremy.liveninja.ui.SETTINGS_TAB_SIZE
import ninja.jeremy.liveninja.ui.jobs.JobConfirmation
import ninja.jeremy.liveninja.ui.jobs.JobDraft
import ninja.jeremy.liveninja.ui.jobs.JobsEvent
import ninja.jeremy.liveninja.ui.jobs.JobsUiState
import ninja.jeremy.liveninja.ui.jobs.JobsViewModel
import ninja.jeremy.liveninja.ui.jobs.jobDateLabel
import ninja.jeremy.liveninja.ui.jobs.jobStatusLabel
import java.time.DayOfWeek
import java.time.Instant
import java.time.LocalDate
import java.time.LocalTime
import java.time.ZoneId
import java.time.ZoneOffset
import java.time.format.TextStyle
import java.util.Locale

@Composable
fun JobsScreen(modifier: Modifier = Modifier) {
    val viewModel: JobsViewModel = hiltViewModel()
    val state by viewModel.state.collectAsStateWithLifecycle()
    val lifecycle = LocalLifecycleOwner.current.lifecycle
    LaunchedEffect(viewModel, lifecycle) {
        lifecycle.repeatOnLifecycle(Lifecycle.State.STARTED) {
            viewModel.loadIfNeeded()
            while (isActive) { delay(15_000); viewModel.refresh(quiet = true) }
        }
    }
    JobsContent(state, viewModel::handle, modifier)
}

/** Stateless surface permits Compose tests without replacing production authentication. */
@Composable
fun JobsContent(state: JobsUiState, onEvent: (JobsEvent) -> Unit, modifier: Modifier = Modifier) {
    BackHandler(enabled = state.selected != null && state.draft == null && state.confirmation == null) { onEvent(JobsEvent.CloseDetail) }
    Scaffold(modifier = modifier.fillMaxSize().testTag("jobs-screen")) { padding ->
        Column(Modifier.fillMaxSize().padding(padding)) {
            Row(
                Modifier.fillMaxWidth().heightIn(min = SETTINGS_TAB_SIZE * 2).padding(start = SETTINGS_TAB_SIZE + 8.dp, end = 8.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                if (state.selected != null) IconButton(onClick = { onEvent(JobsEvent.CloseDetail) }, enabled = !state.busy) {
                    Icon(Icons.AutoMirrored.Outlined.ArrowBack, stringResource(R.string.jobs_back))
                }
                Text(stringResource(R.string.jobs_title), style = MaterialTheme.typography.titleLarge, modifier = Modifier.weight(1f))
                IconButton(onClick = { onEvent(JobsEvent.Refresh) }, enabled = !state.loading && !state.busy) {
                    Icon(Icons.Outlined.Refresh, stringResource(R.string.jobs_refresh))
                }
                if (state.selected == null) IconButton(onClick = { onEvent(JobsEvent.New) }, enabled = !state.busy && (state.capabilities?.reminder == true || state.capabilities?.review == true)) {
                    Icon(Icons.Outlined.Add, stringResource(R.string.jobs_new))
                }
            }
            if (state.loading || state.busy) LinearProgressIndicator(Modifier.fillMaxWidth())
            HorizontalDivider()
            val job = state.selected
            if (job == null) JobsList(state, onEvent) else JobDetail(job, state, onEvent)
        }
    }
    state.draft?.let { JobEditorDialog(it, state, onEvent) }
    state.confirmation?.let { JobConfirmationDialog(it, state, onEvent) }
}

@Composable
private fun JobsList(state: JobsUiState, onEvent: (JobsEvent) -> Unit) {
    val rows = state.jobs.filter { state.filter == "all" || it.status == state.filter }
    LazyColumn(contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp), modifier = Modifier.fillMaxSize().testTag("jobs-list")) {
        item { Text(stringResource(R.string.jobs_subtitle), color = MaterialTheme.colorScheme.onSurfaceVariant) }
        item { CapabilityCard(state) }
        state.notice?.let { notice -> item { NoticeCard(notice, onDismiss = { onEvent(JobsEvent.DismissNotice) }) } }
        state.error?.let { error -> item { ErrorCard(error, onRetry = { onEvent(JobsEvent.Refresh) }) } }
        item {
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                listOf("all" to R.string.jobs_all, "active" to R.string.jobs_active, "paused" to R.string.jobs_paused, "cancelled" to R.string.jobs_cancelled).forEach { (value, label) ->
                    FilterChip(selected = state.filter == value, onClick = { onEvent(JobsEvent.Filter(value)) }, label = { Text(stringResource(label)) })
                }
            }
            Text(stringResource(R.string.jobs_shown, rows.size), style = MaterialTheme.typography.labelMedium)
        }
        if (state.loading && !state.loaded) item { Progress(R.string.jobs_loading) }
        else if (state.loaded && rows.isEmpty()) item {
            Card(Modifier.fillMaxWidth()) {
                Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                    Text(stringResource(R.string.jobs_empty), style = MaterialTheme.typography.titleMedium)
                    Text(stringResource(if (state.jobs.isEmpty()) R.string.jobs_empty_body else R.string.jobs_filtered_empty))
                    Button(onClick = { onEvent(JobsEvent.New) }, enabled = !state.busy && (state.capabilities?.reminder == true || state.capabilities?.review == true)) { Text(stringResource(R.string.jobs_new)) }
                }
            }
        }
        items(rows, key = { it.id }) { job ->
            Card(onClick = { onEvent(JobsEvent.Open(job)) }, enabled = !state.busy, modifier = Modifier.fillMaxWidth().testTag("job-${job.id}")) {
                Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Status(job.status)
                    Text(job.title, style = MaterialTheme.typography.titleMedium)
                    Text(stringResource(if (job.kind == "review") R.string.jobs_review else R.string.jobs_reminder), style = MaterialTheme.typography.labelMedium)
                    Text(scheduleLabel(job.schedule), style = MaterialTheme.typography.bodySmall)
                    Text(nextRunLabel(job), style = MaterialTheme.typography.bodySmall)
                    job.lastRun?.let { Text(stringResource(R.string.jobs_latest, stringResource(jobStatusLabel(it.status))), style = MaterialTheme.typography.labelMedium) }
                    job.error?.takeIf { it.isNotBlank() }?.let { Text(it, color = MaterialTheme.colorScheme.error) }
                }
            }
        }
        if (state.nextCursor != null) item { OutlinedButton(onClick = { onEvent(JobsEvent.More) }, enabled = !state.loadingMore && !state.loading && !state.busy, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.jobs_more)) } }
        if (state.loadingMore) item { Progress(R.string.jobs_loading) }
    }
}

@Composable
private fun CapabilityCard(state: JobsUiState) {
    var expanded by rememberSaveable { mutableStateOf(false) }
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(stringResource(when (state.capabilities?.scheduling) { true -> R.string.jobs_scheduler_ready; false -> R.string.jobs_scheduler_disabled; null -> R.string.jobs_unknown_capabilities }), style = MaterialTheme.typography.labelLarge)
            TextButton(onClick = { expanded = !expanded }) { Text(stringResource(R.string.jobs_capabilities)) }
            if (expanded) {
                Text(stringResource(R.string.jobs_capability_body), style = MaterialTheme.typography.bodySmall)
                state.capabilities?.providers.orEmpty().forEach { provider ->
                    Text("${provider.label}: ${stringResource(if (provider.available == true) R.string.jobs_provider_available else R.string.jobs_provider_unavailable)}", style = MaterialTheme.typography.labelMedium)
                    provider.reason?.let { Text(it, style = MaterialTheme.typography.bodySmall) }
                }
            }
        }
    }
}

@Composable
private fun JobDetail(job: JobDto, state: JobsUiState, onEvent: (JobsEvent) -> Unit) {
    LazyColumn(contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(14.dp), modifier = Modifier.fillMaxSize().testTag("job-detail")) {
        item {
            Status(job.status)
            Text(job.title, style = MaterialTheme.typography.headlineSmall, modifier = Modifier.padding(top = 10.dp))
            SelectionContainer { Text(job.instructions, modifier = Modifier.padding(vertical = 12.dp)) }
            Text(scheduleLabel(job.schedule), style = MaterialTheme.typography.bodyMedium)
            Text(nextRunLabel(job), style = MaterialTheme.typography.bodySmall, modifier = Modifier.padding(top = 6.dp))
            job.error?.takeIf { it.isNotBlank() }?.let { Text(it, color = MaterialTheme.colorScheme.error, modifier = Modifier.padding(top = 12.dp)) }
        }
        state.notice?.let { notice -> item { NoticeCard(notice, onDismiss = { onEvent(JobsEvent.DismissNotice) }) } }
        item {
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                if (job.status != "cancelled") OutlinedButton(onClick = { onEvent(JobsEvent.Edit) }, enabled = !state.busy) { Text(stringResource(R.string.jobs_edit)) }
                if (job.status == "active") {
                    Button(onClick = { onEvent(JobsEvent.Action("run")) }, enabled = !state.busy) { Text(stringResource(R.string.jobs_run_now)) }
                    OutlinedButton(onClick = { onEvent(JobsEvent.Action("pause")) }, enabled = !state.busy) { Text(stringResource(R.string.jobs_pause)) }
                }
                if (job.status == "paused") Button(onClick = { onEvent(JobsEvent.Action("resume")) }, enabled = !state.busy) { Text(stringResource(R.string.jobs_resume)) }
                if (job.status != "cancelled") TextButton(onClick = { onEvent(JobsEvent.Action("cancel")) }, enabled = !state.busy) { Text(stringResource(R.string.jobs_cancel_job), color = MaterialTheme.colorScheme.error) }
            }
        }
        item {
            HorizontalDivider()
            Text(stringResource(R.string.jobs_history), style = MaterialTheme.typography.titleLarge, modifier = Modifier.padding(top = 20.dp))
            Text(stringResource(R.string.jobs_history_hint, state.capabilities?.historyLimit ?: 50), style = MaterialTheme.typography.bodySmall, modifier = Modifier.padding(top = 8.dp))
        }
        state.runsError?.let { error -> item { ErrorCard(error, onRetry = { onEvent(JobsEvent.Refresh) }) } }
        if (state.detailLoading || state.runsLoading) item { Progress(R.string.jobs_loading_runs) }
        if (state.runsLoaded && state.runs.isEmpty()) item { Text(stringResource(R.string.jobs_no_runs)) }
        if (state.runPagesExpanded) item { Text(stringResource(R.string.jobs_older_hint), style = MaterialTheme.typography.bodySmall) }
        items(state.runs, key = { it.id }) { run -> RunCard(job, run, state.busy, onEvent) }
        if (state.runsCursor != null) item { OutlinedButton(onClick = { onEvent(JobsEvent.OlderRuns) }, enabled = !state.runsLoading && !state.busy, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.jobs_older)) } }
    }
}

@Composable
private fun RunCard(job: JobDto, run: JobRunDto, busy: Boolean, onEvent: (JobsEvent) -> Unit) {
    var expanded by rememberSaveable(run.id) { mutableStateOf(false) }
    Card(Modifier.fillMaxWidth().testTag("run-${run.id}")) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            Status(run.status)
            Text(jobDateLabel(run.createdAt), style = MaterialTheme.typography.labelSmall)
            if (run.progress.isNotBlank()) Text(run.progress)
            run.result?.takeIf { it.isNotBlank() }?.let { SelectionContainer { Text(it) } }
            run.error?.takeIf { it.isNotBlank() }?.let { Text(it, color = MaterialTheme.colorScheme.error) }
            TextButton(onClick = { expanded = !expanded }) { Text(stringResource(R.string.jobs_run_details, run.attempt.coerceAtLeast(1))) }
            if (expanded) SelectionContainer {
                Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text(run.title, style = MaterialTheme.typography.titleSmall)
                    Text(run.instructions)
                    Text(stringResource(R.string.jobs_run_id, run.id), style = MaterialTheme.typography.bodySmall)
                    run.finishedAt?.let { Text(stringResource(R.string.jobs_finished, jobDateLabel(it)), style = MaterialTheme.typography.bodySmall) }
                    run.approvalExpiresAt?.let { Text(stringResource(R.string.jobs_review_expires, jobDateLabel(it)), style = MaterialTheme.typography.bodySmall) }
                }
            }
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                if (run.status == "waiting_approval" && job.status != "cancelled") Button(onClick = { onEvent(JobsEvent.Action("approve", run)) }, enabled = !busy) { Text(stringResource(R.string.jobs_approve)) }
                if (run.status in listOf("failed", "cancelled") && job.status == "active" && run.attempt < 3) OutlinedButton(onClick = { onEvent(JobsEvent.Action("retry", run)) }, enabled = !busy) { Text(stringResource(R.string.jobs_retry)) }
                if (run.status in listOf("queued", "waiting_approval")) TextButton(onClick = { onEvent(JobsEvent.Action("cancel", run)) }, enabled = !busy) { Text(stringResource(R.string.jobs_cancel_run), color = MaterialTheme.colorScheme.error) }
            }
        }
    }
}

@Composable
private fun JobEditorDialog(draft: JobDraft, state: JobsUiState, onEvent: (JobsEvent) -> Unit) {
    var dateOpen by rememberSaveable { mutableStateOf(false) }
    var timeOpen by rememberSaveable { mutableStateOf(false) }
    var timezoneOpen by rememberSaveable { mutableStateOf(false) }
    Dialog(onDismissRequest = { if (!state.busy) onEvent(JobsEvent.CloseEditor) }, properties = DialogProperties(usePlatformDefaultWidth = false, dismissOnBackPress = !state.busy, dismissOnClickOutside = false)) {
        Surface(shape = MaterialTheme.shapes.large, modifier = Modifier.widthIn(max = 720.dp).fillMaxWidth().fillMaxHeight(0.96f).padding(8.dp).testTag("job-editor")) {
            Column(Modifier.imePadding()) {
                Row(Modifier.fillMaxWidth().padding(8.dp), verticalAlignment = Alignment.CenterVertically) {
                    IconButton(onClick = { onEvent(JobsEvent.CloseEditor) }, enabled = !state.busy) { Icon(Icons.Outlined.Close, stringResource(R.string.jobs_close)) }
                    Text(stringResource(if (draft.id == null) R.string.jobs_new else R.string.jobs_edit), style = MaterialTheme.typography.titleLarge, modifier = Modifier.weight(1f))
                    TextButton(onClick = { onEvent(JobsEvent.Save) }, enabled = !state.busy && !state.draftConflict) { Text(stringResource(R.string.jobs_save)) }
                }
                if (state.busy) LinearProgressIndicator(Modifier.fillMaxWidth())
                Column(Modifier.fillMaxWidth().weight(1f).verticalScroll(rememberScrollState()).padding(16.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
                    OutlinedTextField(draft.title, { onEvent(JobsEvent.Draft(draft.copy(title = it))) }, enabled = !state.busy, label = { Text(stringResource(R.string.jobs_field_title)) }, modifier = Modifier.fillMaxWidth().testTag("job-title"), singleLine = true)
                    OutlinedTextField(draft.instructions, { onEvent(JobsEvent.Draft(draft.copy(instructions = it))) }, enabled = !state.busy, label = { Text(stringResource(R.string.jobs_field_notes)) }, modifier = Modifier.fillMaxWidth().testTag("job-notes"), minLines = 3, maxLines = 8)
                    JobChoice(stringResource(R.string.jobs_field_type), draft.kind, listOf("reminder" to stringResource(R.string.jobs_reminder), "review" to stringResource(R.string.jobs_review)), enabled = !state.busy, disabledValues = buildSet { if (state.capabilities?.reminder != true) add("reminder"); if (state.capabilities?.review != true) add("review") }) { onEvent(JobsEvent.Draft(draft.copy(kind = it))) }
                    Text(stringResource(R.string.jobs_capability_body), style = MaterialTheme.typography.bodySmall)
                    JobChoice(stringResource(R.string.jobs_field_schedule), draft.scheduleKind, listOf("manual" to stringResource(R.string.jobs_manual), "once" to stringResource(R.string.jobs_once), "daily" to stringResource(R.string.jobs_daily), "weekdays" to stringResource(R.string.jobs_weekdays), "weekly" to stringResource(R.string.jobs_weekly)), enabled = !state.busy, disabledValues = if (state.capabilities?.scheduling == true) emptySet() else setOf("once", "daily", "weekdays", "weekly")) { onEvent(JobsEvent.Draft(draft.copy(scheduleKind = it))) }
                    if (state.capabilities?.scheduling != true) Text(stringResource(R.string.jobs_scheduler_disabled), style = MaterialTheme.typography.bodySmall)
                    Text(stringResource(R.string.jobs_field_timezone), style = MaterialTheme.typography.labelLarge)
                    OutlinedButton(onClick = { timezoneOpen = true }, enabled = !state.busy, modifier = Modifier.fillMaxWidth()) { Text(draft.timezone) }
                    if (draft.scheduleKind == "once") {
                        Text(stringResource(R.string.jobs_field_date), style = MaterialTheme.typography.labelLarge)
                        OutlinedButton(onClick = { dateOpen = true }, enabled = !state.busy, modifier = Modifier.fillMaxWidth()) { Text(draft.date) }
                    }
                    if (draft.scheduleKind != "manual") {
                        Text(stringResource(R.string.jobs_field_time), style = MaterialTheme.typography.labelLarge)
                        OutlinedButton(onClick = { timeOpen = true }, enabled = !state.busy, modifier = Modifier.fillMaxWidth()) { Text(draft.time) }
                    }
                    if (draft.scheduleKind == "weekly") JobChoice(stringResource(R.string.jobs_field_weekday), draft.weekday.toString(), listOf(1, 2, 3, 4, 5, 6, 0).map { it.toString() to weekdayLabel(it) }, enabled = !state.busy) { onEvent(JobsEvent.Draft(draft.copy(weekday = it.toInt()))) }
                    Text(stringResource(R.string.jobs_schedule_hint), style = MaterialTheme.typography.bodySmall)
                    state.draftError?.let { ErrorText(it) }
                    if (state.draftConflict) {
                        Text(stringResource(R.string.jobs_draft_kept))
                        OutlinedButton(onClick = { onEvent(JobsEvent.ReloadDraft) }, enabled = !state.busy) { Text(stringResource(R.string.jobs_reload_draft)) }
                    }
                }
            }
        }
    }
    if (dateOpen) {
        val initial = runCatching { LocalDate.parse(draft.date).atStartOfDay(ZoneOffset.UTC).toInstant().toEpochMilli() }.getOrNull()
        val picker = rememberDatePickerState(initialSelectedDateMillis = initial)
        DatePickerDialog(onDismissRequest = { dateOpen = false }, confirmButton = { TextButton(enabled = picker.selectedDateMillis != null, onClick = { picker.selectedDateMillis?.let { onEvent(JobsEvent.Draft(draft.copy(date = Instant.ofEpochMilli(it).atZone(ZoneOffset.UTC).toLocalDate().toString()))) }; dateOpen = false }) { Text(stringResource(R.string.jobs_choose)) } }, dismissButton = { TextButton(onClick = { dateOpen = false }) { Text(stringResource(R.string.jobs_close)) } }) { DatePicker(picker) }
    }
    if (timeOpen) {
        val initial = runCatching { LocalTime.parse(draft.time) }.getOrDefault(LocalTime.of(9, 0))
        val picker = rememberTimePickerState(initialHour = initial.hour, initialMinute = initial.minute, is24Hour = true)
        AlertDialog(onDismissRequest = { timeOpen = false }, title = { Text(stringResource(R.string.jobs_field_time)) }, text = { TimeInput(picker) }, confirmButton = { TextButton(onClick = { onEvent(JobsEvent.Draft(draft.copy(time = String.format(Locale.ROOT, "%02d:%02d", picker.hour, picker.minute)))); timeOpen = false }) { Text(stringResource(R.string.jobs_choose)) } }, dismissButton = { TextButton(onClick = { timeOpen = false }) { Text(stringResource(R.string.jobs_close)) } })
    }
    if (timezoneOpen) TimezoneDialog(onClose = { timezoneOpen = false }, onChoose = { onEvent(JobsEvent.Draft(draft.copy(timezone = it))); timezoneOpen = false })
}

@Composable
private fun JobChoice(label: String, value: String, choices: List<Pair<String, String>>, enabled: Boolean, disabledValues: Set<String> = emptySet(), onChange: (String) -> Unit) {
    var expanded by remember { mutableStateOf(false) }
    ExposedDropdownMenuBox(expanded = expanded && enabled, onExpandedChange = { if (enabled) expanded = it }) {
        OutlinedTextField(value = choices.firstOrNull { it.first == value }?.second ?: value, onValueChange = {}, readOnly = true, enabled = enabled, label = { Text(label) }, trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(expanded) }, modifier = Modifier.menuAnchor(MenuAnchorType.PrimaryNotEditable).fillMaxWidth())
        ExposedDropdownMenu(expanded = expanded && enabled, onDismissRequest = { expanded = false }) {
            choices.forEach { (key, text) -> DropdownMenuItem(text = { Text(text) }, enabled = key !in disabledValues, onClick = { onChange(key); expanded = false }) }
        }
    }
}

@Composable
private fun TimezoneDialog(onClose: () -> Unit, onChoose: (String) -> Unit) {
    var query by rememberSaveable { mutableStateOf("") }
    val zones = remember { (ZoneId.getAvailableZoneIds() + "UTC").sorted() }
    AlertDialog(onDismissRequest = onClose, title = { Text(stringResource(R.string.jobs_field_timezone)) }, text = {
        Column {
            OutlinedTextField(query, { query = it }, label = { Text(stringResource(R.string.jobs_search_timezone)) }, singleLine = true)
            LazyColumn(Modifier.heightIn(max = 350.dp)) { items(zones.filter { it.contains(query, ignoreCase = true) }, key = { it }) { zone -> TextButton(onClick = { onChoose(zone) }, modifier = Modifier.fillMaxWidth()) { Text(zone, modifier = Modifier.fillMaxWidth()) } } }
        }
    }, confirmButton = { TextButton(onClick = onClose) { Text(stringResource(R.string.jobs_close)) } })
}

@Composable
private fun JobConfirmationDialog(request: JobConfirmation, state: JobsUiState, onEvent: (JobsEvent) -> Unit) {
    val (title, body) = when {
        request.action == "approve" -> R.string.jobs_confirm_review to R.string.jobs_confirm_review_body
        request.action == "retry" -> R.string.jobs_confirm_retry to R.string.jobs_confirm_retry_body
        request.action == "pause" -> R.string.jobs_confirm_pause to R.string.jobs_confirm_pause_body
        request.run != null -> R.string.jobs_confirm_cancel to R.string.jobs_confirm_cancel_body
        else -> R.string.jobs_confirm_job_cancel to R.string.jobs_confirm_job_cancel_body
    }
    AlertDialog(onDismissRequest = { if (!state.busy) onEvent(JobsEvent.DismissConfirmation) }, modifier = Modifier.testTag("jobs-confirmation"), title = { Text(stringResource(title)) }, text = {
        Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text(stringResource(body))
            SelectionContainer { Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text(request.run?.title ?: request.job.title, style = MaterialTheme.typography.titleMedium)
                Text(request.run?.instructions ?: request.job.instructions)
                request.run?.let { Text(stringResource(R.string.jobs_run_id, it.id), style = MaterialTheme.typography.bodySmall) }
                request.run?.approvalExpiresAt?.let { Text(stringResource(R.string.jobs_review_expires, jobDateLabel(it)), style = MaterialTheme.typography.bodySmall) }
            } }
            state.confirmationError?.let { ErrorText(it) }
            if (state.busy) LinearProgressIndicator(Modifier.fillMaxWidth())
        }
    }, confirmButton = { TextButton(onClick = { onEvent(JobsEvent.Confirm) }, enabled = !state.busy && !state.confirmationStale, modifier = Modifier.testTag("jobs-confirm")) { Text(stringResource(R.string.jobs_confirm)) } }, dismissButton = { TextButton(onClick = { onEvent(JobsEvent.DismissConfirmation) }, enabled = !state.busy) { Text(stringResource(R.string.jobs_go_back)) } })
}

@Composable private fun Status(status: String) { Surface(color = MaterialTheme.colorScheme.secondaryContainer, shape = MaterialTheme.shapes.small) { Text(stringResource(jobStatusLabel(status)), modifier = Modifier.padding(horizontal = 10.dp, vertical = 5.dp), style = MaterialTheme.typography.labelMedium) } }
@Composable private fun Progress(label: Int) { Row(Modifier.fillMaxWidth().padding(12.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) { CircularProgressIndicator(Modifier.size(24.dp)); Text(stringResource(label)) } }
@Composable private fun ErrorText(message: Int) { Text(stringResource(message), color = MaterialTheme.colorScheme.error, modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite }) }
@Composable private fun ErrorCard(message: Int, onRetry: () -> Unit) { Card(Modifier.fillMaxWidth()) { Column(Modifier.padding(16.dp)) { ErrorText(message); TextButton(onClick = onRetry) { Text(stringResource(R.string.jobs_try_again)) } } } }
@Composable private fun NoticeCard(message: Int, onDismiss: () -> Unit) { Card(Modifier.fillMaxWidth()) { Row(Modifier.padding(12.dp), verticalAlignment = Alignment.CenterVertically) { Text(stringResource(message), modifier = Modifier.weight(1f).semantics { liveRegion = LiveRegionMode.Polite }); IconButton(onClick = onDismiss) { Icon(Icons.Outlined.Close, stringResource(R.string.jobs_close)) } } } }
private fun weekdayLabel(day: Int): String = DayOfWeek.of(if (day == 0) 7 else day.coerceIn(1, 7)).getDisplayName(TextStyle.FULL, Locale.getDefault())
@Composable private fun scheduleLabel(schedule: JobScheduleDto): String = when (schedule.kind) {
    "once" -> if (schedule.at.isNullOrBlank()) stringResource(R.string.jobs_manual) else "${stringResource(R.string.jobs_once)} · ${jobDateLabel(schedule.at, schedule.timezone)} · ${schedule.timezone ?: "UTC"}"
    "daily" -> "${stringResource(R.string.jobs_daily)} · ${schedule.time} · ${schedule.timezone}"
    "weekdays" -> "${stringResource(R.string.jobs_weekdays)} · ${schedule.time} · ${schedule.timezone}"
    "weekly" -> "${weekdayLabel(schedule.weekday ?: 0)} · ${schedule.time} · ${schedule.timezone}"
    else -> stringResource(R.string.jobs_unknown)
}
@Composable private fun nextRunLabel(job: JobDto): String = if (job.status != "active" || job.nextRunAt.isNullOrBlank()) stringResource(R.string.jobs_no_next) else stringResource(R.string.jobs_next, "${jobDateLabel(job.nextRunAt, job.schedule.timezone)} · ${job.schedule.timezone ?: "UTC"}")
