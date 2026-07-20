package ninja.jeremy.liveninja.realtime

import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import ninja.jeremy.liveninja.ui.state.TranscriptRole

/**
 * Process-wide accumulation of the current session's transcript (02-voice §B4).
 *
 * The realtime session runs even with no Conversation ViewModel alive (screen
 * off / app backgrounded), so transcript turns must survive independently of
 * the UI. [RealtimeSessionCoordinator] writes here as events arrive;
 * `ConversationViewModel` renders [turns] and derives mic state from
 * `RealtimeSessionController.connected`. Cleared at the start of every new
 * session so a fresh conversation begins empty.
 */
@Singleton
class TranscriptStore @Inject constructor() {

    /** One rendered transcript row: a speech bubble or a tool-call chip. */
    data class Entry(
        val id: String,
        val role: TranscriptRole,
        val text: String,
        val done: Boolean,
        val toolName: String? = null,
        val toolSummary: String? = null,
    ) {
        val isToolCall: Boolean get() = toolName != null
    }

    private val _turns = MutableStateFlow<List<Entry>>(emptyList())
    val turns: StateFlow<List<Entry>> = _turns.asStateFlow()

    /** Discard all accumulated turns (call at session start). */
    fun clear() {
        _turns.value = emptyList()
    }

    /**
     * Append a transcript delta to the (non-tool) row identified by [itemId],
     * creating the row on first sight — mirrors the streaming semantics the
     * coordinator emits on [ninja.jeremy.liveninja.ui.state.SessionUiEvent.TranscriptDelta].
     */
    fun appendDelta(itemId: String, role: TranscriptRole, textDelta: String, done: Boolean) {
        _turns.update { current ->
            val turns = current.toMutableList()
            val index = turns.indexOfFirst { it.id == itemId && !it.isToolCall }
            if (index >= 0) {
                val existing = turns[index]
                turns[index] = existing.copy(text = existing.text + textDelta, done = done)
            } else {
                turns += Entry(id = itemId, role = role, text = textDelta, done = done)
            }
            turns
        }
    }

    /** Append a tool-call chip row (rendered distinctly from speech bubbles). */
    fun addToolChip(itemId: String, name: String, summary: String) {
        _turns.update { current ->
            current + Entry(
                id = itemId,
                role = TranscriptRole.ASSISTANT,
                text = "",
                done = true,
                toolName = name,
                toolSummary = summary,
            )
        }
    }
}
