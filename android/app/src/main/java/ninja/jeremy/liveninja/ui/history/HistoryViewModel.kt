package ninja.jeremy.liveninja.ui.history

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dagger.hilt.android.lifecycle.HiltViewModel
import java.time.Instant
import java.time.OffsetDateTime
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.time.format.FormatStyle
import java.util.Locale
import javax.inject.Inject
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.net.ConversationDto

/** One topic chip (on conversation rows and in the filter sheet). */
data class TopicUi(
    val id: String,
    val name: String,
    /** Hex #RRGGBB from the Topic Manager; null = theme default dot. */
    val colorHex: String?,
    val archived: Boolean,
)

/** One device option in the filter dropdown. */
data class DeviceUi(
    val id: String,
    val name: String,
)

/** One row in the history list. */
data class ConversationUi(
    val id: String,
    val title: String?,
    val dateLabel: String?,
    /** device · engine · duration, pre-joined. */
    val metaLabel: String?,
    val topics: List<TopicUi>,
)

/** One transcript line in the detail view. */
data class TurnUi(
    val isUser: Boolean,
    val text: String,
)

/** Loaded detail (conversation + transcript). */
data class ConversationDetailUi(
    val id: String,
    val title: String?,
    val dateLabel: String?,
    val metaLabel: String?,
    val topics: List<TopicUi>,
    val turns: List<TurnUi>,
)

data class HistoryUiState(
    val loading: Boolean = false,
    val loaded: Boolean = false,
    val error: Boolean = false,
    val items: List<ConversationUi> = emptyList(),
    val nextCursor: String? = null,
    val loadingMore: Boolean = false,
    /** Applied filters (drive the server Query). */
    val filters: HistoryFilters = HistoryFilters(),
    /** Filter sheet option sources — populated pickers, never blind text. */
    val topics: List<TopicUi> = emptyList(),
    val devices: List<DeviceUi> = emptyList(),
    val filterSheetOpen: Boolean = false,
    // Detail view (in-screen, back-handled)
    val detailId: String? = null,
    val detailLoading: Boolean = false,
    val detailError: Boolean = false,
    val detail: ConversationDetailUi? = null,
)

/**
 * History tab state: the filterable conversation list (FR-TOP-04/05).
 * Filters — topic multi-select, device dropdown, date range — are populated
 * from `GET /api/v1/topics` and `GET /api/v1/devices`; applying them re-runs
 * the server-side Query. Tapping a row loads the transcript detail in-screen.
 */
@HiltViewModel
class HistoryViewModel @Inject constructor(
    private val repository: HistoryRepository,
) : ViewModel() {

    private val _state = MutableStateFlow(HistoryUiState())
    val state: StateFlow<HistoryUiState> = _state

    /** Screen entry: first page + filter option sources. */
    fun loadIfNeeded() {
        val s = _state.value
        if (!s.loaded && !s.loading) refresh()
        if (s.topics.isEmpty()) loadTopics()
        if (s.devices.isEmpty()) loadDevices()
    }

    fun refresh() {
        _state.update { it.copy(loading = true, error = false) }
        viewModelScope.launch {
            try {
                val page = repository.listConversations(_state.value.filters)
                _state.update {
                    it.copy(
                        loading = false,
                        loaded = true,
                        error = false,
                        items = page.items.mapNotNull { c -> toUi(c, it.topics, it.devices) },
                        nextCursor = page.nextCursor,
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
                val page = repository.listConversations(_state.value.filters, cursor = cursor)
                _state.update {
                    it.copy(
                        loadingMore = false,
                        items = it.items + page.items.mapNotNull { c -> toUi(c, it.topics, it.devices) },
                        nextCursor = page.nextCursor,
                    )
                }
            } catch (e: Exception) {
                _state.update { it.copy(loadingMore = false) }
            }
        }
    }

    private fun loadTopics() {
        viewModelScope.launch {
            try {
                val topics = repository.listTopics().items.mapNotNull { dto ->
                    val id = dto.topicKey ?: return@mapNotNull null
                    // Merged topics point at their survivor; hide them here.
                    if (dto.mergedInto != null) return@mapNotNull null
                    TopicUi(
                        id = id,
                        name = dto.displayName,
                        colorHex = dto.color,
                        archived = dto.archived,
                    )
                }
                _state.update { s ->
                    s.copy(
                        topics = topics,
                        // Re-label already-loaded rows now that names are known.
                        items = s.items.map { row -> row.copy(topics = resolveTopics(rowTopicIds(row), topics)) },
                    )
                }
            } catch (e: Exception) {
                // Chips fall back to bare ids; the list itself still works.
            }
        }
    }

    private fun loadDevices() {
        viewModelScope.launch {
            try {
                val devices = repository.listDevices().items.mapNotNull { dto ->
                    val id = dto.deviceKey ?: return@mapNotNull null
                    DeviceUi(id = id, name = dto.displayName)
                }
                _state.update { it.copy(devices = devices) }
            } catch (e: Exception) {
                // Device filter shows "All devices" only; list still works.
            }
        }
    }

    // ---- filters ----

    fun openFilterSheet() = _state.update { it.copy(filterSheetOpen = true) }

    fun closeFilterSheet() = _state.update { it.copy(filterSheetOpen = false) }

    fun toggleTopicFilter(topicId: String) {
        _state.update {
            val next = if (topicId in it.filters.topicIds) {
                it.filters.topicIds - topicId
            } else {
                it.filters.topicIds + topicId
            }
            it.copy(filters = it.filters.copy(topicIds = next))
        }
    }

    fun setDeviceFilter(deviceId: String?) =
        _state.update { it.copy(filters = it.filters.copy(deviceId = deviceId)) }

    fun setDateRange(fromMillis: Long?, toMillis: Long?) =
        _state.update { it.copy(filters = it.filters.copy(fromMillis = fromMillis, toMillis = toMillis)) }

    fun clearFilters() {
        _state.update { it.copy(filters = HistoryFilters()) }
        refresh()
    }

    /** Filter sheet "Apply": close + re-Query with the current facets. */
    fun applyFilters() {
        _state.update { it.copy(filterSheetOpen = false) }
        refresh()
    }

    // ---- detail ----

    fun openDetail(id: String) {
        _state.update { it.copy(detailId = id, detailLoading = true, detailError = false, detail = null) }
        viewModelScope.launch {
            try {
                val dto = repository.getConversation(id)
                val s = _state.value
                if (s.detailId != id) return@launch // navigated away meanwhile
                val deviceName = dto.deviceName
                    ?: s.devices.firstOrNull { d -> d.id == dto.deviceId }?.name
                    ?: dto.deviceId
                _state.update {
                    it.copy(
                        detailLoading = false,
                        detail = ConversationDetailUi(
                            id = id,
                            title = dto.displayTitle,
                            dateLabel = dto.startTimestamp?.let(::formatDateTime),
                            metaLabel = metaLabel(deviceName, dto.engine, dto.duration),
                            topics = resolveTopics(dto.topicIds, it.topics),
                            turns = dto.allTurns
                                .filter { t -> t.displayText.isNotBlank() }
                                .map { t -> TurnUi(isUser = t.isUser, text = t.displayText) },
                        ),
                    )
                }
            } catch (e: Exception) {
                if (_state.value.detailId == id) {
                    _state.update { it.copy(detailLoading = false, detailError = true) }
                }
            }
        }
    }

    fun closeDetail() =
        _state.update { it.copy(detailId = null, detail = null, detailLoading = false, detailError = false) }

    // ---- mapping / formatting ----

    private fun rowTopicIds(row: ConversationUi): List<String> = row.topics.map { it.id }

    private fun resolveTopics(ids: List<String>, topics: List<TopicUi>): List<TopicUi> =
        ids.map { id ->
            topics.firstOrNull { it.id == id }
                ?: TopicUi(id = id, name = id, colorHex = null, archived = false)
        }

    private fun toUi(
        dto: ConversationDto,
        topics: List<TopicUi>,
        devices: List<DeviceUi>,
    ): ConversationUi? {
        val id = dto.conversationKey ?: return null
        val deviceName = dto.deviceName
            ?: devices.firstOrNull { it.id == dto.deviceId }?.name
            ?: dto.deviceId
        return ConversationUi(
            id = id,
            title = dto.displayTitle,
            dateLabel = dto.startTimestamp?.let(::formatDateTime),
            metaLabel = metaLabel(deviceName, dto.engine, dto.duration),
            topics = resolveTopics(dto.topicIds, topics),
        )
    }

    private fun metaLabel(deviceName: String?, engine: String?, durationSec: Long?): String? {
        val parts = listOfNotNull(
            deviceName?.takeIf { it.isNotBlank() },
            engine?.takeIf { it.isNotBlank() },
            durationSec?.takeIf { it > 0 }?.let(::formatDuration),
        )
        return parts.joinToString(" · ").ifBlank { null }
    }

    private fun formatDuration(seconds: Long): String {
        val m = seconds / 60
        val s = seconds % 60
        return if (m > 0) String.format(Locale.US, "%dm %02ds", m, s) else "${s}s"
    }

    private fun formatDateTime(iso: String): String? = runCatching {
        val instant = if (iso.all { it.isDigit() }) {
            val n = iso.toLong()
            if (n > 100_000_000_000L) Instant.ofEpochMilli(n) else Instant.ofEpochSecond(n)
        } else {
            OffsetDateTime.parse(iso).toInstant()
        }
        DateTimeFormatter.ofLocalizedDateTime(FormatStyle.MEDIUM, FormatStyle.SHORT)
            .withLocale(Locale.getDefault())
            .format(instant.atZone(ZoneId.systemDefault()))
    }.getOrNull()
}
