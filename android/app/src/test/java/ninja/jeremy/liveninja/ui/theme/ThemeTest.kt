package ninja.jeremy.liveninja.ui.theme

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Style-registry dispatch for M8.1 (ninja/minimal/terminal token sets ported
 * from web/static/css/app.css + STYLE_ACCENTS). Pure JVM: [androidx.compose
 * .ui.graphics.Color]/[androidx.compose.material3.ColorScheme] are plain
 * value/data types, no Android runtime or Robolectric needed.
 */
class ThemeTest {

    // ---- HAL: always dark, ignores the darkTheme param ----

    @Test
    fun `hal9000 colorScheme is identical regardless of darkTheme`() {
        assertEquals(
            liveNinjaColorScheme("hal9000", darkTheme = true).primary,
            liveNinjaColorScheme("hal9000", darkTheme = false).primary,
        )
        assertEquals(HalColorScheme.primary, liveNinjaColorScheme("hal9000", darkTheme = false).primary)
    }

    @Test
    fun `hal9000 colors are identical regardless of darkTheme`() {
        assertEquals(
            liveNinjaColors("hal9000", darkTheme = true),
            liveNinjaColors("hal9000", darkTheme = false),
        )
    }

    // ---- ninja/minimal/terminal: dark-mode primary honors STYLE_ACCENTS ----

    @Test
    fun `ninja dark primary is the STYLE_ACCENTS teal`() {
        assertEquals(HalTealForTest, liveNinjaColorScheme("ninja", darkTheme = true).primary)
    }

    @Test
    fun `minimal dark primary is the STYLE_ACCENTS cyan, distinct from ninja teal`() {
        val minimalPrimary = liveNinjaColorScheme("minimal", darkTheme = true).primary
        assertEquals(MinimalCyanForTest, minimalPrimary)
        assertNotEquals(liveNinjaColorScheme("ninja", darkTheme = true).primary, minimalPrimary)
    }

    @Test
    fun `terminal dark primary is the STYLE_ACCENTS green, distinct from ninja and minimal`() {
        val terminalPrimary = liveNinjaColorScheme("terminal", darkTheme = true).primary
        assertEquals(TerminalGreenForTest, terminalPrimary)
        assertNotEquals(liveNinjaColorScheme("ninja", darkTheme = true).primary, terminalPrimary)
        assertNotEquals(liveNinjaColorScheme("minimal", darkTheme = true).primary, terminalPrimary)
    }

    // ---- these three follow the light/dark axis (unlike HAL) ----

    @Test
    fun `ninja light and dark schemes differ`() {
        assertNotEquals(
            liveNinjaColorScheme("ninja", darkTheme = true).background,
            liveNinjaColorScheme("ninja", darkTheme = false).background,
        )
    }

    @Test
    fun `minimal light and dark schemes differ`() {
        assertNotEquals(
            liveNinjaColorScheme("minimal", darkTheme = true).background,
            liveNinjaColorScheme("minimal", darkTheme = false).background,
        )
    }

    @Test
    fun `terminal light and dark schemes differ`() {
        assertNotEquals(
            liveNinjaColorScheme("terminal", darkTheme = true).background,
            liveNinjaColorScheme("terminal", darkTheme = false).background,
        )
        // Dark terminal is literal app.css pure black.
        assertEquals(
            androidx.compose.ui.graphics.Color(0xFF000000),
            liveNinjaColorScheme("terminal", darkTheme = true).background,
        )
    }

    @Test
    fun `unrecognized style falls back to HAL`() {
        assertEquals(HalColorScheme.primary, liveNinjaColorScheme("nonexistent", darkTheme = true).primary)
        assertEquals(HalLiveNinjaColors, liveNinjaColors("nonexistent", darkTheme = false))
    }

    @Test
    fun `every non-HAL style's extended colors track the same primary as its colorScheme`() {
        for (style in listOf("ninja", "minimal", "terminal")) {
            for (dark in listOf(true, false)) {
                val scheme = liveNinjaColorScheme(style, dark)
                val extended = liveNinjaColors(style, dark)
                assertEquals("$style dark=$dark", scheme.primary, extended.orbAccent)
                assertTrue("$style dark=$dark orbCoreStops non-empty", extended.orbCoreStops.isNotEmpty())
            }
        }
    }

    companion object {
        // Mirrors web/static/js/theme.js STYLE_ACCENTS (source of truth for the
        // dark-mode primary — spec 03-theme §A / android-revamp-plan.md M8.1).
        private val HalTealForTest = androidx.compose.ui.graphics.Color(0xFF22E0D0)
        private val MinimalCyanForTest = androidx.compose.ui.graphics.Color(0xFF38D0FF)
        private val TerminalGreenForTest = androidx.compose.ui.graphics.Color(0xFF33FF66)
    }
}
