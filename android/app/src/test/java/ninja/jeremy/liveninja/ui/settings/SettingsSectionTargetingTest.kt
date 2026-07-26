package ninja.jeremy.liveninja.ui.settings

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import ninja.jeremy.liveninja.net.DeviceSectionSettingsDto
import ninja.jeremy.liveninja.net.SettingsSectionEnvelope
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class SettingsSectionTargetingTest {

    @Test
    fun migrationDistinguishesFreshInstallFromRealLegacyDocument() {
        assertEquals(
            InitialSettingsMigrationMode.FRESH_INSTALL,
            initialSettingsMigrationMode(
                migrationComplete = false,
                hasPersistedDocument = false,
            ),
        )
        assertEquals(
            InitialSettingsMigrationMode.LEGACY_DOCUMENT,
            initialSettingsMigrationMode(
                migrationComplete = false,
                hasPersistedDocument = true,
            ),
        )
        assertEquals(
            InitialSettingsMigrationMode.NONE,
            initialSettingsMigrationMode(
                migrationComplete = true,
                hasPersistedDocument = true,
            ),
        )
    }

    @Test
    fun extractionKeepsOnlyCanonicalSectionFields() {
        val full = JSONObject()
            .put("wakeWord", "computer")
            .put("wakeEngine", "openwakeword")
            .put("sensitivity", 0.72)
            .put("lockedSessions", false)
            .put("futureField", "keep elsewhere")

        val section = extractSectionSettings(SettingsSection.WAKE_WORD, full)

        assertEquals("computer", section.getString("wakeWord"))
        assertEquals("openwakeword", section.getString("wakeEngine"))
        assertEquals(0.72, section.getDouble("sensitivity"), 0.001)
        assertFalse(section.has("lockedSessions"))
        assertFalse(section.has("futureField"))
    }

    @Test
    fun legacyAppStyleMigratesIntoCanonicalAppearance() {
        val section = extractSectionSettings(
            SettingsSection.APPEARANCE,
            JSONObject().put("theme", "dark").put("appStyle", "terminal"),
        )

        assertEquals("dark", section.getString("theme"))
        assertEquals("terminal", section.getJSONObject("appearance").getString("appStyle"))
        assertFalse(section.has("appStyle"))
    }

    @Test
    fun remotePreviewDoesNotMutateCurrentDocument() {
        val current = JSONObject()
            .put("theme", "system")
            .put("privacy", JSONObject().put("storeAudio", false))
        val remote = JSONObject().put("theme", "dark")

        val preview = previewSectionSettings(current, remote)

        assertEquals("dark", preview.getString("theme"))
        assertEquals("system", current.getString("theme"))
        assertFalse(current.getJSONObject("privacy").getBoolean("storeAudio"))
    }

    @Test
    fun applyFromCurrentUsesLiveUiInsteadOfLaggingHostSnapshot() {
        val live = JSONObject().put("theme", "dark")
        val staleHost = JSONObject().put("theme", "light")

        val settings = settingsForApply(
            section = SettingsSection.APPEARANCE,
            currentDocument = live,
            viewedSettings = staleHost,
            viewedIsCurrent = true,
        )

        assertEquals("dark", settings.getString("theme"))
    }

    @Test
    fun conflictRebasePreservesFreshUnknownFieldsAndAppliesUserMutation() {
        val freshServer = JSONObject().put(
            "privacy",
            JSONObject()
                .put("storeAudio", false)
                .put("storeTranscripts", true)
                .put("futureServerField", "keep-me"),
        )

        val rebased = applySectionMutations(
            SettingsSection.PRIVACY,
            freshServer,
            listOf { document ->
                document.getJSONObject("privacy").put("storeAudio", true)
            },
        )

        val privacy = rebased.getJSONObject("privacy")
        assertTrue(privacy.getBoolean("storeAudio"))
        assertEquals("keep-me", privacy.getString("futureServerField"))
    }

    @Test
    fun successfulCurrentSaveAdoptsConcurrentSiblingFieldFromResponse() {
        val freshServer = JSONObject().put(
            "privacy",
            JSONObject()
                .put("storeAudio", false)
                .put("storeTranscripts", false)
                .put("retentionDays", 90),
        )
        val rebased = applySectionMutations(
            SettingsSection.PRIVACY,
            freshServer,
            listOf { document ->
                document.getJSONObject("privacy").put("storeAudio", true)
            },
        )
        val refreshed = SettingsSectionEnvelope(
            section = "privacy",
            version = 8,
            currentDeviceId = "current",
            devices = listOf(
                DeviceSectionSettingsDto(
                    deviceId = "current",
                    name = "Current",
                    isCurrent = true,
                    settings = Json.parseToJsonElement(rebased.toString()).jsonObject,
                ),
            ),
        )
        val confirmed = deviceSectionSettingsAfterSave(
            deviceId = "current",
            refreshed = refreshed,
            optimisticSettings = JSONObject().put(
                "privacy",
                JSONObject().put("storeAudio", true).put("retentionDays", 30),
            ),
        )

        val privacy = confirmed.getJSONObject("privacy")
        assertTrue(privacy.getBoolean("storeAudio"))
        assertFalse(privacy.getBoolean("storeTranscripts"))
        assertEquals(90, privacy.getInt("retentionDays"))
    }

    @Test
    fun successfulRemoteSaveAdoptsThatHostsAuthoritativeSiblingFields() {
        val refreshed = SettingsSectionEnvelope(
            section = "appearance",
            version = 9,
            currentDeviceId = "current",
            devices = listOf(
                DeviceSectionSettingsDto(
                    deviceId = "current",
                    name = "Current",
                    isCurrent = true,
                    settings = Json.parseToJsonElement(
                        """{"theme":"light","appearance":{"appStyle":"minimal"}}""",
                    ).jsonObject,
                ),
                DeviceSectionSettingsDto(
                    deviceId = "remote",
                    name = "Remote",
                    settings = Json.parseToJsonElement(
                        """{"theme":"dark","appearance":{"appStyle":"terminal"}}""",
                    ).jsonObject,
                ),
            ),
        )

        val confirmed = deviceSectionSettingsAfterSave(
            deviceId = "remote",
            refreshed = refreshed,
            optimisticSettings = JSONObject()
                .put("theme", "dark")
                .put("appearance", JSONObject().put("appStyle", "minimal")),
        )

        assertEquals("dark", confirmed.getString("theme"))
        assertEquals(
            "terminal",
            confirmed.getJSONObject("appearance").getString("appStyle"),
        )
    }

    @Test
    fun staleSectionEnvelopeCannotRollBackNewerUiState() {
        assertFalse(shouldAcceptSectionEnvelope(incomingVersion = 7, currentVersion = 8))
        assertTrue(shouldAcceptSectionEnvelope(incomingVersion = 8, currentVersion = 8))
        assertTrue(shouldAcceptSectionEnvelope(incomingVersion = 1, currentVersion = null))
    }

    @Test
    fun onlySystemDefaultMicrophoneCanTargetAnotherHost() {
        assertTrue(microphoneSettingsCanTargetOthers(JSONObject()))
        assertTrue(
            microphoneSettingsCanTargetOthers(
                JSONObject().put("micDeviceId", JSONObject.NULL),
            ),
        )
        assertFalse(
            microphoneSettingsCanTargetOthers(
                JSONObject().put("micDeviceId", "android-audio-device-7"),
            ),
        )
        val localRoute = JSONObject().put("micDeviceId", "android-audio-device-7")
        assertTrue(
            microphoneSettingsCanApply(
                localRoute,
                sourceIsCurrent = true,
                targetMode = SettingsTargetMode.CURRENT,
            ),
        )
        assertFalse(
            microphoneSettingsCanApply(
                localRoute,
                sourceIsCurrent = false,
                targetMode = SettingsTargetMode.CURRENT,
            ),
        )
        assertFalse(
            microphoneSettingsCanApply(
                localRoute,
                sourceIsCurrent = true,
                targetMode = SettingsTargetMode.ALL,
            ),
        )
    }

    @Test
    fun summariesExposeUsefulValuesWithoutRawJsonOrMicrophoneIds() {
        val about = settingsSectionSummary(
            SettingsSection.ABOUT_YOU,
            JSONObject().put(
                "profile",
                JSONObject().put("displayName", "Jeremy").put("units", "metric"),
            ),
        )
        val microphone = settingsSectionSummary(
            SettingsSection.MICROPHONE,
            JSONObject().put("micDeviceId", "android-audio-device-7"),
        )

        assertEquals("Jeremy · metric units", about)
        assertEquals("Microphone: Specific device on this host", microphone)
        assertNull(Regex("android-audio-device-7").find(microphone))
    }

    @Test
    fun declaredCapabilitiesExcludeUnsupportedSectionTargets() {
        val legacy = SettingsHostUi(id = "legacy", name = "Legacy")
        val limited = SettingsHostUi(
            id = "speaker",
            name = "Speaker",
            capabilities = setOf("privacy", "voiceEngine"),
        )

        assertTrue(legacy.supports(SettingsSection.MICROPHONE))
        assertTrue(limited.supports(SettingsSection.PRIVACY))
        assertFalse(limited.supports(SettingsSection.MICROPHONE))
    }

    @Test
    fun hostSelectionSkipsUnsupportedPreviousAndCurrentHosts() {
        val hosts = listOf(
            SettingsHostUi(
                id = "current",
                name = "Current",
                isCurrent = true,
                capabilities = setOf("privacy"),
            ),
            SettingsHostUi(
                id = "previous",
                name = "Previous",
                capabilities = setOf("privacy"),
            ),
            SettingsHostUi(
                id = "supported",
                name = "Supported",
                capabilities = setOf("appearance"),
            ),
        )

        assertEquals(
            "supported",
            supportedSettingsHostId(
                section = SettingsSection.APPEARANCE,
                hosts = hosts,
                previousDeviceId = "previous",
            ),
        )
        assertEquals(3, hosts.size)
    }
}
