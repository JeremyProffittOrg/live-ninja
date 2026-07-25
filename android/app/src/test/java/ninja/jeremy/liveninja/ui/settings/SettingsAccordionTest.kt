package ninja.jeremy.liveninja.ui.settings

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertSame
import org.junit.Test

class SettingsAccordionTest {

    @Test
    fun `shared section order is stable and about you starts expanded`() {
        assertEquals(
            listOf(
                SettingsSection.ABOUT_YOU,
                SettingsSection.WAKE_WORD,
                SettingsSection.PERSONA,
                SettingsSection.VOICE_ENGINE,
                SettingsSection.TURN_DETECTION,
                SettingsSection.APPEARANCE,
                SettingsSection.MICROPHONE,
                SettingsSection.PRIVACY,
                SettingsSection.ACCOUNT,
            ),
            SettingsSection.entries,
        )
        assertSame(SettingsSection.ABOUT_YOU, DEFAULT_EXPANDED_SETTINGS_SECTION)
    }

    @Test
    fun `activating a closed section makes it the only expanded section`() {
        assertSame(
            SettingsSection.VOICE_ENGINE,
            toggledSettingsSection(
                current = SettingsSection.ABOUT_YOU,
                activated = SettingsSection.VOICE_ENGINE,
            ),
        )
    }

    @Test
    fun `activating the open section collapses it`() {
        assertNull(
            toggledSettingsSection(
                current = SettingsSection.PRIVACY,
                activated = SettingsSection.PRIVACY,
            ),
        )
    }

    @Test
    fun `pending profile suggestions force about you on open`() {
        assertSame(
            SettingsSection.ABOUT_YOU,
            settingsSectionOnOpen(
                current = SettingsSection.ACCOUNT,
                hasPendingProfileSuggestions = true,
            ),
        )
        assertNull(
            settingsSectionOnOpen(
                current = null,
                hasPendingProfileSuggestions = false,
            ),
        )
    }

    @Test
    fun `header navigation wraps and supports home and end`() {
        val count = SettingsSection.entries.size

        assertEquals(
            0,
            targetSettingsHeaderIndex(count - 1, count, SettingsHeaderMove.NEXT),
        )
        assertEquals(
            count - 1,
            targetSettingsHeaderIndex(0, count, SettingsHeaderMove.PREVIOUS),
        )
        assertEquals(
            0,
            targetSettingsHeaderIndex(4, count, SettingsHeaderMove.FIRST),
        )
        assertEquals(
            count - 1,
            targetSettingsHeaderIndex(4, count, SettingsHeaderMove.LAST),
        )
    }
}
