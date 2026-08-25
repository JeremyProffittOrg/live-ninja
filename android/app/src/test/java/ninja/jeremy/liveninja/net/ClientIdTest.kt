package ninja.jeremy.liveninja.net

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The `X-LN-Client` value this build puts on the wire must satisfy
 * `contracts/headers.md`'s grammar. It did not: `VERSION_NAME` carries a
 * pre-release suffix ("0.2.2-hal"), which the grammar has no room for, so the
 * header was rejected by every server-side parser and this client counted as
 * "unknown" on every request.
 *
 * That is not cosmetic — the Azure client gate falls back to a version check
 * when a client declares no capabilities, and an unparseable header can never
 * satisfy it.
 */
class ClientIdTest {

    /** contracts/headers.md, verbatim. */
    private val grammar = Regex("""^(web|android|m5stack)/(\d+)\.(\d+)\.(\d+)\+([A-Za-z0-9._-]+)$""")

    @Test
    fun headerValue_matchesTheContractGrammar() {
        assertTrue(
            "X-LN-Client value '${ClientId.HEADER_VALUE}' does not match contracts/headers.md",
            grammar.matches(ClientId.HEADER_VALUE),
        )
    }

    @Test
    fun headerValue_carriesNoPreReleaseSuffix() {
        val semver = ClientId.HEADER_VALUE.substringAfter('/').substringBefore('+')
        assertFalse(
            "the wire semver must not carry a pre-release suffix, got '$semver'",
            semver.contains('-'),
        )
    }

    /**
     * The capability list must not name a transport this build has not
     * written: the broker would hand over a credential the client cannot use.
     */
    @Test
    fun capabilities_declareOnlyImplementedTransports() {
        assertTrue(
            "azure-direct must be declared or the broker will never route this client to Azure",
            ClientId.CAPABILITIES.contains("azure-direct"),
        )
        assertFalse(
            "voice-live-direct is not implemented in this build",
            ClientId.CAPABILITIES.contains("voice-live-direct"),
        )
    }
}
