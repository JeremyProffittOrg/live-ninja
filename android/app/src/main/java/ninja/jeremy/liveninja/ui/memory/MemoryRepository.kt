package ninja.jeremy.liveninja.ui.memory

import javax.inject.Inject
import javax.inject.Singleton
import ninja.jeremy.liveninja.net.EntityListResponse
import ninja.jeremy.liveninja.net.GuideDto
import ninja.jeremy.liveninja.net.GuideListResponse
import ninja.jeremy.liveninja.net.GuidePutRequest
import ninja.jeremy.liveninja.net.LiveNinjaApi

/**
 * Android access to the M10 Memory Layer + Guide Entities surface
 * (contracts/api.md): entity browsing with type filter, forget (dual-store
 * delete, FR-MEM-05), and guide enable/priority edits (FR-MEM-09). Pure
 * pass-through over the shared Retrofit [LiveNinjaApi] — auth, refresh and
 * JSON leniency all ride the existing stack.
 */
@Singleton
class MemoryRepository @Inject constructor(
    private val api: LiveNinjaApi,
) {
    /** [type] null = all entity types. */
    suspend fun listEntities(type: String? = null, cursor: String? = null): EntityListResponse =
        api.listEntities(type = type, cursor = cursor, limit = PAGE_SIZE)

    suspend fun forget(entityId: String) {
        api.forgetMemory(entityId)
    }

    suspend fun listGuides(): GuideListResponse = api.listGuides()

    /**
     * Persist a guide edit (toggle / priority). PUT replaces, so the caller's
     * current view of the guide is resent in full; `text` and `body` both
     * carry the guide text (see [GuidePutRequest]).
     */
    suspend fun saveGuide(guide: GuideDto, enabled: Boolean, priority: Int): GuideDto {
        val id = requireNotNull(guide.guideKey) { "guide without id" }
        return api.putGuide(
            id = id,
            body = GuidePutRequest(
                title = guide.displayTitle,
                text = guide.displayText,
                body = guide.displayText,
                enabled = enabled,
                priority = priority,
                version = guide.version,
            ),
        )
    }

    companion object {
        const val PAGE_SIZE = 50
    }
}
