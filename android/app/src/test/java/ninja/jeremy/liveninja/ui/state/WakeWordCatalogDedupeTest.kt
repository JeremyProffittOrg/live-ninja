package ninja.jeremy.liveninja.ui.state

import io.mockk.mockk
import java.util.Optional
import okhttp3.OkHttpClient
import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * The wake picker must show one row per phrase.
 *
 * The shipped list carries the bare slug `hey-live-ninja`, while a trained model of the same
 * phrase lives under a user-scoped id (`hey-live-ninja-47df2e`). Once the picker started reading
 * the authenticated catalog — which is the only source that knows about a user's trained phrases
 * — a signed-in user saw "Hey Live Ninja" twice: once as a placeholder promising a model, once
 * as the real thing.
 */
class WakeWordCatalogDedupeTest {

    private val repo = WakeWordCatalogRepository(mockk<OkHttpClient>(), Optional.empty())

    private val builtinSlug = WakeWordOption(
        id = "hey-live-ninja",
        label = "“Hey Live Ninja”",
        description = "Platform default · needs a trained model synced to this device",
        engines = listOf("openwakeword"),
    )
    private val trained = WakeWordOption(
        id = "hey-live-ninja-47df2e",
        label = "hey live ninja",
        description = "Trained · ready to use",
        engines = listOf("openwakeword"),
    )

    @Test
    fun `the trained entry wins over the placeholder slug for the same phrase`() {
        val collapsed = repo.collapseDuplicatePhrases(
            all = listOf(builtinSlug, trained),
            fromServer = listOf(trained),
        )

        assertEquals(1, collapsed.size)
        assertEquals("hey-live-ninja-47df2e", collapsed.single().id)
    }

    /** Curly quotes and casing are presentation, not identity. */
    @Test
    fun `quoted title case and bare lowercase are the same phrase`() {
        val collapsed = repo.collapseDuplicatePhrases(
            all = listOf(builtinSlug, trained),
            fromServer = listOf(trained),
        )
        assertEquals(1, collapsed.size)
    }

    /** Distinct phrases must all survive — dedupe must not eat the catalog. */
    @Test
    fun `different phrases are all kept`() {
        val automatica = WakeWordOption("hey-automatica-7e4f38", "hey automatica", "Trained", listOf("openwakeword"))
        val joshua = WakeWordOption("okay-joshua-5996e4", "okay joshua", "Trained", listOf("openwakeword"))
        val jarvis = WakeWordOption("hey-jarvis", "“Hey Jarvis”", "Built in", listOf("openwakeword"))

        val collapsed = repo.collapseDuplicatePhrases(
            all = listOf(jarvis, builtinSlug, trained, automatica, joshua),
            fromServer = listOf(trained, automatica, joshua),
        )

        assertEquals(
            listOf("hey-jarvis", "hey-live-ninja-47df2e", "hey-automatica-7e4f38", "okay-joshua-5996e4"),
            collapsed.map { it.id },
        )
    }

    /** With nothing from the server the shipped list is untouched. */
    @Test
    fun `offline keeps the shipped placeholder`() {
        val collapsed = repo.collapseDuplicatePhrases(
            all = listOf(builtinSlug),
            fromServer = emptyList(),
        )
        assertEquals(listOf("hey-live-ninja"), collapsed.map { it.id })
    }
}
