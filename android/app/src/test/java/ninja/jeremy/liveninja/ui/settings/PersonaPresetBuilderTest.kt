package ninja.jeremy.liveninja.ui.settings

import io.mockk.every
import io.mockk.mockk
import kotlinx.coroutines.flow.MutableStateFlow
import ninja.jeremy.liveninja.net.PersonaInfoDto
import ninja.jeremy.liveninja.ui.state.SettingsDocument
import ninja.jeremy.liveninja.ui.state.SettingsStore
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * SettingsViewModel.buildPersonaPresets — the catalog-to-picker rules.
 *
 * This replaced a hardcoded list that offered `focused` / `friendly` / `coach`
 * / `analyst`, none of which exist in the server registry: ResolvePersona falls
 * back to `default` for an unknown id, so choosing "Coach Ninja" on Android
 * silently gave you the standard persona and nothing said so. These tests pin
 * the three rules that make the API-driven version safe.
 */
class PersonaPresetBuilderTest {

    private fun vmWith(selected: String, hidden: Set<String>): SettingsViewModel {
        val doc = SettingsDocument(
            version = 1,
            wakeWord = "hey-live-ninja",
            wakeEngine = "openwakeword",
            sensitivity = 0.5f,
            personaPresetId = selected,
            hiddenPersonas = hidden,
            personaSystemInstructions = null,
            voice = "cedar",
            geminiVoice = "",
            turnDetection = "semantic_vad",
            micEagerness = "auto",
            theme = "system",
            micDeviceId = null,
            voiceEngineDefault = "openai-realtime",
            storeAudio = false,
            storeTranscripts = true,
            retentionDays = 30,
            raw = JSONObject(),
        )
        val store = mockk<SettingsStore>()
        every { store.document } returns MutableStateFlow(doc)

        // Only settingsStore is exercised by buildPersonaPresets; the rest of
        // the graph stays unconstructed so this remains a pure JVM test.
        val vm = mockk<SettingsViewModel>(relaxed = true)
        every { vm.buildPersonaPresets(any()) } answers { callOriginal() }
        val field = SettingsViewModel::class.java.getDeclaredField("settingsStore")
        field.isAccessible = true
        field.set(vm, store)
        return vm
    }

    private val catalog = listOf(
        PersonaInfoDto("default", "Live Ninja", "The standard one", "General"),
        PersonaInfoDto("product-owner", "Product Owner", "Owns the eval", "PDLC"),
        PersonaInfoDto("staff-sre", "Staff SRE", "Flat on-call cadence", "PDLC"),
        PersonaInfoDto("esp32-p4-engineer", "ESP32-P4 Engineer", "No radio", "ESP32"),
        PersonaInfoDto("bard", "The Bard", "Thee and thou", "Fun"),
    )

    @Test
    fun `catalog entries carry their group and custom is appended last`() {
        val out = vmWith(selected = "default", hidden = emptySet()).buildPersonaPresets(catalog)

        assertEquals("PDLC", out.first { it.id == "product-owner" }.group)
        assertEquals("ESP32", out.first { it.id == "esp32-p4-engineer" }.group)
        assertEquals(SettingsViewModel.CUSTOM_PERSONA_ID, out.last().id)
        // "custom" is a client-side concept; it must not claim a server group.
        assertEquals("", out.last().group)
    }

    @Test
    fun `hidden personas are dropped`() {
        val out = vmWith(selected = "default", hidden = setOf("bard", "staff-sre"))
            .buildPersonaPresets(catalog)

        assertFalse(out.any { it.id == "bard" })
        assertFalse(out.any { it.id == "staff-sre" })
        assertTrue(out.any { it.id == "product-owner" })
    }

    @Test
    fun `the selected persona survives even when hidden`() {
        // Otherwise the picker displays a DIFFERENT persona than the document
        // holds, and the next save writes that wrong value back.
        val out = vmWith(selected = "bard", hidden = setOf("bard")).buildPersonaPresets(catalog)
        assertTrue(out.any { it.id == "bard" })
    }

    @Test
    fun `default is never dropped`() {
        val out = vmWith(selected = "bard", hidden = setOf("default")).buildPersonaPresets(catalog)
        assertTrue(out.any { it.id == "default" })
    }

    @Test
    fun `a stored persona missing from the catalog is kept, not silently swapped`() {
        val out = vmWith(selected = "some-retired-persona", hidden = emptySet())
            .buildPersonaPresets(catalog)

        val kept = out.firstOrNull { it.id == "some-retired-persona" }
        assertTrue("a persona the catalog no longer lists must survive", kept != null)
        assertTrue(kept!!.description.contains("Kept as-is"))
    }

    @Test
    fun `an empty catalog falls back rather than offering only custom`() {
        val out = vmWith(selected = "default", hidden = emptySet()).buildPersonaPresets(emptyList())
        assertEquals(SettingsViewModel.PERSONA_PRESETS, out)
        assertTrue(out.any { it.id == "default" })
    }
}
