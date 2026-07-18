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
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.ArrowBack
import androidx.compose.material.icons.outlined.FilterList
import androidx.compose.material.icons.outlined.History
import androidx.compose.material.icons.outlined.Refresh
import androidx.compose.material3.Badge
import androidx.compose.material3.BadgedBox
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DatePickerDialog
import androidx.compose.material3.DateRangePicker
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExposedDropdownMenuBox
import androidx.compose.material3.ExposedDropdownMenuDefaults
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.MenuAnchorType
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberDateRangePickerState
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import java.time.Instant
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter
import java.time.format.FormatStyle
import java.util.Locale
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.ui.history.ConversationUi
import ninja.jeremy.liveninja.ui.history.HistoryViewModel
import ninja.jeremy.liveninja.ui.history.TopicUi
import ninja.jeremy.liveninja.ui.history.TurnUi

/**
 * History tab — the Android face of the M11 filterable conversation history
 * (FR-TOP-04/05), mirroring the web /history page over the same Query-only
 * API. The list shows each conversation with its topic chips; the filter
 * sheet offers a populated topic multi-select, a populated device dropdown,
 * and a date-range picker (no blind text boxes anywhere). Tapping a row opens
 * the transcript detail in-screen (system back returns to the list).
 */
@Composable
fun HistoryScreen(modifier: Modifier = Modifier) {
    val viewModel: HistoryViewModel = hiltViewModel()
    val state by viewModel.state.collectAsState()

    LaunchedEffect(Unit) { viewModel.loadIfNeeded() }

    // Detail view replaces the list; back closes it.
    if (state.detailId != null) {
        BackHandler(onBack = viewModel::closeDetail)
        ConversationDetail(viewModel = viewModel, modifier = modifier)
        return
    }

    Scaffold(modifier = modifier.fillMaxSize()) { padding ->
        Column(Modifier.fillMaxSize().padding(padding)) {
            HeaderBar(
                loading = state.loading,
                filtersActive = state.filters.isActive,
                onFilter = viewModel::openFilterSheet,
                onRefresh = viewModel::refresh,
            )
            if (state.filters.isActive) {
                ActiveFilterSummary(viewModel = viewModel)
            }
            HorizontalDivider()

            when {
                state.loading && !state.loaded -> CenteredProgress()
                state.error && state.items.isEmpty() -> CenteredError(onRetry = viewModel::refresh)
                state.loaded && state.items.isEmpty() -> CenteredEmpty(
                    filtered = state.filters.isActive,
                    onClearFilters = viewModel::clearFilters,
                )
                else -> ConversationList(
                    items = state.items,
                    hasMore = state.nextCursor != null,
                    loadingMore = state.loadingMore,
                    onTap = { viewModel.openDetail(it.id) },
                    onLoadMore = viewModel::loadMore,
                )
            }
        }
    }

    if (state.filterSheetOpen) {
        FilterSheet(viewModel = viewModel)
    }
}

@Composable
private fun HeaderBar(
    loading: Boolean,
    filtersActive: Boolean,
    onFilter: () -> Unit,
    onRefresh: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(start = 16.dp, end = 4.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(
            stringResource(R.string.history_title),
            style = MaterialTheme.typography.titleLarge,
            modifier = Modifier.weight(1f),
        )
        IconButton(onClick = onFilter, modifier = Modifier.size(48.dp)) {
            BadgedBox(badge = { if (filtersActive) Badge() }) {
                Icon(
                    Icons.Outlined.FilterList,
                    contentDescription = stringResource(R.string.history_filter),
                )
            }
        }
        if (loading) {
            CircularProgressIndicator(
                modifier = Modifier.padding(12.dp).size(24.dp),
                strokeWidth = 2.dp,
            )
        } else {
            IconButton(onClick = onRefresh, modifier = Modifier.size(48.dp)) {
                Icon(
                    Icons.Outlined.Refresh,
                    contentDescription = stringResource(R.string.history_refresh),
                )
            }
        }
    }
}

/** Removable chips summarizing the applied filters, plus "clear all". */
@Composable
private fun ActiveFilterSummary(viewModel: HistoryViewModel) {
    val state by viewModel.state.collectAsState()
    val filters = state.filters
    FlowRow(
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 16.dp, vertical = 4.dp),
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        filters.topicIds.forEach { id ->
            val topic = state.topics.firstOrNull { it.id == id }
            FilterChip(
                selected = true,
                onClick = {
                    viewModel.toggleTopicFilter(id)
                    viewModel.applyFilters()
                },
                label = { Text(topic?.name ?: id) },
                leadingIcon = { TopicDot(topic?.colorHex) },
            )
        }
        filters.deviceId?.let { deviceId ->
            val device = state.devices.firstOrNull { it.id == deviceId }
            FilterChip(
                selected = true,
                onClick = {
                    viewModel.setDeviceFilter(null)
                    viewModel.applyFilters()
                },
                label = { Text(device?.name ?: deviceId) },
            )
        }
        if (filters.fromMillis != null || filters.toMillis != null) {
            FilterChip(
                selected = true,
                onClick = {
                    viewModel.setDateRange(null, null)
                    viewModel.applyFilters()
                },
                label = { Text(rangeLabel(filters.fromMillis, filters.toMillis)) },
            )
        }
        TextButton(onClick = viewModel::clearFilters, modifier = Modifier.heightIn(min = 40.dp)) {
            Text(stringResource(R.string.history_filter_clear_all))
        }
    }
}

// ---- list ----

@Composable
private fun ConversationList(
    items: List<ConversationUi>,
    hasMore: Boolean,
    loadingMore: Boolean,
    onTap: (ConversationUi) -> Unit,
    onLoadMore: () -> Unit,
) {
    LazyColumn(
        modifier = Modifier.fillMaxSize(),
        contentPadding = PaddingValues(horizontal = 16.dp, vertical = 8.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        items(items, key = { it.id }) { item ->
            ConversationCard(item = item, onTap = { onTap(item) })
        }
        if (hasMore) {
            item(key = "load-more") {
                Box(Modifier.fillMaxWidth(), contentAlignment = Alignment.Center) {
                    if (loadingMore) {
                        CircularProgressIndicator(
                            modifier = Modifier.padding(8.dp).size(24.dp),
                            strokeWidth = 2.dp,
                        )
                    } else {
                        OutlinedButton(
                            onClick = onLoadMore,
                            modifier = Modifier.heightIn(min = 48.dp),
                        ) { Text(stringResource(R.string.history_load_more)) }
                    }
                }
            }
        }
    }
}

@Composable
private fun ConversationCard(item: ConversationUi, onTap: () -> Unit) {
    Card(onClick = onTap, modifier = Modifier.fillMaxWidth()) {
        Column(
            Modifier
                .fillMaxWidth()
                .heightIn(min = 64.dp)
                .padding(horizontal = 16.dp, vertical = 12.dp),
        ) {
            Text(
                item.title ?: stringResource(R.string.history_untitled),
                style = MaterialTheme.typography.bodyLarge,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis,
            )
            val meta = listOfNotNull(item.dateLabel, item.metaLabel).joinToString(" · ")
            if (meta.isNotBlank()) {
                Text(
                    meta,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            if (item.topics.isNotEmpty()) {
                TopicChipRow(topics = item.topics)
            }
        }
    }
}

@Composable
private fun TopicChipRow(topics: List<TopicUi>) {
    FlowRow(
        modifier = Modifier.padding(top = 4.dp),
        horizontalArrangement = Arrangement.spacedBy(6.dp),
    ) {
        topics.forEach { topic ->
            Surface(
                shape = MaterialTheme.shapes.small,
                color = MaterialTheme.colorScheme.secondaryContainer,
            ) {
                Row(
                    modifier = Modifier.padding(horizontal = 8.dp, vertical = 4.dp),
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(4.dp),
                ) {
                    TopicDot(topic.colorHex)
                    Text(topic.name, style = MaterialTheme.typography.labelMedium)
                }
            }
        }
    }
}

/** Colored dot for a topic (color from the Topic Manager; theme fallback). */
@Composable
private fun TopicDot(colorHex: String?) {
    val color = parseHexColor(colorHex) ?: MaterialTheme.colorScheme.primary
    Surface(color = color, shape = CircleShape, modifier = Modifier.size(8.dp)) {}
}

private fun parseHexColor(hex: String?): Color? {
    if (hex.isNullOrBlank()) return null
    return runCatching { Color(android.graphics.Color.parseColor(hex)) }.getOrNull()
}

// ---- filter sheet ----

@Composable
private fun FilterSheet(viewModel: HistoryViewModel) {
    val state by viewModel.state.collectAsState()
    val sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    var showDatePicker by remember { mutableStateOf(false) }

    ModalBottomSheet(
        onDismissRequest = viewModel::applyFilters,
        sheetState = sheetState,
    ) {
        Column(
            Modifier
                .fillMaxWidth()
                .padding(horizontal = 16.dp)
                .padding(bottom = 24.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            Text(
                stringResource(R.string.history_filter_title),
                style = MaterialTheme.typography.titleMedium,
            )

            // Topics: multi-select chips, populated from the taxonomy.
            Text(
                stringResource(R.string.history_filter_topics),
                style = MaterialTheme.typography.labelLarge,
            )
            if (state.topics.isEmpty()) {
                Text(
                    stringResource(R.string.history_filter_no_topics),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            } else {
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    state.topics.filterNot { it.archived }.forEach { topic ->
                        FilterChip(
                            selected = topic.id in state.filters.topicIds,
                            onClick = { viewModel.toggleTopicFilter(topic.id) },
                            label = { Text(topic.name) },
                            leadingIcon = { TopicDot(topic.colorHex) },
                        )
                    }
                }
            }

            // Device: populated dropdown ("All devices" default).
            Text(
                stringResource(R.string.history_filter_device),
                style = MaterialTheme.typography.labelLarge,
            )
            DevicePicker(viewModel = viewModel)

            // Date range: picker, never typed.
            Text(
                stringResource(R.string.history_filter_dates),
                style = MaterialTheme.typography.labelLarge,
            )
            Row(
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                OutlinedButton(
                    onClick = { showDatePicker = true },
                    modifier = Modifier.heightIn(min = 48.dp),
                ) {
                    Text(
                        if (state.filters.fromMillis == null && state.filters.toMillis == null) {
                            stringResource(R.string.history_filter_dates_any)
                        } else {
                            rangeLabel(state.filters.fromMillis, state.filters.toMillis)
                        },
                    )
                }
                if (state.filters.fromMillis != null || state.filters.toMillis != null) {
                    TextButton(
                        onClick = { viewModel.setDateRange(null, null) },
                        modifier = Modifier.heightIn(min = 48.dp),
                    ) { Text(stringResource(R.string.history_filter_dates_clear)) }
                }
            }

            Button(
                onClick = viewModel::applyFilters,
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = 48.dp),
            ) { Text(stringResource(R.string.history_filter_apply)) }
        }
    }

    if (showDatePicker) {
        val pickerState = rememberDateRangePickerState(
            initialSelectedStartDateMillis = state.filters.fromMillis,
            initialSelectedEndDateMillis = state.filters.toMillis,
        )
        DatePickerDialog(
            onDismissRequest = { showDatePicker = false },
            confirmButton = {
                TextButton(
                    onClick = {
                        viewModel.setDateRange(
                            pickerState.selectedStartDateMillis,
                            pickerState.selectedEndDateMillis
                                ?: pickerState.selectedStartDateMillis,
                        )
                        showDatePicker = false
                    },
                    modifier = Modifier.heightIn(min = 48.dp),
                ) { Text(stringResource(R.string.history_filter_dates_set)) }
            },
            dismissButton = {
                TextButton(
                    onClick = { showDatePicker = false },
                    modifier = Modifier.heightIn(min = 48.dp),
                ) { Text(stringResource(R.string.dialog_cancel)) }
            },
        ) {
            DateRangePicker(
                state = pickerState,
                modifier = Modifier.weight(1f),
                showModeToggle = false,
            )
        }
    }
}

@Composable
private fun DevicePicker(viewModel: HistoryViewModel) {
    val state by viewModel.state.collectAsState()
    var expanded by remember { mutableStateOf(false) }
    val allLabel = stringResource(R.string.history_filter_device_all)
    val selectedLabel = state.filters.deviceId
        ?.let { id -> state.devices.firstOrNull { it.id == id }?.name ?: id }
        ?: allLabel

    ExposedDropdownMenuBox(
        expanded = expanded,
        onExpandedChange = { expanded = it },
    ) {
        OutlinedTextField(
            value = selectedLabel,
            onValueChange = {},
            readOnly = true,
            label = { Text(stringResource(R.string.history_filter_device)) },
            trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(expanded = expanded) },
            modifier = Modifier
                .fillMaxWidth()
                .menuAnchor(MenuAnchorType.PrimaryNotEditable),
        )
        ExposedDropdownMenu(
            expanded = expanded,
            onDismissRequest = { expanded = false },
        ) {
            DropdownMenuItem(
                text = { Text(allLabel) },
                onClick = {
                    viewModel.setDeviceFilter(null)
                    expanded = false
                },
            )
            state.devices.forEach { device ->
                DropdownMenuItem(
                    text = { Text(device.name) },
                    onClick = {
                        viewModel.setDeviceFilter(device.id)
                        expanded = false
                    },
                )
            }
        }
    }
}

// ---- detail ----

@Composable
private fun ConversationDetail(viewModel: HistoryViewModel, modifier: Modifier = Modifier) {
    val state by viewModel.state.collectAsState()

    Scaffold(modifier = modifier.fillMaxSize()) { padding ->
        Column(Modifier.fillMaxSize().padding(padding)) {
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(start = 4.dp, end = 16.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                IconButton(onClick = viewModel::closeDetail, modifier = Modifier.size(48.dp)) {
                    Icon(
                        Icons.AutoMirrored.Outlined.ArrowBack,
                        contentDescription = stringResource(R.string.history_detail_back),
                    )
                }
                Text(
                    state.detail?.title ?: stringResource(R.string.history_untitled),
                    style = MaterialTheme.typography.titleLarge,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f),
                )
            }
            HorizontalDivider()

            val detail = state.detail
            when {
                state.detailLoading -> CenteredProgress()
                state.detailError || detail == null -> CenteredError(
                    onRetry = { state.detailId?.let(viewModel::openDetail) },
                )
                else -> LazyColumn(
                    modifier = Modifier.fillMaxSize(),
                    contentPadding = PaddingValues(horizontal = 16.dp, vertical = 8.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    item(key = "meta") {
                        Column {
                            val meta = listOfNotNull(detail.dateLabel, detail.metaLabel)
                                .joinToString(" · ")
                            if (meta.isNotBlank()) {
                                Text(
                                    meta,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            }
                            if (detail.topics.isNotEmpty()) {
                                TopicChipRow(topics = detail.topics)
                            }
                        }
                    }
                    if (detail.turns.isEmpty()) {
                        item(key = "no-transcript") {
                            Text(
                                stringResource(R.string.history_no_transcript),
                                style = MaterialTheme.typography.bodyMedium,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                modifier = Modifier.padding(top = 16.dp),
                            )
                        }
                    } else {
                        items(detail.turns.size) { index ->
                            TranscriptTurn(turn = detail.turns[index])
                        }
                    }
                }
            }
        }
    }
}

@Composable
private fun TranscriptTurn(turn: TurnUi) {
    Column {
        Text(
            stringResource(
                if (turn.isUser) R.string.conversation_role_you else R.string.conversation_role_assistant,
            ),
            style = MaterialTheme.typography.labelMedium,
            color = if (turn.isUser) {
                MaterialTheme.colorScheme.primary
            } else {
                MaterialTheme.colorScheme.tertiary
            },
        )
        Text(turn.text, style = MaterialTheme.typography.bodyMedium)
    }
}

// ---- shared states ----

@Composable
private fun CenteredProgress() {
    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
        CircularProgressIndicator()
    }
}

@Composable
private fun CenteredError(onRetry: () -> Unit) {
    Column(
        modifier = Modifier.fillMaxSize().padding(24.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Text(
            stringResource(R.string.history_error_title),
            style = MaterialTheme.typography.headlineSmall,
            textAlign = TextAlign.Center,
        )
        Text(
            stringResource(R.string.history_error_body),
            style = MaterialTheme.typography.bodyMedium,
            textAlign = TextAlign.Center,
            modifier = Modifier.padding(top = 8.dp),
        )
        Button(
            onClick = onRetry,
            modifier = Modifier.padding(top = 16.dp).heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.history_retry)) }
    }
}

@Composable
private fun CenteredEmpty(filtered: Boolean, onClearFilters: () -> Unit) {
    Column(
        modifier = Modifier.fillMaxSize().padding(24.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Icon(
            imageVector = Icons.Outlined.History,
            contentDescription = null,
            modifier = Modifier.size(64.dp),
            tint = MaterialTheme.colorScheme.primary,
        )
        Text(
            stringResource(
                if (filtered) R.string.history_empty_filtered_title else R.string.history_empty_title,
            ),
            style = MaterialTheme.typography.headlineSmall,
            textAlign = TextAlign.Center,
            modifier = Modifier.padding(top = 16.dp),
        )
        Text(
            stringResource(
                if (filtered) R.string.history_empty_filtered_body else R.string.history_empty_body,
            ),
            style = MaterialTheme.typography.bodyMedium,
            textAlign = TextAlign.Center,
            modifier = Modifier.padding(top = 8.dp),
        )
        if (filtered) {
            OutlinedButton(
                onClick = onClearFilters,
                modifier = Modifier.padding(top = 16.dp).heightIn(min = 48.dp),
            ) { Text(stringResource(R.string.history_filter_clear_all)) }
        }
    }
}

// ---- formatting ----

private fun rangeLabel(fromMillis: Long?, toMillis: Long?): String {
    val fmt = DateTimeFormatter.ofLocalizedDate(FormatStyle.MEDIUM).withLocale(Locale.getDefault())
    // Picker millis are UTC-midnight based; format in UTC to avoid day drift.
    fun label(ms: Long) = fmt.format(Instant.ofEpochMilli(ms).atZone(ZoneOffset.UTC).toLocalDate())
    val from = fromMillis?.let(::label)
    val to = toMillis?.let(::label)
    return when {
        from != null && to != null && from == to -> from
        from != null && to != null -> "$from – $to"
        from != null -> "$from –"
        to != null -> "– $to"
        else -> ""
    }
}
