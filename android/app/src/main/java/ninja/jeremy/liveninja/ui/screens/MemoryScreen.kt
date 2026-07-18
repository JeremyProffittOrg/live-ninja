@file:OptIn(ExperimentalMaterial3Api::class)

package ninja.jeremy.liveninja.ui.screens

import androidx.compose.foundation.horizontalScroll
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
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.DeleteOutline
import androidx.compose.material.icons.outlined.KeyboardArrowDown
import androidx.compose.material.icons.outlined.KeyboardArrowUp
import androidx.compose.material.icons.outlined.Psychology
import androidx.compose.material.icons.outlined.Refresh
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Switch
import androidx.compose.material3.Tab
import androidx.compose.material3.TabRow
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.ui.memory.EntityType
import ninja.jeremy.liveninja.ui.memory.EntityUi
import ninja.jeremy.liveninja.ui.memory.GuideUi
import ninja.jeremy.liveninja.ui.memory.MemoryNotice
import ninja.jeremy.liveninja.ui.memory.MemoryViewModel

/**
 * Memory tab — the Android face of the M10 memory browser + Guide Manager
 * (FR-MEM-05/09), mirroring the web /memory page over the same Query-only
 * API. Two sub-tabs: Entities (type-filter chips from the fixed six-type
 * enum, tap for read-only detail, forget behind a confirm dialog — the
 * delete propagates to both DynamoDB and the embedding store server-side)
 * and Guides (enable toggles + priority steppers; enabled guides are
 * injected into every session priority-ascending).
 */
@Composable
fun MemoryScreen(modifier: Modifier = Modifier) {
    val viewModel: MemoryViewModel = hiltViewModel()
    val state by viewModel.state.collectAsState()
    val snackbarHostState = remember { SnackbarHostState() }
    val context = LocalContext.current
    var tab by rememberSaveable { mutableIntStateOf(0) }

    LaunchedEffect(Unit) { viewModel.loadIfNeeded() }

    LaunchedEffect(Unit) {
        viewModel.notices.collect { notice ->
            val res = when (notice) {
                MemoryNotice.FORGOTTEN -> R.string.memory_forgotten
                MemoryNotice.FORGET_FAILED -> R.string.memory_forget_failed
                MemoryNotice.GUIDE_UPDATE_FAILED -> R.string.memory_guide_update_failed
            }
            snackbarHostState.showSnackbar(context.getString(res))
        }
    }

    Scaffold(
        modifier = modifier.fillMaxSize(),
        snackbarHost = { SnackbarHost(snackbarHostState) },
    ) { padding ->
        Column(Modifier.fillMaxSize().padding(padding)) {
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(start = 16.dp, end = 4.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    stringResource(R.string.memory_title),
                    style = MaterialTheme.typography.titleLarge,
                    modifier = Modifier.weight(1f),
                )
                val loading =
                    if (tab == 0) state.entitiesLoading else state.guidesLoading
                if (loading) {
                    CircularProgressIndicator(
                        modifier = Modifier.padding(12.dp).size(24.dp),
                        strokeWidth = 2.dp,
                    )
                } else {
                    IconButton(
                        onClick = {
                            if (tab == 0) viewModel.refreshEntities() else viewModel.refreshGuides()
                        },
                        modifier = Modifier.size(48.dp),
                    ) {
                        Icon(
                            Icons.Outlined.Refresh,
                            contentDescription = stringResource(R.string.memory_refresh),
                        )
                    }
                }
            }
            TabRow(selectedTabIndex = tab) {
                Tab(
                    selected = tab == 0,
                    onClick = { tab = 0 },
                    text = { Text(stringResource(R.string.memory_tab_entities)) },
                    modifier = Modifier.heightIn(min = 48.dp),
                )
                Tab(
                    selected = tab == 1,
                    onClick = { tab = 1 },
                    text = { Text(stringResource(R.string.memory_tab_guides)) },
                    modifier = Modifier.heightIn(min = 48.dp),
                )
            }
            when (tab) {
                0 -> EntitiesTab(viewModel = viewModel)
                else -> GuidesTab(viewModel = viewModel)
            }
        }
    }

    state.detail?.let { entity ->
        EntityDetailDialog(
            entity = entity,
            onForget = { viewModel.requestForget(entity) },
            onDismiss = viewModel::closeDetail,
        )
    }

    state.confirmForget?.let { entity ->
        AlertDialog(
            onDismissRequest = viewModel::cancelForget,
            title = { Text(stringResource(R.string.memory_forget_confirm_title)) },
            text = { Text(stringResource(R.string.memory_forget_confirm_body, entity.name)) },
            confirmButton = {
                Button(
                    onClick = viewModel::confirmForget,
                    enabled = !state.forgetInProgress,
                    modifier = Modifier.heightIn(min = 48.dp),
                ) { Text(stringResource(R.string.memory_forget)) }
            },
            dismissButton = {
                TextButton(
                    onClick = viewModel::cancelForget,
                    modifier = Modifier.heightIn(min = 48.dp),
                ) { Text(stringResource(R.string.dialog_cancel)) }
            },
        )
    }
}

// ---- Entities tab ----

@Composable
private fun EntitiesTab(viewModel: MemoryViewModel) {
    val state by viewModel.state.collectAsState()

    Column(Modifier.fillMaxSize()) {
        // Type filter: fixed enumerable set -> chips, never free text.
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .horizontalScroll(rememberScrollState())
                .padding(horizontal = 16.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            FilterChip(
                selected = state.typeFilter == null,
                onClick = { viewModel.setTypeFilter(null) },
                label = { Text(stringResource(R.string.memory_type_all)) },
                modifier = Modifier.padding(vertical = 8.dp),
            )
            EntityType.entries.forEach { type ->
                FilterChip(
                    selected = state.typeFilter == type,
                    onClick = { viewModel.setTypeFilter(type) },
                    label = { Text(stringResource(typeLabel(type))) },
                    modifier = Modifier.padding(vertical = 8.dp),
                )
            }
        }
        HorizontalDivider()

        when {
            state.entitiesLoading && !state.entitiesLoaded -> CenteredProgress()
            state.entitiesError && state.entities.isEmpty() -> CenteredError(
                titleRes = R.string.memory_error_title,
                bodyRes = R.string.memory_error_body,
                onRetry = viewModel::refreshEntities,
            )
            state.entitiesLoaded && state.entities.isEmpty() -> CenteredEmpty(
                titleRes = R.string.memory_empty_title,
                bodyRes = R.string.memory_empty_body,
            )
            else -> LazyColumn(
                modifier = Modifier.fillMaxSize(),
                contentPadding = PaddingValues(horizontal = 16.dp, vertical = 8.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                items(state.entities, key = { it.id }) { entity ->
                    EntityCard(
                        entity = entity,
                        onTap = { viewModel.showDetail(entity) },
                        onForget = { viewModel.requestForget(entity) },
                    )
                }
                if (state.entitiesCursor != null) {
                    item(key = "load-more") {
                        Box(Modifier.fillMaxWidth(), contentAlignment = Alignment.Center) {
                            if (state.entitiesLoadingMore) {
                                CircularProgressIndicator(
                                    modifier = Modifier.padding(8.dp).size(24.dp),
                                    strokeWidth = 2.dp,
                                )
                            } else {
                                OutlinedButton(
                                    onClick = viewModel::loadMoreEntities,
                                    modifier = Modifier.heightIn(min = 48.dp),
                                ) { Text(stringResource(R.string.memory_load_more)) }
                            }
                        }
                    }
                }
            }
        }
    }
}

@Composable
private fun EntityCard(
    entity: EntityUi,
    onTap: () -> Unit,
    onForget: () -> Unit,
) {
    Card(onClick = onTap, modifier = Modifier.fillMaxWidth()) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 64.dp)
                .padding(start = 16.dp, end = 4.dp, top = 8.dp, bottom = 8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(Modifier.weight(1f)) {
                Text(
                    entity.name,
                    style = MaterialTheme.typography.bodyLarge,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
                val meta = listOfNotNull(
                    entityTypeDisplay(entity.type),
                    entity.updatedLabel,
                    entity.relationCount.takeIf { it > 0 }?.let {
                        stringResource(R.string.memory_relation_count, it)
                    },
                ).joinToString(" · ")
                if (meta.isNotBlank()) {
                    Text(
                        meta,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            IconButton(onClick = onForget, modifier = Modifier.size(48.dp)) {
                Icon(
                    Icons.Outlined.DeleteOutline,
                    contentDescription = stringResource(R.string.memory_forget_cd, entity.name),
                )
            }
        }
    }
}

@Composable
private fun EntityDetailDialog(
    entity: EntityUi,
    onForget: () -> Unit,
    onDismiss: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(entity.name) },
        text = {
            Column(
                modifier = Modifier.verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                LabeledValue(
                    label = stringResource(R.string.memory_detail_type),
                    value = entityTypeDisplay(entity.type) ?: entity.type,
                )
                entity.updatedLabel?.let {
                    LabeledValue(label = stringResource(R.string.memory_detail_updated), value = it)
                }
                entity.attrLines.forEach { (key, value) ->
                    LabeledValue(label = key, value = value)
                }
                if (entity.attrLines.isEmpty()) {
                    Text(
                        stringResource(R.string.memory_detail_no_attrs),
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        },
        confirmButton = {
            TextButton(onClick = onForget, modifier = Modifier.heightIn(min = 48.dp)) {
                Text(stringResource(R.string.memory_forget))
            }
        },
        dismissButton = {
            TextButton(onClick = onDismiss, modifier = Modifier.heightIn(min = 48.dp)) {
                Text(stringResource(R.string.memory_detail_close))
            }
        },
    )
}

/** Key/value line — every value gets a visible label. */
@Composable
private fun LabeledValue(label: String, value: String) {
    Column {
        Text(
            label,
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Text(value, style = MaterialTheme.typography.bodyMedium)
    }
}

// ---- Guides tab ----

@Composable
private fun GuidesTab(viewModel: MemoryViewModel) {
    val state by viewModel.state.collectAsState()

    when {
        state.guidesLoading && !state.guidesLoaded -> CenteredProgress()
        state.guidesError && state.guides.isEmpty() -> CenteredError(
            titleRes = R.string.memory_guides_error_title,
            bodyRes = R.string.memory_error_body,
            onRetry = viewModel::refreshGuides,
        )
        state.guidesLoaded && state.guides.isEmpty() -> CenteredEmpty(
            titleRes = R.string.memory_guides_empty_title,
            bodyRes = R.string.memory_guides_empty_body,
        )
        else -> LazyColumn(
            modifier = Modifier.fillMaxSize(),
            contentPadding = PaddingValues(horizontal = 16.dp, vertical = 8.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            item(key = "caption") {
                Text(
                    stringResource(R.string.memory_guides_caption),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(bottom = 4.dp),
                )
            }
            items(state.guides, key = { it.id }) { guide ->
                GuideCard(
                    guide = guide,
                    onToggle = { enabled -> viewModel.setGuideEnabled(guide, enabled) },
                    onPriorityUp = { viewModel.adjustGuidePriority(guide, -1) },
                    onPriorityDown = { viewModel.adjustGuidePriority(guide, +1) },
                )
            }
        }
    }
}

@Composable
private fun GuideCard(
    guide: GuideUi,
    onToggle: (Boolean) -> Unit,
    onPriorityUp: () -> Unit,
    onPriorityDown: () -> Unit,
) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.fillMaxWidth().padding(start = 16.dp, end = 8.dp, top = 8.dp, bottom = 8.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) {
                    Text(
                        guide.title,
                        style = MaterialTheme.typography.bodyLarge,
                        maxLines = 2,
                        overflow = TextOverflow.Ellipsis,
                    )
                    if (guide.text.isNotBlank()) {
                        Text(
                            guide.text,
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            maxLines = 3,
                            overflow = TextOverflow.Ellipsis,
                        )
                    }
                }
                Switch(
                    checked = guide.enabled,
                    onCheckedChange = onToggle,
                    enabled = !guide.busy,
                    modifier = Modifier.padding(start = 8.dp),
                )
            }
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    stringResource(R.string.memory_guide_priority, guide.priority),
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.weight(1f),
                )
                if (guide.busy) {
                    CircularProgressIndicator(
                        modifier = Modifier.padding(12.dp).size(20.dp),
                        strokeWidth = 2.dp,
                    )
                } else {
                    // Bounded number -> stepper (lower = injected earlier).
                    IconButton(
                        onClick = onPriorityUp,
                        enabled = guide.priority > 0,
                        modifier = Modifier.size(48.dp),
                    ) {
                        Icon(
                            Icons.Outlined.KeyboardArrowUp,
                            contentDescription =
                                stringResource(R.string.memory_guide_priority_raise, guide.title),
                        )
                    }
                    IconButton(onClick = onPriorityDown, modifier = Modifier.size(48.dp)) {
                        Icon(
                            Icons.Outlined.KeyboardArrowDown,
                            contentDescription =
                                stringResource(R.string.memory_guide_priority_lower, guide.title),
                        )
                    }
                }
            }
        }
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
private fun CenteredError(titleRes: Int, bodyRes: Int, onRetry: () -> Unit) {
    Column(
        modifier = Modifier.fillMaxSize().padding(24.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Text(
            stringResource(titleRes),
            style = MaterialTheme.typography.headlineSmall,
            textAlign = TextAlign.Center,
        )
        Text(
            stringResource(bodyRes),
            style = MaterialTheme.typography.bodyMedium,
            textAlign = TextAlign.Center,
            modifier = Modifier.padding(top = 8.dp),
        )
        Button(
            onClick = onRetry,
            modifier = Modifier.padding(top = 16.dp).heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.memory_retry)) }
    }
}

@Composable
private fun CenteredEmpty(titleRes: Int, bodyRes: Int) {
    Column(
        modifier = Modifier.fillMaxSize().padding(24.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Icon(
            imageVector = Icons.Outlined.Psychology,
            contentDescription = null,
            modifier = Modifier.size(64.dp),
            tint = MaterialTheme.colorScheme.primary,
        )
        Text(
            stringResource(titleRes),
            style = MaterialTheme.typography.headlineSmall,
            textAlign = TextAlign.Center,
            modifier = Modifier.padding(top = 16.dp),
        )
        Text(
            stringResource(bodyRes),
            style = MaterialTheme.typography.bodyMedium,
            textAlign = TextAlign.Center,
            modifier = Modifier.padding(top = 8.dp),
        )
    }
}

private fun typeLabel(type: EntityType): Int = when (type) {
    EntityType.PERSON -> R.string.memory_type_person
    EntityType.PLACE -> R.string.memory_type_place
    EntityType.INFO -> R.string.memory_type_info
    EntityType.PROJECT -> R.string.memory_type_project
    EntityType.TASK -> R.string.memory_type_task
    EntityType.PLAN -> R.string.memory_type_plan
}

/** Server wire type -> localized label; unknown types shown verbatim. */
@Composable
private fun entityTypeDisplay(wire: String): String? {
    val type = EntityType.entries.firstOrNull { it.wire == wire } ?: return wire.ifBlank { null }
    return stringResource(typeLabel(type))
}
