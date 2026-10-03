package ninja.jeremy.liveninja.ui.permissions

enum class PermissionFeature { MICROPHONE, CAMERA, NOTIFICATIONS, APPROXIMATE_LOCATION }
enum class PermissionAction { NONE, REQUEST, OPEN_SETTINGS }

data class PermissionFacts(
    val granted: Boolean,
    val shouldShowRationale: Boolean = false,
    val runtimeRequestAvailable: Boolean = true,
    val hardwareAvailable: Boolean = true,
)

/** Local UX history only: neither this store nor a previous result is proof of current access. */
interface PermissionHistory {
    var guideSeen: Boolean
    fun wasRequested(feature: PermissionFeature): Boolean
    fun markRequested(feature: PermissionFeature)
}

/** Android-independent policy. Calling these methods never opens a system prompt. */
class PermissionCoordinator(private val history: PermissionHistory) {
    fun claimStartupGuide(): Boolean {
        if (history.guideSeen) return false
        history.guideSeen = true
        return true
    }

    fun action(feature: PermissionFeature, facts: PermissionFacts): PermissionAction = when {
        !facts.hardwareAvailable || facts.granted -> PermissionAction.NONE
        !facts.runtimeRequestAvailable -> PermissionAction.OPEN_SETTINGS
        history.wasRequested(feature) && !facts.shouldShowRationale -> PermissionAction.OPEN_SETTINGS
        else -> PermissionAction.REQUEST
    }

    /** Record only an explicit request, never merely showing a disclosure or skipping it. */
    fun requestStarted(feature: PermissionFeature) = history.markRequested(feature)
}
