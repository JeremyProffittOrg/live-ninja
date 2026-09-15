package ninja.jeremy.liveninja.ui.conversation

/**
 * What the idle conversation screen may honestly say about the wake phrase.
 *
 * Three facts feed it, and they are allowed to disagree:
 *  - the phrase the user SELECTED (Settings / onboarding pick),
 *  - the phrase the loaded head model can actually MATCH (`ModelManager.headModel`),
 *  - whether the wake service is RUNNING at all (`WakeWordService.runningFlow`).
 *
 * The 2026-09-15 defect this exists for: a fresh install advertised “Or just say Hey Jarvis”
 * while nothing was listening (always-listening had never been switched on) and while the
 * user had picked Hey Live Ninja. A caption that names a phrase must be true in both senses —
 * it is the selected phrase, and something is listening for it. Pure so it is unit-testable.
 */
enum class WakeCaptionKind {
    /** Service running and the loaded model is the selected phrase: "Or just say “X”". */
    LISTENING,

    /** Nothing is listening: say so and offer to turn it on. Never advertise a phrase here. */
    OFF,

    /** Service running, but the loaded model is not the selected phrase yet. */
    MODEL_PENDING,
}

data class WakeCaption(
    val kind: WakeCaptionKind,
    /** Human label of the user's selection. */
    val selectedLabel: String,
    /** Human label of the phrase the detector can currently match. */
    val activeLabel: String,
)

fun wakeCaption(
    selectedId: String,
    activeId: String,
    serviceRunning: Boolean,
    labelFor: (String) -> String,
): WakeCaption {
    val effectiveSelected = selectedId.ifEmpty { activeId }
    val effectiveActive = activeId.ifEmpty { selectedId }
    val selectedLabel = labelFor(effectiveSelected)
    val activeLabel = labelFor(effectiveActive)
    val kind = when {
        !serviceRunning -> WakeCaptionKind.OFF
        effectiveSelected.isNotEmpty() && effectiveActive.isNotEmpty() &&
            effectiveSelected != effectiveActive -> WakeCaptionKind.MODEL_PENDING
        else -> WakeCaptionKind.LISTENING
    }
    return WakeCaption(kind = kind, selectedLabel = selectedLabel, activeLabel = activeLabel)
}

/** Trained phrases carry a user-scoped 6-hex suffix (`hey-live-ninja-47df2e`). */
private val TRAINED_SUFFIX = Regex("-[0-9a-f]{6}$")

/** Catalog labels are quoted (`“Hey Live Ninja”`); the caption adds its own quotes. */
private val QUOTES = Regex("^[“\"]+|[”\"]+$")

/**
 * Human label for a wake-word catalog id: the catalog's own label when it knows the id,
 * otherwise the id title-cased with any trained-model suffix removed — never
 * "Hey Live Ninja 47df2e".
 */
fun wakePhraseLabel(id: String, catalogLabel: String?): String {
    catalogLabel?.replace(QUOTES, "")?.trim()?.takeIf { it.isNotEmpty() }?.let { return it }
    return id.replace(TRAINED_SUFFIX, "")
        .split('-')
        .filter { it.isNotEmpty() }
        .joinToString(" ") { part -> part.replaceFirstChar { c -> c.uppercaseChar() } }
}
