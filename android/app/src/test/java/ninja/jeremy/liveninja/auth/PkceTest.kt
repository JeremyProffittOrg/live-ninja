package ninja.jeremy.liveninja.auth

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/** RFC 7636 PKCE helpers: verifier shape, S256 challenge, state nonce. */
class PkceTest {

    /** Appendix B of RFC 7636 — the canonical S256 test vector. */
    @Test
    fun codeChallenge_matchesRfc7636AppendixB() {
        val verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
        assertEquals(
            "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
            Pkce.codeChallengeS256(verifier),
        )
    }

    @Test
    fun newCodeVerifier_lengthWithinRfcBounds() {
        repeat(20) {
            val v = Pkce.newCodeVerifier()
            // 64 random octets -> 86 base64url chars; RFC bounds are 43..128.
            assertEquals(86, v.length)
            assertTrue(v.length in 43..128)
        }
    }

    @Test
    fun newCodeVerifier_usesOnlyUnreservedBase64UrlChars() {
        repeat(20) {
            val v = Pkce.newCodeVerifier()
            assertTrue(
                "verifier must be base64url without padding: $v",
                v.all { it.isLetterOrDigit() || it == '-' || it == '_' },
            )
        }
    }

    @Test
    fun newCodeVerifier_isUniquePerCall() {
        val seen = HashSet<String>()
        repeat(50) { assertTrue(seen.add(Pkce.newCodeVerifier())) }
    }

    @Test
    fun challenge_isDeterministicForSameVerifier_andDiffersAcrossVerifiers() {
        val v1 = Pkce.newCodeVerifier()
        val v2 = Pkce.newCodeVerifier()
        assertEquals(Pkce.codeChallengeS256(v1), Pkce.codeChallengeS256(v1))
        assertNotEquals(Pkce.codeChallengeS256(v1), Pkce.codeChallengeS256(v2))
    }

    @Test
    fun newState_isBase64UrlAndUnique() {
        val s1 = Pkce.newState()
        val s2 = Pkce.newState()
        // 32 octets -> 43 base64url chars, no padding.
        assertEquals(43, s1.length)
        assertTrue(s1.all { it.isLetterOrDigit() || it == '-' || it == '_' })
        assertNotEquals(s1, s2)
    }
}
