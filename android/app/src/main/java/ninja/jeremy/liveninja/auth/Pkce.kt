package ninja.jeremy.liveninja.auth

import java.security.MessageDigest
import java.security.SecureRandom
import java.util.Base64

/**
 * RFC 7636 PKCE helpers (S256) plus the OAuth `state` nonce, shared by the
 * LWA Custom-Tabs login flow.
 */
object Pkce {

    private val random = SecureRandom()

    /** URL-safe base64, no padding — the encoding RFC 7636 mandates. */
    private fun b64Url(bytes: ByteArray): String =
        Base64.getUrlEncoder().withoutPadding().encodeToString(bytes)

    /**
     * A fresh `code_verifier`: 64 random octets -> 86 base64url chars
     * (within RFC 7636's 43..128 length bounds).
     */
    fun newCodeVerifier(): String {
        val bytes = ByteArray(64)
        random.nextBytes(bytes)
        return b64Url(bytes)
    }

    /** S256 `code_challenge` for [verifier]: base64url(sha256(ascii(verifier))). */
    fun codeChallengeS256(verifier: String): String {
        val digest = MessageDigest.getInstance("SHA-256")
        return b64Url(digest.digest(verifier.toByteArray(Charsets.US_ASCII)))
    }

    /** A fresh OAuth `state` nonce (32 random octets, base64url). */
    fun newState(): String {
        val bytes = ByteArray(32)
        random.nextBytes(bytes)
        return b64Url(bytes)
    }
}
