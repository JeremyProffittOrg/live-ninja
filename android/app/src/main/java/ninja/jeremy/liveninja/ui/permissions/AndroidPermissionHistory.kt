package ninja.jeremy.liveninja.ui.permissions

import android.content.Context

class AndroidPermissionHistory(context: Context) : PermissionHistory {
    private val preferences = context.applicationContext.getSharedPreferences(
        "permission_guide", Context.MODE_PRIVATE,
    )
    override var guideSeen: Boolean
        get() = preferences.getBoolean("guide_seen_v1", false)
        set(value) { preferences.edit().putBoolean("guide_seen_v1", value).apply() }

    override fun wasRequested(feature: PermissionFeature): Boolean =
        preferences.getBoolean("requested_${feature.name}", false)

    override fun markRequested(feature: PermissionFeature) {
        preferences.edit().putBoolean("requested_${feature.name}", true).apply()
    }
}
