package ninja.jeremy.liveninja.log

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Fixtures mirror the exact header shapes built by
 * `net/AuthInterceptor.kt` (`.header("Authorization", "Bearer $it")`) and
 * `net/TokenAuthenticator.kt` (`request.header("Authorization")
 * ?.removePrefix("Bearer ")`, `.header("Authorization", "Bearer
 * ${outcome.accessToken}")`) — a real JWT-shaped access token, and the raw
 * bearer/JWT text as it would appear if either class's request/response
 * were ever dumped into a log line.
 */
class RedactorTest {

    private val jwtAccessToken =
        "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyLTEyMyIsImV4cCI6MTc1MzAwMDAwMH0.QGx1nF3z9K2sVv8pR1dY7wM0eA5bC6dE7fG8hI9jK0l"

    @Test
    fun `redacts a full Authorization header line as AuthInterceptor builds it`() {
        val line = "Adding header Authorization: Bearer $jwtAccessToken"
        val redacted = Redactor.redact(line)
        assertFalse("raw JWT must not survive redaction", redacted.contains(jwtAccessToken))
        assertFalse("raw token substring must not survive redaction", redacted.contains("eyJ"))
        assertTrue(redacted.contains("[REDACTED"))
        assertTrue(redacted.startsWith("Adding header Authorization: [REDACTED"))
    }

    @Test
    fun `redacts the TokenAuthenticator retry header the same way`() {
        // TokenAuthenticator: request.newBuilder().header("Authorization", "Bearer ${outcome.accessToken}")
        val line = """request.newBuilder().header("Authorization", "Bearer $jwtAccessToken")"""
        val redacted = Redactor.redact(line)
        assertFalse(redacted.contains(jwtAccessToken))
        assertTrue(redacted.contains("[REDACTED"))
    }

    @Test
    fun `redacts the staleToken extracted via removePrefix even without a Bearer prefix`() {
        // TokenAuthenticator: staleToken = request.header("Authorization")?.removePrefix("Bearer ")
        val line = "refresh attempt for staleToken=$jwtAccessToken"
        val redacted = Redactor.redact(line)
        assertFalse(redacted.contains(jwtAccessToken))
        assertTrue(redacted.contains("[REDACTED-JWT]"))
    }

    @Test
    fun `redacts opaque non-JWT bearer tokens`() {
        val line = "Authorization: Bearer opaque-session-token-abc123XYZ=="
        val redacted = Redactor.redact(line)
        assertFalse(redacted.contains("opaque-session-token-abc123XYZ"))
    }

    @Test
    fun `redacts X-Api-Key header values`() {
        val line = "outgoing headers: X-Api-Key: sk-live-abcdef1234567890"
        val redacted = Redactor.redact(line)
        assertFalse(redacted.contains("sk-live-abcdef1234567890"))
        assertTrue(redacted.contains("X-Api-Key: [REDACTED]"))
    }

    @Test
    fun `redacts Cookie header values`() {
        val line = "Cookie: session=abcxyz789; other=1"
        val redacted = Redactor.redact(line)
        assertFalse(redacted.contains("abcxyz789"))
        assertTrue(redacted.contains("Cookie: [REDACTED]"))
    }

    @Test
    fun `header redaction is case-insensitive on the header name`() {
        val line = "authorization: Bearer $jwtAccessToken"
        val redacted = Redactor.redact(line)
        assertFalse(redacted.contains(jwtAccessToken))
    }

    @Test
    fun `leaves ordinary log lines untouched`() {
        val line = "WakeWordService: detection score=0.87 threshold=0.5"
        assertEquals(line, Redactor.redact(line))
    }

    @Test
    fun `bare JWT anywhere in a message is redacted`() {
        val line = "session bootstrap returned accessToken=$jwtAccessToken expiresIn=3600"
        val redacted = Redactor.redact(line)
        assertFalse(redacted.contains(jwtAccessToken))
        assertTrue(redacted.contains("[REDACTED-JWT]"))
    }
}
