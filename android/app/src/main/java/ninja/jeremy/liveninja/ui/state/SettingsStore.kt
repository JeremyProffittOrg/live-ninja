package ninja.jeremy.liveninja.ui.state

import android.content.Context
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import org.json.JSONArray
import org.json.JSONObject

/**
 * Verbose-diagnostics configuration (04-logging §A4 / 01-platform §B-iv).
 * Persisted as the nested `diagnostics` object of the settings document.
 * Consumed by the logging workstream (LogSink observes this shape for
 * enabled / severity-floor / per-category gating). Owner defaults: fully on,
 * VERBOSE floor, all eight categories enabled (troubleshooting phase).
 */
data class DiagnosticsConfig(
    val enabled: Boolean = true,
    val minLevel: String = "VERBOSE",
    val categories: Map<String, Boolean> = DEFAULT_CATEGORIES,
) {
    companion object {
        /** The eight log categories (mirrors ninja.jeremy.liveninja.log LogCategory). */
        val CATEGORY_KEYS = listOf(
            "WAKE", "AUDIO", "REALTIME", "AUTH", "TOOLS", "UI", "NET", "GENERAL",
        )

        /** Severity floor choices, most-verbose first (radio group order in Settings). */
        val LEVELS = listOf("VERBOSE", "DEBUG", "INFO", "WARN", "ERROR")

        /** All eight categories enabled — the owner default. */
        val DEFAULT_CATEGORIES: Map<String, Boolean> = CATEGORY_KEYS.associateWith { true }
    }
}

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
    /**
     * Persona IDs switched off in the picker (`persona.hidden`, 2026-08-01).
     * Presentation only — the server's ResolvePersona ignores it, so a hidden
     * persona still mints a working session when a document still names it.
     */
    val hiddenPersonas: Set<String>,
    val personaSystemInstructions: String?,
    val voice: String,
    /**
     * Gemini Live voice for the gemini-flash-live engine (M13, D4).
     * Lenient/additive per the schema: "" = unset → the server resolves
     * persona mapping ?? [DEFAULT_GEMINI_VOICE].
     */
    val geminiVoice: String,
    val turnDetection: String,
    /**
     * Mic pickup: how quickly semantic VAD treats a pause as end-of-turn
     * (contracts/settings.schema.json `micEagerness`; low|medium|high|auto,
     * absent normalizes to auto). The value is consumed SERVER-side —
     * internal/webapp/api_routes.go reads it out of the effective settings
     * document at mint and internal/realtime/mint.go turns it into
     * turn_detection.eagerness — so persisting it here is the whole job on
     * this client; it takes effect on the next session, not the live one.
     */
    val micEagerness: String,
    val theme: String,
    val micDeviceId: String?,
    val voiceEngineDefault: String,
    val storeAudio: Boolean,
    val storeTranscripts: Boolean,
    val retentionDays: Int,
    /**
     * Voice-session lifecycle toggles (01-platform §B-iv). Additive keys; owner
     * defaults baked here so a document written by an older app version (which
     * omits them) reads as the intended defaults.
     */
    val lockedSessions: Boolean = true,
    val wakeScreenOnWake: Boolean = true,
    val keepScreenOn: Boolean = false,
    /** Active visual style (03-theme). hal9000 is the default look. */
    val appStyle: String = "hal9000",
    /** Verbose diagnostics configuration (04-logging §A4). */
    val diagnostics: DiagnosticsConfig = DiagnosticsConfig(),
    val raw: JSONObject,
) {
    companion object {
        /** Schema enum for `voice`; unrecognized stored values fall back to [DEFAULT_VOICE] in the UI. */
        val VOICES = listOf(
            "alloy", "ash", "ballad", "cedar", "coral",
            "echo", "marin", "sage", "shimmer", "verse",
        )
        const val DEFAULT_VOICE = "cedar"

        /** Locked Gemini engine default voice (gemini-plan.md D4). */
        const val DEFAULT_GEMINI_VOICE = "Kore"
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

    /**
     * True only when a valid settings document was actually persisted before
     * this store was opened. A synthesized default document is not migration
     * input for a brand-new install.
     */
    val hasPersistedDocument: Boolean
        get() = prefs.getString(KEY_DOC, null)
            ?.let { stored -> runCatching { JSONObject(stored) }.isSuccess }
            ?: false

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

    /**
     * Set the Gemini Live voice (M13, D4) — a top-level additive key,
     * preserved-the-rest through [update] like every other write.
     */
    fun setGeminiVoice(voice: String) = update { it.put("geminiVoice", voice) }
    fun setTurnDetection(value: String) = update { it.put("turnDetection", value) }

    /**
     * Set mic pickup (low|medium|high|auto). Additive top-level key, written
     * through [update] so unknown fields survive; the server reads it at the
     * next mint (see [SettingsDocument.micEagerness]).
     */
    fun setMicEagerness(value: String) = update { it.put("micEagerness", value) }

    /** Set the default voice engine (M12 FR-VE-04), preserving the per-device pin map. */
    fun setVoiceEngineDefault(engine: String) = update {
        val voiceEngine = it.optJSONObject("voiceEngine") ?: JSONObject().apply { put("devices", JSONObject()) }
        voiceEngine.put("default", engine)
        if (!voiceEngine.has("devices")) voiceEngine.put("devices", JSONObject())
        it.put("voiceEngine", voiceEngine)
    }
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

    // ---- voice-session lifecycle toggles (01-platform §B-iv) ----

    fun setLockedSessions(value: Boolean) = update { it.put("lockedSessions", value) }
    fun setWakeScreenOnWake(value: Boolean) = update { it.put("wakeScreenOnWake", value) }
    fun setKeepScreenOn(value: Boolean) = update { it.put("keepScreenOn", value) }

    /**
     * Set the canonical app-zone visual style. Older Android builds stored a
     * top-level `appStyle`; reads still understand it for migration, while new
     * writes use `appearance.appStyle` like the shared settings schema.
     */
    fun setAppStyle(style: String) = update {
        val appearance = it.optJSONObject("appearance") ?: JSONObject()
        appearance.put("appStyle", style)
        it.put("appearance", appearance)
        it.remove("appStyle")
    }

    // ---- diagnostics (04-logging §A4) ----

    fun setDiagnosticsEnabled(enabled: Boolean) = updateDiagnostics { it.put("enabled", enabled) }

    fun setDiagnosticsMinLevel(level: String) = updateDiagnostics { it.put("minLevel", level) }

    /** Toggle a single log category on/off, preserving the other seven. */
    fun setDiagnosticsCategory(category: String, enabled: Boolean) = updateDiagnostics {
        val categories = it.optJSONObject("categories") ?: JSONObject()
        categories.put(category, enabled)
        it.put("categories", categories)
    }

    /** Replace the whole diagnostics config (used by the all/none Settings actions). */
    fun setDiagnostics(config: DiagnosticsConfig) = update {
        it.put("diagnostics", diagnosticsJson(config.enabled, config.minLevel, config.categories))
    }

    private fun updateDiagnostics(block: (JSONObject) -> Unit) = update {
        val diagnostics = it.optJSONObject("diagnostics")
            ?: diagnosticsJson(true, "VERBOSE", DiagnosticsConfig.DEFAULT_CATEGORIES)
        block(diagnostics)
        it.put("diagnostics", diagnostics)
    }

    /** Reset the local document to schema defaults (used on sign-out). */
    fun resetToDefaults() {
        synchronized(lock) {
            val fresh = defaultDocument()
            prefs.edit().putString(KEY_DOC, fresh.toString()).apply()
            _document.value = parse(fresh)
        }
    }

    /** Deep snapshot used by the one-time local-to-current-device migration. */
    fun rawSnapshot(): JSONObject = synchronized(lock) {
        JSONObject(_document.value.raw.toString())
    }

    /**
     * Adopt the backend's effective settings for this device without treating
     * the pull as a local edit. Android-only controls remain local because the
     * shared backend sections intentionally do not classify them as portable.
     */
    fun replaceFromServer(effective: JSONObject) {
        synchronized(lock) {
            val current = _document.value.raw
            val next = JSONObject(effective.toString())
            LOCAL_ONLY_KEYS.forEach { key ->
                if (current.has(key)) next.put(key, copyJsonValue(current.opt(key)))
            }
            prefs.edit().putString(KEY_DOC, next.toString()).apply()
            _document.value = parse(next)
        }
    }

    /** Build a typed remote-host preview without changing the runtime flow. */
    fun preview(sectionSettings: JSONObject): SettingsDocument {
        val next = JSONObject(_document.value.raw.toString())
        sectionSettings.keys().forEach { key ->
            next.put(key, copyJsonValue(sectionSettings.opt(key)))
        }
        return parse(next)
    }

    /** Confirm locally-edited section values after a queued server save. */
    fun applySyncedSection(sectionSettings: JSONObject) {
        synchronized(lock) {
            val next = JSONObject(_document.value.raw.toString())
            sectionSettings.keys().forEach { key ->
                next.put(key, copyJsonValue(sectionSettings.opt(key)))
            }
            prefs.edit().putString(KEY_DOC, next.toString()).apply()
            _document.value = parse(next)
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
        val appearance = raw.optJSONObject("appearance")
        return SettingsDocument(
            version = raw.optInt("version", 1),
            wakeWord = raw.optString("wakeWord", "hey-live-ninja"),
            wakeEngine = raw.optString("wakeEngine", "openwakeword"),
            sensitivity = raw.optDouble("sensitivity", 0.5).toFloat(),
            personaPresetId = persona?.optString("presetId", "default") ?: "default",
            hiddenPersonas = persona?.optJSONArray("hidden")?.let { arr ->
                (0 until arr.length()).mapNotNull { arr.optString(it).takeIf(String::isNotEmpty) }.toSet()
            } ?: emptySet(),
            personaSystemInstructions = persona?.let {
                if (it.isNull("systemInstructions")) null else it.optString("systemInstructions")
            },
            voice = raw.optString("voice", SettingsDocument.DEFAULT_VOICE),
            geminiVoice = raw.optString("geminiVoice", ""),
            turnDetection = raw.optString("turnDetection", "semantic_vad"),
            micEagerness = raw.optString("micEagerness", "auto").ifEmpty { "auto" },
            theme = raw.optString("theme", "system"),
            micDeviceId = if (raw.isNull("micDeviceId")) null else raw.optString("micDeviceId"),
            voiceEngineDefault = voiceEngine?.optString("default", "openai-realtime") ?: "openai-realtime",
            storeAudio = privacy?.optBoolean("storeAudio", false) ?: false,
            storeTranscripts = privacy?.optBoolean("storeTranscripts", true) ?: true,
            retentionDays = privacy?.optInt("retentionDays", 30) ?: 30,
            lockedSessions = raw.optBoolean("lockedSessions", true),
            wakeScreenOnWake = raw.optBoolean("wakeScreenOnWake", true),
            keepScreenOn = raw.optBoolean("keepScreenOn", false),
            appStyle = appearance?.optString(
                "appStyle",
                raw.optString("appStyle", "hal9000"),
            ) ?: raw.optString("appStyle", "hal9000"),
            diagnostics = parseDiagnostics(raw.optJSONObject("diagnostics")),
            raw = raw,
        )
    }

    /**
     * Project the nested `diagnostics` object, tolerating a missing object,
     * missing keys, and missing per-category entries (each defaults on). Unknown
     * category keys in the stored JSON are ignored by this typed projection but
     * still round-trip through [raw].
     */
    private fun parseDiagnostics(obj: JSONObject?): DiagnosticsConfig {
        if (obj == null) return DiagnosticsConfig()
        val storedCategories = obj.optJSONObject("categories")
        val categories = DiagnosticsConfig.CATEGORY_KEYS.associateWith { key ->
            storedCategories?.optBoolean(key, true) ?: true
        }
        return DiagnosticsConfig(
            enabled = obj.optBoolean("enabled", true),
            minLevel = obj.optString("minLevel", "VERBOSE"),
            categories = categories,
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
        put("micEagerness", "auto")
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
        put("lockedSessions", true)
        put("wakeScreenOnWake", true)
        put("keepScreenOn", false)
        put("appStyle", "hal9000")
        put("diagnostics", diagnosticsJson(true, "VERBOSE", DiagnosticsConfig.DEFAULT_CATEGORIES))
    }

    private fun diagnosticsJson(
        enabled: Boolean,
        minLevel: String,
        categories: Map<String, Boolean>,
    ): JSONObject = JSONObject().apply {
        put("enabled", enabled)
        put("minLevel", minLevel)
        put(
            "categories",
            JSONObject().apply { categories.forEach { (key, value) -> put(key, value) } },
        )
    }

    private companion object {
        const val KEY_DOC = "settings_document_v1"
        val LOCAL_ONLY_KEYS = listOf(
            "lockedSessions",
            "wakeScreenOnWake",
            "keepScreenOn",
            "diagnostics",
            // Read compatibility only; setAppStyle migrates this into appearance.
            "appStyle",
        )

        fun copyJsonValue(value: Any?): Any? = when (value) {
            is JSONObject -> JSONObject(value.toString())
            is JSONArray -> JSONArray(value.toString())
            else -> value
        }
    }
}
