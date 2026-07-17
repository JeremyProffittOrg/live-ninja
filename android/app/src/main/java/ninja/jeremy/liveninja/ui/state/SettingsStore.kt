package ninja.jeremy.liveninja.ui.state

import android.content.Context
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import org.json.JSONObject

/**
 * Typed projection of the canonical settings document
 * (contracts/settings.schema.json). [raw] carries the FULL JSON document —
 * including fields this app version does not understand — so every write-back
 * preserves unknown fields per contracts/README.md rule 2.
 */
data class SettingsDocument(
    val version: Int,
    val wakeWord: String,
    val wakeEngine: String,
    val sensitivity: Float,
    val personaPresetId: String,
    val personaSystemInstructions: String?,
    val voice: String,
    val turnDetection: String,
    val theme: String,
    val micDeviceId: String?,
    val voiceEngineDefault: String,
    val storeAudio: Boolean,
    val storeTranscripts: Boolean,
    val retentionDays: Int,
    val raw: JSONObject,
) {
    companion object {
        /** Schema enum for `voice`; unrecognized stored values fall back to [DEFAULT_VOICE] in the UI. */
        val VOICES = listOf(
            "alloy", "ash", "ballad", "cedar", "coral",
            "echo", "marin", "sage", "shimmer", "verse",
        )
        const val DEFAULT_VOICE = "cedar"
        val RETENTION_CHOICES = listOf(0, 7, 30, 90)
    }

    /** Voice for display: schema mandates falling back to cedar on an unknown value. */
    val displayVoice: String get() = if (voice in VOICES) voice else DEFAULT_VOICE
}

/**
 * Local persistence + change source for the canonical settings document.
 *
 * The document lives in SharedPreferences as its full JSON string. Every
 * mutation goes through [update], which copies the raw document, applies the
 * mutation, bumps `version` (optimistic-concurrency counter — the eventual
 * `PUT /v1/settings` sync in M6 carries the pre-bump value as `expected`),
 * persists, and emits. Server push/pull sync is an M6 task; this store is the
 * single local source of truth the sync layer will reconcile against.
 */
@Singleton
class SettingsStore @Inject constructor(
    @ApplicationContext context: Context,
) {
    private val prefs = context.getSharedPreferences("liveninja_settings", Context.MODE_PRIVATE)
    private val lock = Any()

    private val _document = MutableStateFlow(parse(loadRaw()))
    val document: StateFlow<SettingsDocument> = _document

    /** Apply [block] to a copy of the raw document, bump `version`, persist, emit. */
    fun update(block: (JSONObject) -> Unit) {
        synchronized(lock) {
            val current = _document.value.raw
            val next = JSONObject(current.toString()) // deep copy, unknown fields intact
            block(next)
            next.put("version", current.optInt("version", 1) + 1)
            prefs.edit().putString(KEY_DOC, next.toString()).apply()
            _document.value = parse(next)
        }
    }

    fun setWakeWord(id: String) = update { it.put("wakeWord", id) }
    fun setWakeEngine(engine: String) = update { it.put("wakeEngine", engine) }
    fun setSensitivity(value: Float) = update { it.put("sensitivity", value.toDouble()) }
    fun setVoice(voice: String) = update { it.put("voice", voice) }
    fun setTurnDetection(value: String) = update { it.put("turnDetection", value) }
    fun setTheme(theme: String) = update { it.put("theme", theme) }
    fun setMicDeviceId(id: String?) = update { it.put("micDeviceId", id ?: JSONObject.NULL) }

    fun setPersona(presetId: String, systemInstructions: String?) = update {
        val persona = it.optJSONObject("persona") ?: JSONObject()
        persona.put("presetId", presetId)
        persona.put(
            "systemInstructions",
            if (presetId == "custom" && systemInstructions != null) systemInstructions else JSONObject.NULL,
        )
        it.put("persona", persona)
    }

    fun setPrivacy(storeAudio: Boolean, storeTranscripts: Boolean, retentionDays: Int) = update {
        val privacy = it.optJSONObject("privacy") ?: JSONObject()
        privacy.put("storeAudio", storeAudio)
        privacy.put("storeTranscripts", storeTranscripts)
        privacy.put("retentionDays", retentionDays)
        it.put("privacy", privacy)
    }

    /** Reset the local document to schema defaults (used on sign-out). */
    fun resetToDefaults() {
        synchronized(lock) {
            val fresh = defaultDocument()
            prefs.edit().putString(KEY_DOC, fresh.toString()).apply()
            _document.value = parse(fresh)
        }
    }

    private fun loadRaw(): JSONObject {
        val stored = prefs.getString(KEY_DOC, null) ?: return defaultDocument()
        return runCatching { JSONObject(stored) }.getOrElse { defaultDocument() }
    }

    private fun parse(raw: JSONObject): SettingsDocument {
        val persona = raw.optJSONObject("persona")
        val privacy = raw.optJSONObject("privacy")
        val voiceEngine = raw.optJSONObject("voiceEngine")
        return SettingsDocument(
            version = raw.optInt("version", 1),
            wakeWord = raw.optString("wakeWord", "hey-live-ninja"),
            wakeEngine = raw.optString("wakeEngine", "openwakeword"),
            sensitivity = raw.optDouble("sensitivity", 0.5).toFloat(),
            personaPresetId = persona?.optString("presetId", "default") ?: "default",
            personaSystemInstructions = persona?.let {
                if (it.isNull("systemInstructions")) null else it.optString("systemInstructions")
            },
            voice = raw.optString("voice", SettingsDocument.DEFAULT_VOICE),
            turnDetection = raw.optString("turnDetection", "semantic_vad"),
            theme = raw.optString("theme", "system"),
            micDeviceId = if (raw.isNull("micDeviceId")) null else raw.optString("micDeviceId"),
            voiceEngineDefault = voiceEngine?.optString("default", "openai-realtime") ?: "openai-realtime",
            storeAudio = privacy?.optBoolean("storeAudio", false) ?: false,
            storeTranscripts = privacy?.optBoolean("storeTranscripts", true) ?: true,
            retentionDays = privacy?.optInt("retentionDays", 30) ?: 30,
            raw = raw,
        )
    }

    private fun defaultDocument(): JSONObject = JSONObject().apply {
        put("version", 1)
        put("wakeWord", "hey-live-ninja")
        put("wakeEngine", "openwakeword")
        put("sensitivity", 0.5)
        put(
            "persona",
            JSONObject().apply {
                put("presetId", "default")
                put("systemInstructions", JSONObject.NULL)
            },
        )
        put("voice", SettingsDocument.DEFAULT_VOICE)
        put("turnDetection", "semantic_vad")
        put("theme", "system")
        put("micDeviceId", JSONObject.NULL)
        put(
            "voiceEngine",
            JSONObject().apply {
                put("default", "openai-realtime")
                put("devices", JSONObject())
            },
        )
        put(
            "privacy",
            JSONObject().apply {
                put("storeAudio", false)
                put("storeTranscripts", true)
                put("retentionDays", 30)
            },
        )
    }

    private companion object {
        const val KEY_DOC = "settings_document_v1"
    }
}
