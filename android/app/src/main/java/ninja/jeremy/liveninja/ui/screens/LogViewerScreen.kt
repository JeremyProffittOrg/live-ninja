package ninja.jeremy.liveninja.ui.screens

import android.content.Intent
import android.widget.Toast
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.ArrowBack
import androidx.compose.material.icons.outlined.CleaningServices
import androidx.compose.material.icons.outlined.ContentCopy
import androidx.compose.material.icons.outlined.Share
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.ViewModel
import dagger.hilt.android.lifecycle.HiltViewModel
import java.text.SimpleDateFormat
import java.util.Locale
import javax.inject.Inject
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.log.LogCategory
import ninja.jeremy.liveninja.log.LogEntry
import ninja.jeremy.liveninja.log.LogExporter
import ninja.jeremy.liveninja.log.LogLevel
import ninja.jeremy.liveninja.log.LogSink

/**
 * Diagnostics in-app log viewer (04-logging §A7 secondary path). Reads
 * [LogSink]'s ring buffer live via [LogSink.entriesFlow], with a
 * view-only severity floor + category filter (independent from the
 * Diagnostics *capture* settings in M6.2 — this just controls what's
 * currently displayed, not what gets buffered/written).
 *
 * Standalone composable per plan M3.4: no navigation route wired here —
 * M6.4 (LiveNinjaRoot owner) adds the Diagnostics-section entry point.
 * Callers can host this directly, e.g. inside a Settings sub-screen or a
 * temporary debug entry point, by just calling `LogViewerScreen()`.
 */
@HiltViewModel
class LogViewerViewModel @Inject constructor(
    private val logSink: LogSink,
    private val logExporter: LogExporter,
) : ViewModel() {

    val entries: StateFlow<List<LogEntry>> get() = logSink.entriesFlow

    fun clear() = logSink.clear()

    /** Flushes + zips + mints the share Intent, or null if there's nothing to export. */
    suspend fun exportZip(): Intent? = logExporter.exportZip()
}

private val TIME_FORMAT = SimpleDateFormat("HH:mm:ss.SSS", Locale.US)

@Composable
fun LogViewerScreen(modifier: Modifier = Modifier, onBack: () -> Unit = {}) {
    val viewModel: LogViewerViewModel = hiltViewModel()
    val allEntries by viewModel.entries.collectAsState()
    val context = LocalContext.current
    val clipboard = LocalClipboardManager.current
    val coroutineScope = rememberCoroutineScope()
    val snackbarHostState = remember { SnackbarHostState() }

    // null = no floor (show everything); otherwise show this level and above.
    var levelFilter by rememberSaveable { mutableStateOf<LogLevel?>(null) }
    var categoryFilter by rememberSaveable { mutableStateOf(LogCategory.entries.toSet()) }
    var confirmClear by remember { mutableStateOf(false) }
    var exporting by remember { mutableStateOf(false) }

    val filtered = remember(allEntries, levelFilter, categoryFilter) {
        allEntries.filter { entry ->
            (levelFilter == null || entry.level.priority >= levelFilter!!.priority) &&
                entry.category in categoryFilter
        }.asReversed() // newest first
    }

    Scaffold(
        modifier = modifier.fillMaxSize(),
        snackbarHost = { SnackbarHost(snackbarHostState) },
    ) { padding ->
        Column(Modifier.fillMaxSize().padding(padding)) {
            HeaderBar(
                totalCount = allEntries.size,
                shownCount = filtered.size,
                exporting = exporting,
                onBack = onBack,
                onCopyAll = {
                    val text = filtered.asReversed().joinToString("\n") { formatLine(it) }
                    clipboard.setText(AnnotatedString(text))
                    Toast.makeText(context, "Copied ${filtered.size} entries", Toast.LENGTH_SHORT).show()
                },
                onExport = {
                    if (!exporting) {
                        exporting = true
                        coroutineScope.launch {
                            val intent = viewModel.exportZip()
                            exporting = false
                            if (intent != null) {
                                context.startActivity(Intent.createChooser(intent, "Share Live Ninja logs"))
                            } else {
                                Toast.makeText(context, "No log data to export yet", Toast.LENGTH_SHORT).show()
                            }
                        }
                    }
                },
                onClearRequested = { confirmClear = true },
            )
            HorizontalDivider()
            FilterRow(
                levelFilter = levelFilter,
                onLevelFilterChange = { levelFilter = it },
                categoryFilter = categoryFilter,
                onCategoryFilterChange = { categoryFilter = it },
            )
            HorizontalDivider()

            if (filtered.isEmpty()) {
                EmptyState(hasAnyEntries = allEntries.isNotEmpty())
            } else {
                LazyColumn(Modifier.fillMaxSize()) {
                    items(filtered, key = { it.timestampMs.toString() + it.tag + it.message.hashCode() }) { entry ->
                        LogRow(
                            entry = entry,
                            onCopy = {
                                clipboard.setText(AnnotatedString(formatLine(entry)))
                                Toast.makeText(context, "Copied", Toast.LENGTH_SHORT).show()
                            },
                        )
                        HorizontalDivider()
                    }
                }
            }
        }
    }

    if (confirmClear) {
        AlertDialog(
            onDismissRequest = { confirmClear = false },
            title = { Text("Clear logs?") },
            text = { Text("This clears the in-app log buffer. Exported/rotated files on disk are unaffected.") },
            confirmButton = {
                TextButton(onClick = {
                    viewModel.clear()
                    confirmClear = false
                }) { Text("Clear") }
            },
            dismissButton = {
                TextButton(onClick = { confirmClear = false }) { Text("Cancel") }
            },
        )
    }
}

@Composable
private fun HeaderBar(
    totalCount: Int,
    shownCount: Int,
    exporting: Boolean,
    onBack: () -> Unit,
    onCopyAll: () -> Unit,
    onExport: () -> Unit,
    onClearRequested: () -> Unit,
) {
    Row(
        modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        IconButton(onClick = onBack) {
            Icon(
                Icons.AutoMirrored.Outlined.ArrowBack,
                contentDescription = "Back",
            )
        }
        Column(Modifier.weight(1f)) {
            Text("Diagnostics Log", style = MaterialTheme.typography.titleMedium)
            Text(
                "$shownCount of $totalCount entries",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        IconButton(onClick = onCopyAll) {
            Icon(Icons.Outlined.ContentCopy, contentDescription = "Copy visible entries")
        }
        IconButton(onClick = onExport, enabled = !exporting) {
            if (exporting) {
                CircularProgressIndicator(modifier = Modifier.padding(4.dp))
            } else {
                Icon(Icons.Outlined.Share, contentDescription = "Export logs")
            }
        }
        IconButton(onClick = onClearRequested) {
            Icon(Icons.Outlined.CleaningServices, contentDescription = "Clear logs")
        }
    }
}

@Composable
private fun FilterRow(
    levelFilter: LogLevel?,
    onLevelFilterChange: (LogLevel?) -> Unit,
    categoryFilter: Set<LogCategory>,
    onCategoryFilterChange: (Set<LogCategory>) -> Unit,
) {
    Column(Modifier.fillMaxWidth().padding(vertical = 8.dp)) {
        Text(
            "Minimum level",
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.padding(horizontal = 16.dp),
        )
        LazyRow(
            horizontalArrangement = Arrangement.spacedBy(8.dp),
            contentPadding = PaddingValues(horizontal = 16.dp),
        ) {
            item {
                FilterChip(
                    selected = levelFilter == null,
                    onClick = { onLevelFilterChange(null) },
                    label = { Text("All") },
                )
            }
            items(LogLevel.entries.filter { it != LogLevel.ASSERT }) { level ->
                FilterChip(
                    selected = levelFilter == level,
                    onClick = { onLevelFilterChange(if (levelFilter == level) null else level) },
                    label = { Text(level.name.take(4)) },
                )
            }
        }
        Spacer(Modifier.width(4.dp))
        Text(
            "Categories",
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp),
        )
        LazyRow(
            horizontalArrangement = Arrangement.spacedBy(8.dp),
            contentPadding = PaddingValues(horizontal = 16.dp),
        ) {
            item {
                FilterChip(
                    selected = categoryFilter.size == LogCategory.entries.size,
                    onClick = {
                        onCategoryFilterChange(
                            if (categoryFilter.size == LogCategory.entries.size) emptySet() else LogCategory.entries.toSet(),
                        )
                    },
                    label = { Text("All") },
                )
            }
            items(LogCategory.entries) { category ->
                FilterChip(
                    selected = category in categoryFilter,
                    onClick = {
                        onCategoryFilterChange(
                            if (category in categoryFilter) categoryFilter - category else categoryFilter + category,
                        )
                    },
                    label = { Text(category.name) },
                )
            }
        }
    }
}

@Composable
private fun EmptyState(hasAnyEntries: Boolean) {
    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
        Text(
            if (hasAnyEntries) "No log entries match the current filter." else "No log entries yet.",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun LogRow(entry: LogEntry, onCopy: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clickable(onClick = onCopy)
            .padding(horizontal = 16.dp, vertical = 6.dp),
    ) {
        Column {
            Row {
                Text(
                    TIME_FORMAT.format(entry.timestampMs),
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Spacer(Modifier.width(8.dp))
                Text(
                    entry.level.name,
                    style = MaterialTheme.typography.labelSmall,
                    color = levelColor(entry.level),
                )
                Spacer(Modifier.width(8.dp))
                Text(
                    entry.category.name,
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Text(
                "${entry.tag}: ${entry.message}",
                style = MaterialTheme.typography.bodySmall,
            )
        }
    }
}

@Composable
private fun levelColor(level: LogLevel) = when (level) {
    LogLevel.ERROR, LogLevel.ASSERT -> MaterialTheme.colorScheme.error
    LogLevel.WARN -> MaterialTheme.colorScheme.tertiary
    else -> MaterialTheme.colorScheme.onSurfaceVariant
}

/** `ts|level|category|tag: message` — matches LogSinkCore's on-disk format for copy/paste parity. */
private fun formatLine(entry: LogEntry): String =
    "${entry.timestampMs}|${entry.level.name}|${entry.category.name}|${entry.tag}: ${entry.message}"
