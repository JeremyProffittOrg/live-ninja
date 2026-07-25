package ninja.jeremy.liveninja.ui.settings

import ninja.jeremy.liveninja.wake.WakeModelRef

/**
 * WS-5 M21.3 pure resolution: what wake phrase Settings may honestly claim is listening.
 *
 * The catalog selection (`SettingsDocument.wakeWord` / `WakePreferences.wakeWordId`) is an
 * aspiration — it is whatever the user picked in the combobox. The phrase that can actually
 * fire wake detection is whichever model `ModelManager.headModel` has loaded, which can lag
 * behind the selection (still downloading, download failed, or the device fell back to the
 * bundled `hey-jarvis` asset). The defect this guards against: showing/claiming a phrase the
 * loaded model cannot match. `SettingsViewModel`'s `activeWakeWordId` state field and
 * SettingsScreen's mismatch warning are both driven from the same [WakeModelRef.wakeWordId]
 * this function reads; it is extracted as a standalone function (rather than left inline in
 * the composable) purely so the property is unit-testable on the JVM without a Compose or
 * Robolectric harness.
 */
data class WakePhraseResolution(
    /** Catalog id of the phrase actually loaded and able to fire detection right now. */
    val activeId: String,
    /** True when [activeId] differs from what the user selected — Settings must warn. */
    val mismatched: Boolean,
)

/**
 * @param selectedId the user's catalog selection (`SettingsDocument.wakeWord`).
 * @param activeId catalog id of the loaded head model, as `SettingsViewModel` mirrors it into
 *   `SettingsUiState.activeWakeWordId` from `ModelManager.headModel`. Empty until the first
 *   model resolves.
 */
fun resolveWakePhrase(selectedId: String, activeId: String): WakePhraseResolution {
    // An empty selection means nothing has been chosen yet (defensive; in practice
    // SettingsStore always seeds a default) — nothing to contradict, so never warn.
    val mismatched = selectedId.isNotEmpty() && activeId.isNotEmpty() && activeId != selectedId
    return WakePhraseResolution(activeId = activeId, mismatched = mismatched)
}

/**
 * Model-ref overload. Settings reaches this logic through the id-taking function above
 * (the composable only ever holds the mirrored id, not the ref), while the wake stack holds
 * the [WakeModelRef] itself — both must resolve identically, so this delegates rather than
 * repeating the comparison.
 *
 * @param active the currently loaded head model (`ModelManager.headModel.value`).
 */
fun resolveWakePhrase(selectedId: String, active: WakeModelRef): WakePhraseResolution =
    resolveWakePhrase(selectedId = selectedId, activeId = active.wakeWordId)
