package ninja.jeremy.liveninja.ui.files

import android.app.DownloadManager
import android.content.Context
import android.net.Uri
import android.os.Environment
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import java.time.Instant
import java.time.OffsetDateTime
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.time.format.FormatStyle
import java.util.Locale
import javax.inject.Inject
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.net.DeliverableDto

/** One row in the Files tab. */
data class DeliverableUi(
    val id: String,
    val name: String,
    val contentType: String,
    val sizeLabel: String?,
    val dateLabel: String?,
    /** Non-null while an async zip is still being produced ("pending"/"processing"). */
    val pending: Boolean,
)

/** Snackbar-level notices (mapped to string resources by the screen). */
enum class FilesNotice {
    DOWNLOAD_STARTED,
    DOWNLOAD_FAILED,
    ZIP_REQUESTED,
    ZIP_FAILED,
    DELETED,
    DELETE_FAILED,
    SHARE_FAILED,
}

/** One-shot events that need an Activity context to act on. */
sealed interface FilesEvent {
    /** Fire an ACTION_SEND chooser with this text (presigned link lines). */
    data class Share(val text: String) : FilesEvent
}

data class FilesUiState(
    /** Initial-load / refresh in flight with nothing usable on screen yet. */
    val loading: Boolean = false,
    /** First page has been fetched at least once (gates empty-state vs loading). */
    val loaded: Boolean = false,
    val items: List<DeliverableUi> = emptyList(),
    val error: Boolean = false,
    val nextCursor: String? = null,
    val loadingMore: Boolean = false,
    /** Multi-select set; non-empty == selection mode (FR-DLV-05 zip/share). */
    val selected: Set<String> = emptySet(),
    /** A zip/share/delete batch action is running. */
    val actionInProgress: Boolean = false,
)

/**
 * Files tab state: lists the caller's deliverables (`GET /api/v1/deliverables`,
 * Query-paginated), downloads via presigned URL through [DownloadManager],
 * multi-selects into server-side zip, and shares presigned links.
 */
@HiltViewModel
class FilesViewModel @Inject constructor(
    @ApplicationContext private val context: Context,
    private val repository: DeliverablesRepository,
) : ViewModel() {

    private val _state = MutableStateFlow(FilesUiState())
    val state: StateFlow<FilesUiState> = _state

    private val _notices = MutableSharedFlow<FilesNotice>(extraBufferCapacity = 8)
    val notices: SharedFlow<FilesNotice> = _notices

    private val _events = MutableSharedFlow<FilesEvent>(extraBufferCapacity = 4)
    val events: SharedFlow<FilesEvent> = _events

    /** Load the first page if it was never fetched (screen entry). */
    fun loadIfNeeded() {
        if (!_state.value.loaded && !_state.value.loading) refresh()
    }

    fun refresh() {
        _state.update { it.copy(loading = true, error = false) }
        viewModelScope.launch {
            try {
                val page = repository.list()
                _state.update {
                    it.copy(
                        loading = false,
                        loaded = true,
                        error = false,
                        items = page.items.map(::toUi),
                        nextCursor = page.nextCursor,
                        // Drop selections pointing at rows that vanished.
                        selected = it.selected intersect page.items.map { d -> d.id }.toSet(),
                    )
                }
            } catch (e: Exception) {
                _state.update { it.copy(loading = false, error = true) }
            }
        }
    }

    fun loadMore() {
        val cursor = _state.value.nextCursor ?: return
        if (_state.value.loadingMore) return
        _state.update { it.copy(loadingMore = true) }
        viewModelScope.launch {
            try {
                val page = repository.list(cursor = cursor)
                _state.update {
                    it.copy(
                        loadingMore = false,
                        items = it.items + page.items.map(::toUi),
                        nextCursor = page.nextCursor,
                    )
                }
            } catch (e: Exception) {
                // Keep what we have; the "load more" row stays available to retry.
                _state.update { it.copy(loadingMore = false) }
            }
        }
    }

    // ---- selection ----

    fun toggleSelected(id: String) = _state.update {
        it.copy(selected = if (id in it.selected) it.selected - id else it.selected + id)
    }

    fun clearSelection() = _state.update { it.copy(selected = emptySet()) }

    // ---- row tap: download ----

    /**
     * Resolve the presigned URL, then hand it to [DownloadManager] — the URL
     * carries query-string auth, so the system downloader needs no headers.
     */
    fun download(item: DeliverableUi) {
        viewModelScope.launch {
            try {
                val url = repository.resolveDownloadUrl(item.id)
                val fileName = safeFileName(item.name)
                val request = DownloadManager.Request(Uri.parse(url))
                    .setTitle(item.name)
                    .setMimeType(item.contentType.ifBlank { null })
                    .setNotificationVisibility(
                        DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED,
                    )
                    .setDestinationInExternalPublicDir(
                        Environment.DIRECTORY_DOWNLOADS,
                        fileName,
                    )
                val dm = context.getSystemService(Context.DOWNLOAD_SERVICE) as DownloadManager
                dm.enqueue(request)
                _notices.tryEmit(FilesNotice.DOWNLOAD_STARTED)
            } catch (e: Exception) {
                _notices.tryEmit(FilesNotice.DOWNLOAD_FAILED)
            }
        }
    }

    // ---- batch actions on the selection ----

    /** Server-side zip of the selection; the new ZIP shows up on refresh. */
    fun zipSelected() {
        val ids = _state.value.selected.toList()
        if (ids.isEmpty() || _state.value.actionInProgress) return
        _state.update { it.copy(actionInProgress = true) }
        viewModelScope.launch {
            try {
                repository.zip(ids)
                _notices.tryEmit(FilesNotice.ZIP_REQUESTED)
                _state.update { it.copy(actionInProgress = false, selected = emptySet()) }
                refresh()
            } catch (e: Exception) {
                _notices.tryEmit(FilesNotice.ZIP_FAILED)
                _state.update { it.copy(actionInProgress = false) }
            }
        }
    }

    /** Mint presigned links for the selection and hand them to ACTION_SEND. */
    fun shareSelected() {
        val current = _state.value
        val chosen = current.items.filter { it.id in current.selected }
        if (chosen.isEmpty() || current.actionInProgress) return
        _state.update { it.copy(actionInProgress = true) }
        viewModelScope.launch {
            try {
                val lines = chosen.map { item ->
                    val url = repository.resolveDownloadUrl(item.id)
                    if (chosen.size == 1) url else "${item.name}: $url"
                }
                _events.tryEmit(FilesEvent.Share(lines.joinToString("\n")))
                _state.update { it.copy(actionInProgress = false, selected = emptySet()) }
            } catch (e: Exception) {
                _notices.tryEmit(FilesNotice.SHARE_FAILED)
                _state.update { it.copy(actionInProgress = false) }
            }
        }
    }

    /** Delete the selection (screen guards this behind a confirm dialog). */
    fun deleteSelected() {
        val ids = _state.value.selected.toList()
        if (ids.isEmpty() || _state.value.actionInProgress) return
        _state.update { it.copy(actionInProgress = true) }
        viewModelScope.launch {
            var failed = false
            for (id in ids) {
                try {
                    repository.delete(id)
                } catch (e: Exception) {
                    failed = true
                }
            }
            _notices.tryEmit(if (failed) FilesNotice.DELETE_FAILED else FilesNotice.DELETED)
            _state.update { it.copy(actionInProgress = false, selected = emptySet()) }
            refresh()
        }
    }

    // ---- mapping / formatting ----

    private fun toUi(dto: DeliverableDto) = DeliverableUi(
        id = dto.id,
        name = dto.displayName,
        contentType = dto.contentType.orEmpty(),
        sizeLabel = dto.sizeBytes?.takeIf { it >= 0 }?.let(::formatSize),
        dateLabel = dto.createdAt?.let(::formatDate),
        pending = dto.status != null && dto.status != "ready",
    )

    private fun formatSize(bytes: Long): String = when {
        bytes < 1024 -> "$bytes B"
        bytes < 1024 * 1024 -> String.format(Locale.US, "%.1f KB", bytes / 1024.0)
        bytes < 1024L * 1024 * 1024 -> String.format(Locale.US, "%.1f MB", bytes / (1024.0 * 1024))
        else -> String.format(Locale.US, "%.1f GB", bytes / (1024.0 * 1024 * 1024))
    }

    private fun formatDate(iso: String): String? = runCatching {
        val instant = if (iso.all { it.isDigit() }) {
            // Tolerate epoch seconds/millis just in case.
            val n = iso.toLong()
            if (n > 100_000_000_000L) Instant.ofEpochMilli(n) else Instant.ofEpochSecond(n)
        } else {
            OffsetDateTime.parse(iso).toInstant()
        }
        DateTimeFormatter.ofLocalizedDate(FormatStyle.MEDIUM)
            .withLocale(Locale.getDefault())
            .format(instant.atZone(ZoneId.systemDefault()).toLocalDate())
    }.getOrNull()

    private fun safeFileName(name: String): String {
        val cleaned = name.replace(Regex("""[\\/:*?"<>|]"""), "_").trim()
        return cleaned.ifBlank { "deliverable" }
    }
}
