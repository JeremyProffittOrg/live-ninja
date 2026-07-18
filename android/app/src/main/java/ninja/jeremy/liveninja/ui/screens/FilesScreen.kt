@file:OptIn(ExperimentalFoundationApi::class)

package ninja.jeremy.liveninja.ui.screens

import android.content.Intent
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.outlined.Code
import androidx.compose.material.icons.outlined.Delete
import androidx.compose.material.icons.outlined.Description
import androidx.compose.material.icons.outlined.FolderOpen
import androidx.compose.material.icons.outlined.FolderZip
import androidx.compose.material.icons.outlined.Image
import androidx.compose.material.icons.outlined.InsertDriveFile
import androidx.compose.material.icons.outlined.PictureAsPdf
import androidx.compose.material.icons.outlined.Refresh
import androidx.compose.material.icons.outlined.Share
import androidx.compose.material.icons.outlined.TableChart
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.ui.files.DeliverableUi
import ninja.jeremy.liveninja.ui.files.FilesEvent
import ninja.jeremy.liveninja.ui.files.FilesNotice
import ninja.jeremy.liveninja.ui.files.FilesViewModel

/**
 * Files tab — the Android face of the M9 Deliverables Store (FR-DLV-05),
 * mirroring the web Download Center over the same `GET /api/v1/deliverables`
 * Query. Tap a row to download it via presigned URL + DownloadManager;
 * long-press to multi-select for server-side zip, presigned-link share
 * (ACTION_SEND), or delete (guarded by a confirm dialog). Explicit loading /
 * empty / error states per the rich-UI data-presentation rules.
 */
@Composable
fun FilesScreen(modifier: Modifier = Modifier) {
    val viewModel: FilesViewModel = hiltViewModel()
    val state by viewModel.state.collectAsState()
    val snackbarHostState = remember { SnackbarHostState() }
    val context = LocalContext.current
    var confirmDelete by remember { mutableStateOf(false) }

    LaunchedEffect(Unit) { viewModel.loadIfNeeded() }

    LaunchedEffect(Unit) {
        viewModel.notices.collect { notice ->
            val res = when (notice) {
                FilesNotice.DOWNLOAD_STARTED -> R.string.files_download_started
                FilesNotice.DOWNLOAD_FAILED -> R.string.files_download_failed
                FilesNotice.ZIP_REQUESTED -> R.string.files_zip_requested
                FilesNotice.ZIP_FAILED -> R.string.files_zip_failed
                FilesNotice.DELETED -> R.string.files_deleted
                FilesNotice.DELETE_FAILED -> R.string.files_delete_failed
                FilesNotice.SHARE_FAILED -> R.string.files_share_failed
            }
            snackbarHostState.showSnackbar(context.getString(res))
        }
    }

    LaunchedEffect(Unit) {
        viewModel.events.collect { event ->
            when (event) {
                is FilesEvent.Share -> {
                    val send = Intent(Intent.ACTION_SEND).apply {
                        type = "text/plain"
                        putExtra(Intent.EXTRA_TEXT, event.text)
                    }
                    context.startActivity(
                        Intent.createChooser(
                            send,
                            context.getString(R.string.files_share_chooser_title),
                        ),
                    )
                }
            }
        }
    }

    Scaffold(
        modifier = modifier.fillMaxSize(),
        snackbarHost = { SnackbarHost(snackbarHostState) },
    ) { padding ->
        Column(Modifier.fillMaxSize().padding(padding)) {
            if (state.selected.isEmpty()) {
                HeaderBar(
                    loading = state.loading,
                    onRefresh = viewModel::refresh,
                )
            } else {
                SelectionBar(
                    count = state.selected.size,
                    busy = state.actionInProgress,
                    onZip = viewModel::zipSelected,
                    onShare = viewModel::shareSelected,
                    onDelete = { confirmDelete = true },
                    onClear = viewModel::clearSelection,
                )
            }
            HorizontalDivider()

            when {
                state.loading && !state.loaded -> LoadingState()
                state.error && state.items.isEmpty() -> ErrorState(onRetry = viewModel::refresh)
                state.loaded && state.items.isEmpty() -> EmptyState()
                else -> DeliverableList(
                    items = state.items,
                    selected = state.selected,
                    selectionMode = state.selected.isNotEmpty(),
                    hasMore = state.nextCursor != null,
                    loadingMore = state.loadingMore,
                    onTap = { item ->
                        if (state.selected.isNotEmpty()) {
                            viewModel.toggleSelected(item.id)
                        } else {
                            viewModel.download(item)
                        }
                    },
                    onLongPress = { item -> viewModel.toggleSelected(item.id) },
                    onLoadMore = viewModel::loadMore,
                )
            }
        }
    }

    if (confirmDelete) {
        AlertDialog(
            onDismissRequest = { confirmDelete = false },
            title = { Text(stringResource(R.string.files_delete_confirm_title)) },
            text = {
                Text(stringResource(R.string.files_delete_confirm_body, state.selected.size))
            },
            confirmButton = {
                Button(
                    onClick = {
                        confirmDelete = false
                        viewModel.deleteSelected()
                    },
                    modifier = Modifier.heightIn(min = 48.dp),
                ) { Text(stringResource(R.string.files_action_delete)) }
            },
            dismissButton = {
                TextButton(
                    onClick = { confirmDelete = false },
                    modifier = Modifier.heightIn(min = 48.dp),
                ) { Text(stringResource(R.string.dialog_cancel)) }
            },
        )
    }
}

@Composable
private fun HeaderBar(loading: Boolean, onRefresh: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(start = 16.dp, end = 4.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(
            stringResource(R.string.files_title),
            style = MaterialTheme.typography.titleLarge,
            modifier = Modifier.weight(1f),
        )
        if (loading) {
            CircularProgressIndicator(
                modifier = Modifier.padding(12.dp).size(24.dp),
                strokeWidth = 2.dp,
            )
        } else {
            IconButton(onClick = onRefresh, modifier = Modifier.size(48.dp)) {
                Icon(
                    Icons.Outlined.Refresh,
                    contentDescription = stringResource(R.string.files_refresh),
                )
            }
        }
    }
}

@Composable
private fun SelectionBar(
    count: Int,
    busy: Boolean,
    onZip: () -> Unit,
    onShare: () -> Unit,
    onDelete: () -> Unit,
    onClear: () -> Unit,
) {
    Surface(color = MaterialTheme.colorScheme.secondaryContainer) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .padding(start = 4.dp, end = 4.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            IconButton(onClick = onClear, modifier = Modifier.size(48.dp)) {
                Icon(
                    Icons.Filled.Close,
                    contentDescription = stringResource(R.string.files_action_clear_selection),
                )
            }
            Text(
                stringResource(R.string.files_selected_count, count),
                style = MaterialTheme.typography.titleMedium,
                modifier = Modifier.weight(1f),
            )
            if (busy) {
                CircularProgressIndicator(
                    modifier = Modifier.padding(12.dp).size(24.dp),
                    strokeWidth = 2.dp,
                )
            } else {
                IconButton(onClick = onZip, modifier = Modifier.size(48.dp)) {
                    Icon(
                        Icons.Outlined.FolderZip,
                        contentDescription = stringResource(R.string.files_action_zip),
                    )
                }
                IconButton(onClick = onShare, modifier = Modifier.size(48.dp)) {
                    Icon(
                        Icons.Outlined.Share,
                        contentDescription = stringResource(R.string.files_action_share),
                    )
                }
                IconButton(onClick = onDelete, modifier = Modifier.size(48.dp)) {
                    Icon(
                        Icons.Outlined.Delete,
                        contentDescription = stringResource(R.string.files_action_delete),
                    )
                }
            }
        }
    }
}

@Composable
private fun DeliverableList(
    items: List<DeliverableUi>,
    selected: Set<String>,
    selectionMode: Boolean,
    hasMore: Boolean,
    loadingMore: Boolean,
    onTap: (DeliverableUi) -> Unit,
    onLongPress: (DeliverableUi) -> Unit,
    onLoadMore: () -> Unit,
) {
    LazyColumn(
        modifier = Modifier.fillMaxSize(),
        contentPadding = androidx.compose.foundation.layout.PaddingValues(
            horizontal = 16.dp,
            vertical = 8.dp,
        ),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        items(items, key = { it.id }) { item ->
            DeliverableCard(
                item = item,
                selected = item.id in selected,
                selectionMode = selectionMode,
                onTap = { onTap(item) },
                onLongPress = { onLongPress(item) },
            )
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
                        ) { Text(stringResource(R.string.files_load_more)) }
                    }
                }
            }
        }
    }
}

@Composable
private fun DeliverableCard(
    item: DeliverableUi,
    selected: Boolean,
    selectionMode: Boolean,
    onTap: () -> Unit,
    onLongPress: () -> Unit,
) {
    Card(
        modifier = Modifier
            .fillMaxWidth()
            .combinedClickable(onClick = onTap, onLongClick = onLongPress),
    ) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .heightIn(min = 64.dp)
                .padding(horizontal = 16.dp, vertical = 12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Icon(
                imageVector = iconFor(item),
                contentDescription = null,
                tint = MaterialTheme.colorScheme.primary,
                modifier = Modifier.size(28.dp),
            )
            Column(
                Modifier
                    .weight(1f)
                    .padding(start = 16.dp),
            ) {
                Text(
                    item.name,
                    style = MaterialTheme.typography.bodyLarge,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
                val meta = listOfNotNull(
                    item.sizeLabel,
                    item.dateLabel,
                    if (item.pending) stringResource(R.string.files_status_pending) else null,
                ).joinToString(" · ")
                if (meta.isNotBlank()) {
                    Text(
                        meta,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            Spacer(Modifier.width(8.dp))
            if (selectionMode) {
                // Checkbox mirrors the card's toggle; the whole row is the target.
                Checkbox(checked = selected, onCheckedChange = null)
            }
        }
    }
}

private fun iconFor(item: DeliverableUi): ImageVector {
    val ct = item.contentType.lowercase()
    val name = item.name.lowercase()
    return when {
        ct.startsWith("image/") -> Icons.Outlined.Image
        ct.contains("zip") || name.endsWith(".zip") -> Icons.Outlined.FolderZip
        ct.contains("pdf") || name.endsWith(".pdf") -> Icons.Outlined.PictureAsPdf
        ct.contains("csv") || name.endsWith(".csv") -> Icons.Outlined.TableChart
        ct.contains("json") || name.endsWith(".json") -> Icons.Outlined.Code
        ct.startsWith("text/") || name.endsWith(".md") || name.endsWith(".html") ->
            Icons.Outlined.Description
        else -> Icons.Outlined.InsertDriveFile
    }
}

@Composable
private fun LoadingState() {
    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
        CircularProgressIndicator()
    }
}

@Composable
private fun ErrorState(onRetry: () -> Unit) {
    Column(
        modifier = Modifier
            .fillMaxSize()
            .padding(24.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Text(
            stringResource(R.string.files_error_title),
            style = MaterialTheme.typography.headlineSmall,
            textAlign = TextAlign.Center,
        )
        Text(
            stringResource(R.string.files_error_body),
            style = MaterialTheme.typography.bodyMedium,
            textAlign = TextAlign.Center,
            modifier = Modifier.padding(top = 8.dp),
        )
        Button(
            onClick = onRetry,
            modifier = Modifier
                .padding(top = 16.dp)
                .heightIn(min = 48.dp),
        ) { Text(stringResource(R.string.files_retry)) }
    }
}

@Composable
private fun EmptyState() {
    Column(
        modifier = Modifier
            .fillMaxSize()
            .padding(24.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Icon(
            imageVector = Icons.Outlined.FolderOpen,
            contentDescription = null,
            modifier = Modifier.size(64.dp),
            tint = MaterialTheme.colorScheme.primary,
        )
        Text(
            text = stringResource(R.string.files_empty_title),
            style = MaterialTheme.typography.headlineSmall,
            textAlign = TextAlign.Center,
            modifier = Modifier.padding(top = 16.dp),
        )
        Text(
            text = stringResource(R.string.files_empty_body),
            style = MaterialTheme.typography.bodyMedium,
            textAlign = TextAlign.Center,
            modifier = Modifier.padding(top = 8.dp),
        )
    }
}
