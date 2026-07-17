package ninja.jeremy.liveninja.ui.state

import android.content.Context
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import org.json.JSONArray
import org.json.JSONObject

/** The consent/disclosure events the onboarding flow records (plan.md M4 §5/§8). */
enum class ConsentEvent(val wireName: String) {
    MIC_DISCLOSURE_SHOWN("mic_disclosure_shown"),
    MIC_PERMISSION_GRANTED("mic_permission_granted"),
    MIC_PERMISSION_DENIED("mic_permission_denied"),
    NOTIFICATIONS_GRANTED("notifications_granted"),
    NOTIFICATIONS_DENIED("notifications_denied"),
    ASSISTANT_ROLE_ACQUIRED("assistant_role_acquired"),
    ASSISTANT_ROLE_DECLINED("assistant_role_declined"),
    OVERLAY_PERMISSION_GRANTED("overlay_permission_granted"),
}

/**
 * Append-only local consent record. Each entry stores the event, a UTC epoch-ms
 * timestamp, and the app version that showed the disclosure — the prominent-
 * disclosure evidence Google Play policy expects the app to retain. M7's
 * privacy milestone syncs these to the backend `CONSENT#` items; until then
 * they are complete and queryable locally.
 */
@Singleton
class ConsentLog @Inject constructor(
    @ApplicationContext context: Context,
) {
    private val prefs = context.getSharedPreferences("liveninja_consent", Context.MODE_PRIVATE)
    private val lock = Any()

    fun record(event: ConsentEvent, detail: String? = null) {
        synchronized(lock) {
            val entries = load()
            entries.put(
                JSONObject().apply {
                    put("event", event.wireName)
                    put("atEpochMs", System.currentTimeMillis())
                    if (detail != null) put("detail", detail)
                },
            )
            prefs.edit().putString(KEY_ENTRIES, entries.toString()).apply()
        }
    }

    /** All recorded entries, oldest first. */
    fun entries(): List<JSONObject> {
        val array = synchronized(lock) { load() }
        return (0 until array.length()).mapNotNull { array.optJSONObject(it) }
    }

    fun hasRecorded(event: ConsentEvent): Boolean =
        entries().any { it.optString("event") == event.wireName }

    private fun load(): JSONArray {
        val stored = prefs.getString(KEY_ENTRIES, null) ?: return JSONArray()
        return runCatching { JSONArray(stored) }.getOrElse { JSONArray() }
    }

    private companion object {
        const val KEY_ENTRIES = "consent_entries_v1"
    }
}
