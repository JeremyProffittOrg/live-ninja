package ninja.jeremy.liveninja.ui.settings

import ninja.jeremy.liveninja.net.SettingsSectionEnvelope
import org.json.JSONArray
import org.json.JSONObject

internal enum class InitialSettingsMigrationMode {
    NONE,
    LEGACY_DOCUMENT,
    FRESH_INSTALL,
}

internal fun initialSettingsMigrationMode(
    migrationComplete: Boolean,
    hasPersistedDocument: Boolean,
): InitialSettingsMigrationMode = when {
    migrationComplete -> InitialSettingsMigrationMode.NONE
    hasPersistedDocument -> InitialSettingsMigrationMode.LEGACY_DOCUMENT
    else -> InitialSettingsMigrationMode.FRESH_INSTALL
}

/** Canonical, portable fields owned by each backend settings section. */
internal val SETTINGS_SECTION_FIELDS: Map<SettingsSection, List<String>> = mapOf(
    SettingsSection.ABOUT_YOU to listOf("profile"),
    SettingsSection.WAKE_WORD to listOf("wakeWord", "wakeEngine", "sensitivity"),
    SettingsSection.PERSONA to listOf("persona", "voice", "voiceAccent", "personaPrefs"),
    SettingsSection.VOICE_ENGINE to listOf("voiceEngine", "geminiVoice"),
    SettingsSection.TURN_DETECTION to
        listOf("turnDetection", "micEagerness", "keepListeningSeconds"),
    SettingsSection.APPEARANCE to listOf("theme", "appearance"),
    SettingsSection.MICROPHONE to listOf("micDeviceId"),
    SettingsSection.PRIVACY to listOf("privacy"),
)

/**
 * Snapshot the portable values for one section from a full local document.
 * Legacy Android `appStyle` is translated into canonical
 * `appearance.appStyle` during the one-time upload.
 */
internal fun extractSectionSettings(
    section: SettingsSection,
    fullDocument: JSONObject,
): JSONObject {
    val out = JSONObject()
    SETTINGS_SECTION_FIELDS[section].orEmpty().forEach { key ->
        if (fullDocument.has(key)) {
            out.put(key, deepCopyJsonValue(fullDocument.opt(key)))
        }
    }
    if (section == SettingsSection.APPEARANCE &&
        !out.has("appearance") &&
        fullDocument.has("appStyle")
    ) {
        out.put(
            "appearance",
            JSONObject().put("appStyle", fullDocument.optString("appStyle", "hal9000")),
        )
    }
    return out
}

/** Overlay a section response on a copy without mutating current runtime state. */
internal fun previewSectionSettings(
    currentDocument: JSONObject,
    sectionSettings: JSONObject,
): JSONObject {
    val preview = JSONObject(currentDocument.toString())
    sectionSettings.keys().forEach { key ->
        preview.put(key, deepCopyJsonValue(sectionSettings.opt(key)))
    }
    return preview
}

/** Current-host apply reads live UI state, never a lagging envelope snapshot. */
internal fun settingsForApply(
    section: SettingsSection,
    currentDocument: JSONObject,
    viewedSettings: JSONObject?,
    viewedIsCurrent: Boolean,
): JSONObject =
    if (viewedSettings == null || viewedIsCurrent) {
        extractSectionSettings(section, currentDocument)
    } else {
        JSONObject(viewedSettings.toString())
    }

/**
 * Keep a prior host selection only while that host supports the section.
 * Otherwise prefer the current host, then the first supported host.
 */
internal fun supportedSettingsHostId(
    section: SettingsSection,
    hosts: List<SettingsHostUi>,
    previousDeviceId: String?,
): String? =
    hosts.firstOrNull { it.id == previousDeviceId && it.supports(section) }?.id
        ?: hosts.firstOrNull { it.isCurrent && it.supports(section) }?.id
        ?: hosts.firstOrNull { it.supports(section) }?.id

/**
 * A successful write response is authoritative for the saved host's section. This
 * preserves sibling fields that another client changed while the local edit
 * was being rebased; older servers that omit device rows fall back to the
 * optimistic local section.
 */
internal fun deviceSectionSettingsAfterSave(
    deviceId: String,
    refreshed: SettingsSectionEnvelope,
    optimisticSettings: JSONObject,
): JSONObject {
    val responseSettings = refreshed.devices
        .firstOrNull { it.deviceId == deviceId }
        ?.settings
        ?: if (refreshed.currentDeviceId == deviceId) {
            refreshed.devices.firstOrNull { it.isCurrent }?.settings
        } else {
            null
        }
    return responseSettings?.let { JSONObject(it.toString()) }
        ?: JSONObject(optimisticSettings.toString())
}

/** Delayed loads must never replace a section state produced by a newer write. */
internal fun shouldAcceptSectionEnvelope(
    incomingVersion: Int,
    currentVersion: Int?,
): Boolean = currentVersion == null || incomingVersion >= currentVersion

/** Reapply coalesced user mutations to a fresh 409 baseline. */
internal fun applySectionMutations(
    section: SettingsSection,
    baseline: JSONObject,
    mutations: List<(JSONObject) -> Unit>,
): JSONObject {
    val fresh = JSONObject(baseline.toString())
    mutations.forEach { mutation -> mutation(fresh) }
    return extractSectionSettings(section, fresh)
}

/**
 * A microphone route id belongs to one host's AudioManager. Only the semantic
 * system default (`null`) may be copied to other hosts.
 */
internal fun microphoneSettingsCanTargetOthers(settings: JSONObject): Boolean =
    !settings.has("micDeviceId") || settings.isNull("micDeviceId")

/**
 * A host-local route may only be re-saved to the host it came from. System
 * default is semantic and can safely be copied to current, selected, or all.
 */
internal fun microphoneSettingsCanApply(
    settings: JSONObject,
    sourceIsCurrent: Boolean,
    targetMode: SettingsTargetMode,
): Boolean =
    microphoneSettingsCanTargetOthers(settings) ||
        (sourceIsCurrent && targetMode == SettingsTargetMode.CURRENT)

/** Concise, human-readable values for the host picker; never expose raw JSON. */
internal fun settingsSectionSummary(
    section: SettingsSection,
    settings: JSONObject,
): String = when (section) {
    SettingsSection.ABOUT_YOU -> {
        val profile = settings.optJSONObject("profile")
        val name = profile?.optString("displayName")?.takeIf(String::isNotBlank) ?: "Name not set"
        val units = profile?.optString("units")?.takeIf(String::isNotBlank) ?: "imperial"
        "$name · $units units"
    }
    SettingsSection.WAKE_WORD -> {
        val wakeWord = settings.optString("wakeWord", "Default wake word")
        val engine = settings.optString("wakeEngine", "default engine")
        val sensitivity = (settings.optDouble("sensitivity", 0.5) * 100).toInt()
        "$wakeWord · $engine · $sensitivity% sensitivity"
    }
    SettingsSection.PERSONA -> {
        val persona = settings.optJSONObject("persona")
            ?.optString("presetId")
            ?.takeIf(String::isNotBlank)
            ?: "default"
        val voice = settings.optString("voice", "default")
        "Persona: $persona · Voice: $voice"
    }
    SettingsSection.VOICE_ENGINE -> {
        val engine = settings.optJSONObject("voiceEngine")
            ?.optString("default")
            ?.takeIf(String::isNotBlank)
            ?: "default"
        val geminiVoice = settings.optString("geminiVoice")
            .takeIf(String::isNotBlank)
        if (geminiVoice == null) "Engine: $engine" else "Engine: $engine · Voice: $geminiVoice"
    }
    SettingsSection.TURN_DETECTION -> {
        val mode = settings.optString("turnDetection", "default")
        val keepListening = settings.optInt("keepListeningSeconds", -1)
        if (keepListening >= 0) {
            "$mode · Keep listening: ${keepListening}s"
        } else {
            "Detection: $mode"
        }
    }
    SettingsSection.APPEARANCE -> {
        val style = settings.optJSONObject("appearance")
            ?.optString("appStyle")
            ?.takeIf(String::isNotBlank)
            ?: "default"
        val theme = settings.optString("theme", "system")
        "Style: $style · Theme: $theme"
    }
    SettingsSection.MICROPHONE -> {
        if (!settings.has("micDeviceId") || settings.isNull("micDeviceId")) {
            "Microphone: System default"
        } else {
            "Microphone: Specific device on this host"
        }
    }
    SettingsSection.PRIVACY -> {
        val privacy = settings.optJSONObject("privacy")
        val transcripts = if (privacy?.optBoolean("storeTranscripts", true) != false) "on" else "off"
        val audio = if (privacy?.optBoolean("storeAudio", false) == true) "on" else "off"
        val retention = privacy?.optInt("retentionDays", 30) ?: 30
        "Transcripts: $transcripts · Audio: $audio · Retention: $retention days"
    }
    SettingsSection.ACCOUNT -> ""
}

private fun deepCopyJsonValue(value: Any?): Any? = when (value) {
    is JSONObject -> JSONObject(value.toString())
    is JSONArray -> JSONArray(value.toString())
    else -> value
}
