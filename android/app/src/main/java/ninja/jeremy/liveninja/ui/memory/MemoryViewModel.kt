package ninja.jeremy.liveninja.ui.memory

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
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.net.EntityDto
import ninja.jeremy.liveninja.net.GuideDto

/**
 * The six M10 entity types (sort-key discriminator in ENT#<type>#<id>).
 * Fixed, enumerable set → the type filter is chips populated from this enum,
 * never free text.
 */
enum class EntityType(val wire: String) {
    PERSON("person"),
    PLACE("place"),
    INFO("info"),
    PROJECT("project"),
    TASK("task"),
    PLAN("plan"),
}

/** One row in the entities list. */
data class EntityUi(
    val id: String,
    val name: String,
    val type: String,
    val updatedLabel: String?,
    val attrLines: List<Pair<String, String>>,
    val relationCount: Int,
)

/** One row in the guides list (FR-MEM-09). */
data class GuideUi(
    val id: String,
    val title: String,
    val text: String,
    val enabled: Boolean,
    val priority: Int,
    /** A PUT for this guide is in flight (disables its controls). */
    val busy: Boolean = false,
    /** Source DTO — resent in full on PUT (replace semantics + version). */
    val dto: GuideDto,
)

/** Snackbar-level notices (mapped to string resources by the screen). */
enum class MemoryNotice {
    FORGOTTEN,
    FORGET_FAILED,
    GUIDE_UPDATE_FAILED,
}

data class MemoryUiState(
    // Entities tab
    val entitiesLoading: Boolean = false,
    val entitiesLoaded: Boolean = false,
    val entitiesError: Boolean = false,
    val entities: List<EntityUi> = emptyList(),
    val entitiesCursor: String? = null,
    val entitiesLoadingMore: Boolean = false,
    /** null = all types. */
    val typeFilter: EntityType? = null,
    /** Entity shown in the read-only detail dialog. */
    val detail: EntityUi? = null,
    /** Entity id awaiting the forget confirmation dialog. */
    val confirmForget: EntityUi? = null,
    val forgetInProgress: Boolean = false,
    // Guides tab
    val guidesLoading: Boolean = false,
    val guidesLoaded: Boolean = false,
    val guidesError: Boolean = false,
    val guides: List<GuideUi> = emptyList(),
)

/**
 * Memory tab state: entity browser with type filter + forget (FR-MEM-05,
 * dual-store delete server-side) and the Guide Entities manager with enable
 * toggles + priority steppers (FR-MEM-09). Everything is Query-backed
 * server-side; the type filter chips come from [EntityType].
 */
@HiltViewModel
class MemoryViewModel @Inject constructor(
    private val repository: MemoryRepository,
) : ViewModel() {

    private val _state = MutableStateFlow(MemoryUiState())
    val state: StateFlow<MemoryUiState> = _state

    private val _notices = MutableSharedFlow<MemoryNotice>(extraBufferCapacity = 8)
    val notices: SharedFlow<MemoryNotice> = _notices

    /** Screen entry: fetch both tabs once. */
    fun loadIfNeeded() {
        val s = _state.value
        if (!s.entitiesLoaded && !s.entitiesLoading) refreshEntities()
        if (!s.guidesLoaded && !s.guidesLoading) refreshGuides()
    }

    // ---- entities ----

    fun setTypeFilter(type: EntityType?) {
        if (_state.value.typeFilter == type) return
        _state.update { it.copy(typeFilter = type) }
        refreshEntities()
    }

    fun refreshEntities() {
        _state.update { it.copy(entitiesLoading = true, entitiesError = false) }
        viewModelScope.launch {
            try {
                val page = repository.listEntities(type = _state.value.typeFilter?.wire)
                _state.update {
                    it.copy(
                        entitiesLoading = false,
                        entitiesLoaded = true,
                        entitiesError = false,
                        entities = page.items.mapNotNull(::toUi),
                        entitiesCursor = page.nextCursor,
                    )
                }
            } catch (e: Exception) {
                _state.update { it.copy(entitiesLoading = false, entitiesError = true) }
            }
        }
    }

    fun loadMoreEntities() {
        val cursor = _state.value.entitiesCursor ?: return
        if (_state.value.entitiesLoadingMore) return
        _state.update { it.copy(entitiesLoadingMore = true) }
        viewModelScope.launch {
            try {
                val page = repository.listEntities(
                    type = _state.value.typeFilter?.wire,
                    cursor = cursor,
                )
                _state.update {
                    it.copy(
                        entitiesLoadingMore = false,
                        entities = it.entities + page.items.mapNotNull(::toUi),
                        entitiesCursor = page.nextCursor,
                    )
                }
            } catch (e: Exception) {
                // Keep what we have; the "load more" row stays available to retry.
                _state.update { it.copy(entitiesLoadingMore = false) }
            }
        }
    }

    fun showDetail(entity: EntityUi) = _state.update { it.copy(detail = entity) }

    fun closeDetail() = _state.update { it.copy(detail = null) }

    /** Ask for confirmation — forget is irreversible (entity + embedding). */
    fun requestForget(entity: EntityUi) = _state.update { it.copy(confirmForget = entity) }

    fun cancelForget() = _state.update { it.copy(confirmForget = null) }

    fun confirmForget() {
        val entity = _state.value.confirmForget ?: return
        if (_state.value.forgetInProgress) return
        _state.update { it.copy(forgetInProgress = true) }
        viewModelScope.launch {
            try {
                repository.forget(entity.id)
                _notices.tryEmit(MemoryNotice.FORGOTTEN)
                _state.update {
                    it.copy(
                        forgetInProgress = false,
                        confirmForget = null,
                        detail = if (it.detail?.id == entity.id) null else it.detail,
                        entities = it.entities.filterNot { e -> e.id == entity.id },
                    )
                }
            } catch (e: Exception) {
                _notices.tryEmit(MemoryNotice.FORGET_FAILED)
                _state.update { it.copy(forgetInProgress = false, confirmForget = null) }
            }
        }
    }

    // ---- guides ----

    fun refreshGuides() {
        _state.update { it.copy(guidesLoading = true, guidesError = false) }
        viewModelScope.launch {
            try {
                val response = repository.listGuides()
                _state.update {
                    it.copy(
                        guidesLoading = false,
                        guidesLoaded = true,
                        guidesError = false,
                        guides = response.items.mapNotNull(::toUi)
                            .sortedBy { g -> g.priority },
                    )
                }
            } catch (e: Exception) {
                _state.update { it.copy(guidesLoading = false, guidesError = true) }
            }
        }
    }

    fun setGuideEnabled(guide: GuideUi, enabled: Boolean) =
        saveGuide(guide, enabled = enabled, priority = guide.priority)

    /** Priority stepper: lower value = injected earlier (priority ascending). */
    fun adjustGuidePriority(guide: GuideUi, delta: Int) {
        val next = (guide.priority + delta).coerceAtLeast(0)
        if (next == guide.priority) return
        saveGuide(guide, enabled = guide.enabled, priority = next)
    }

    private fun saveGuide(guide: GuideUi, enabled: Boolean, priority: Int) {
        if (guide.busy) return
        markGuide(guide.id) { it.copy(busy = true) }
        viewModelScope.launch {
            try {
                val saved = repository.saveGuide(guide.dto, enabled = enabled, priority = priority)
                markGuide(guide.id) {
                    it.copy(
                        enabled = enabled,
                        priority = priority,
                        busy = false,
                        // Keep the server's version for the next conditional PUT.
                        dto = guide.dto.copy(
                            enabled = enabled,
                            priority = priority,
                            version = saved.version ?: guide.dto.version,
                        ),
                    )
                }
                _state.update { it.copy(guides = it.guides.sortedBy { g -> g.priority }) }
            } catch (e: Exception) {
                _notices.tryEmit(MemoryNotice.GUIDE_UPDATE_FAILED)
                markGuide(guide.id) { it.copy(busy = false) }
                // A 409 (concurrent edit elsewhere) leaves us stale — resync.
                refreshGuides()
            }
        }
    }

    private fun markGuide(id: String, transform: (GuideUi) -> GuideUi) = _state.update { s ->
        s.copy(guides = s.guides.map { if (it.id == id) transform(it) else it })
    }

    // ---- mapping / formatting ----

    private fun toUi(dto: EntityDto): EntityUi? {
        val id = dto.entityKey ?: return null
        return EntityUi(
            id = id,
            name = dto.displayName,
            type = dto.displayType,
            updatedLabel = dto.updatedAt?.let(::formatDate),
            attrLines = dto.attrLines,
            relationCount = dto.relations.orEmpty().size,
        )
    }

    private fun toUi(dto: GuideDto): GuideUi? {
        val id = dto.guideKey ?: return null
        return GuideUi(
            id = id,
            title = dto.displayTitle,
            text = dto.displayText,
            enabled = dto.enabled,
            priority = dto.priority,
            dto = dto,
        )
    }

    private fun formatDate(iso: String): String? = runCatching {
        val instant = if (iso.all { it.isDigit() }) {
            val n = iso.toLong()
            if (n > 100_000_000_000L) Instant.ofEpochMilli(n) else Instant.ofEpochSecond(n)
        } else {
            OffsetDateTime.parse(iso).toInstant()
        }
        DateTimeFormatter.ofLocalizedDate(FormatStyle.MEDIUM)
            .withLocale(Locale.getDefault())
            .format(instant.atZone(ZoneId.systemDefault()).toLocalDate())
    }.getOrNull()
}
