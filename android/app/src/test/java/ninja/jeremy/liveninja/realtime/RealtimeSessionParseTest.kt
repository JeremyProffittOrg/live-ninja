package ninja.jeremy.liveninja.realtime

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.fail
import org.junit.Test

/**
 * Per-mode parsing/validation of the `GET /api/v1/realtime/session` success
 * shapes ([RealtimeSessionApi.parseSession]) — especially the third,
 * gemini-direct shape added in M13 (gemini-plan.md §3.4).
 */
class RealtimeSessionParseTest {

    private fun geminiBody(): JSONObject = JSONObject(
        """
        {
          "mode": "gemini-direct",
          "engine": "gemini-flash-live",
          "model": "gemini-3.1-flash-live-preview",
          "voice": "Kore",
          "geminiEndpoint": "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContentConstrained",
          "accessToken": {
            "value": "auth_tokens/abc123",
            "expiresAt": "2026-07-19T12:30:00Z",
            "newSessionExpiresAt": "2026-07-19T12:02:00Z"
          },
          "sessionConfig": {"model": "models/gemini-3.1-flash-live-preview"},
          "sessionId": "rs-9"
        }
        """.trimIndent(),
    )

    @Test
    fun geminiDirect_parsesEndpointTokenAndSessionConfig() {
        val session = RealtimeSessionApi.parseSession(geminiBody(), 200, null)

        assertEquals(RealtimeSession.MODE_GEMINI_DIRECT, session.mode)
        assertEquals(
            "wss://generativelanguage.googleapis.com/ws/" +
                "google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContentConstrained",
            session.geminiEndpoint,
        )
        val token = session.accessToken
        assertNotNull(token)
        assertEquals("auth_tokens/abc123", token!!.value)
        assertEquals("2026-07-19T12:30:00Z", token.expiresAt)
        assertEquals("2026-07-19T12:02:00Z", token.newSessionExpiresAt)
        assertEquals(
            "models/gemini-3.1-flash-live-preview",
            session.sessionConfig?.optString("model"),
        )
        assertEquals("gemini-3.1-flash-live-preview", session.model)
        assertEquals("Kore", session.voice)
        assertEquals("rs-9", session.sessionId)
        // Never inherits the Nova field family (§3.4 naming rule).
        assertNull(session.wsUrl)
    }

    @Test
    fun geminiDirect_missingRequiredFields_throwsInvalidResponse() {
        for (missing in listOf("geminiEndpoint", "accessToken", "sessionConfig")) {
            val body = geminiBody().also { it.remove(missing) }
            try {
                RealtimeSessionApi.parseSession(body, 200, null)
                fail("expected invalid_response when $missing is absent")
            } catch (e: RealtimeSessionException) {
                assertEquals("invalid_response", e.kind)
            }
        }
    }

    @Test
    fun openAiDirect_shapeUnchanged() {
        val body = JSONObject(
            """
            {
              "clientSecret": {"value": "ek_secret", "expiresAt": "2026-07-19T12:01:00Z"},
              "model": "gpt-realtime",
              "voice": "cedar",
              "sessionId": "rs-1"
            }
            """.trimIndent(),
        )
        val session = RealtimeSessionApi.parseSession(body, 200, "daily_minutes=83%")

        assertEquals(RealtimeSession.MODE_OPENAI_DIRECT, session.mode)
        assertEquals("ek_secret", session.clientSecret)
        assertEquals("daily_minutes=83%", session.quotaWarning)
        assertNull(session.geminiEndpoint)
        assertNull(session.accessToken)
        assertNull(session.sessionConfig)
    }

    @Test
    fun novaBridge_parsesRequiredSessionConfig() {
        val body = JSONObject(
            """
            {
              "mode": "nova-bridge",
              "wsUrl": "wss://nova.live.jeremy.ninja/session?sid=rs-3",
              "token": "bridge-token",
              "sessionConfig": {
                "voice": "matthew",
                "sampleRateIn": 16000,
                "sampleRateOut": 24000,
                "systemPrompt": "Be concise."
              },
              "sessionId": "rs-3"
            }
            """.trimIndent(),
        )
        val session = RealtimeSessionApi.parseSession(body, 200, null)

        assertEquals(RealtimeSession.MODE_NOVA_BRIDGE, session.mode)
        assertEquals("wss://nova.live.jeremy.ninja/session?sid=rs-3", session.wsUrl)
        assertEquals("bridge-token", session.bridgeToken)
        assertEquals("matthew", session.sessionConfig?.optString("voice"))
        assertEquals(16_000, session.sessionConfig?.optInt("sampleRateIn"))
        assertNull(session.geminiEndpoint)
    }

    @Test
    fun novaBridge_missingSessionConfig_throwsInvalidResponse() {
        val body = JSONObject(
            """
            {
              "mode": "nova-bridge",
              "wsUrl": "wss://nova.live.jeremy.ninja/session?sid=rs-3",
              "token": "bridge-token",
              "sessionId": "rs-3"
            }
            """.trimIndent(),
        )

        try {
            RealtimeSessionApi.parseSession(body, 200, null)
            fail("expected invalid_response when sessionConfig is absent")
        } catch (e: RealtimeSessionException) {
            assertEquals("invalid_response", e.kind)
        }
    }

    /**
     * The R7 fix. `parseSession` never read `callsUrl`, so
     * [RealtimeSession.callsUrl] was always the compile-time OpenAI constant
     * no matter what the broker said — an azure-direct session would have
     * POSTed its Azure ephemeral secret to api.openai.com.
     */
    @Test
    fun azureDirect_usesTheServerSuppliedCallsUrl() {
        val body = JSONObject(
            """
            {
              "mode": "azure-direct",
              "clientSecret": {"value": "ek_azure", "expiresAt": "2026-08-24T12:01:00Z"},
              "model": "gpt-realtime-2-1",
              "voice": "cedar",
              "sessionId": "rs-az-1",
              "callsUrl": "https://ln-aoai-eastus2.openai.azure.com/openai/v1/realtime/calls"
            }
            """.trimIndent(),
        )
        val session = RealtimeSessionApi.parseSession(body, 200, null)

        assertEquals(RealtimeSession.MODE_AZURE_DIRECT, session.mode)
        assertEquals("ek_azure", session.clientSecret)
        assertEquals("gpt-realtime-2-1", session.model)
        assertEquals(
            "https://ln-aoai-eastus2.openai.azure.com/openai/v1/realtime/calls",
            session.callsUrl,
        )
    }

    /**
     * The other half: a response with no `callsUrl` must still reach OpenAI,
     * so this build keeps working against a broker that predates the field.
     */
    @Test
    fun openAiDirect_withoutCallsUrl_fallsBackToTheOpenAiHost() {
        val body = JSONObject(
            """
            {
              "clientSecret": {"value": "ek_secret"},
              "model": "gpt-realtime"
            }
            """.trimIndent(),
        )
        val session = RealtimeSessionApi.parseSession(body, 200, null)

        assertEquals(
            ninja.jeremy.liveninja.config.BackendConfig.OPENAI_REALTIME_CALLS_URL,
            session.callsUrl,
        )
    }

    /**
     * An explicit OpenAI `callsUrl` on the wire must be honoured too — the
     * broker emits it on openai-direct precisely so the field is exercised by
     * the default path and cannot rot between releases.
     */
    @Test
    fun openAiDirect_honoursAnExplicitCallsUrl() {
        val body = JSONObject(
            """
            {
              "clientSecret": {"value": "ek_secret"},
              "model": "gpt-realtime",
              "callsUrl": "https://api.openai.com/v1/realtime/calls"
            }
            """.trimIndent(),
        )
        val session = RealtimeSessionApi.parseSession(body, 200, null)
        assertEquals("https://api.openai.com/v1/realtime/calls", session.callsUrl)
    }
}
