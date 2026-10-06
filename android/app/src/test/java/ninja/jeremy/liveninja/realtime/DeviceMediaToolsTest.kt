package ninja.jeremy.liveninja.realtime

import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.runBlocking
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

/** Fake platform seam shared by the media tool and coordinator tests. */
internal class FakeMediaGateway : MediaLaunchGateway {
    @Volatile var state: MediaForegroundState = MediaForegroundState.READY
    @Volatile var resolver: (MediaLaunchCandidate) -> Boolean = { true }
    @Volatile var starter: (MediaLaunchCandidate) -> Unit = {}
    val foregroundChecks = AtomicInteger()
    val startAttempts = AtomicInteger()
    val resolved = CopyOnWriteArrayList<MediaLaunchCandidate>()
    val started = CopyOnWriteArrayList<MediaLaunchCandidate>()
    val toasts = CopyOnWriteArrayList<String>()

    override fun foregroundState(): MediaForegroundState {
        foregroundChecks.incrementAndGet()
        return state
    }

    override fun canResolve(candidate: MediaLaunchCandidate): Boolean {
        resolved += candidate
        return resolver(candidate)
    }

    override fun start(candidate: MediaLaunchCandidate) {
        startAttempts.incrementAndGet()
        starter(candidate)
        started += candidate
    }

    override fun showHandoffToast(message: String) {
        toasts += message
    }
}

class DeviceMediaToolsTest {

    private fun execute(
        arguments: String,
        gateway: FakeMediaGateway,
        current: () -> Boolean = { true },
    ): DeviceMediaToolResult = runBlocking {
        DeviceMediaToolHandler(gateway, Dispatchers.Unconfined).execute("call-1", arguments, current)
    }

    private fun args(kind: String, query: String): String =
        JSONObject().put("kind", kind).put("query", query).toString()

    private fun successOutput(result: DeviceMediaToolResult): JSONObject {
        assertTrue(result.launched)
        val json = JSONObject(result.output)
        assertTrue(json.getBoolean("ok"))
        assertEquals(PLAY_MEDIA_TOOL_NAME, json.getString("tool"))
        val out = json.getJSONObject("output")
        assertFalse("playback is never confirmed", out.getBoolean("playbackConfirmed"))
        return out
    }

    private fun error(result: DeviceMediaToolResult): JSONObject {
        assertFalse(result.launched)
        val json = JSONObject(result.output)
        assertFalse(json.getBoolean("ok"))
        return json.getJSONObject("error")
    }

    /** Raw arguments whose query is `a`, one JSON `\u` escape with [hex] as its body, then `b`. */
    private fun queryWithUnicodeEscape(hex: String): String =
        "{\"kind\":\"music\",\"query\":\"a\\u" + hex + "b\"}"

    @Test
    fun music_prefersScopedPlayFromSearchWithExactQuery() {
        val gateway = FakeMediaGateway()
        val out = successOutput(execute(args("music", "Clair de Lune"), gateway))
        assertEquals(
            listOf(MediaLaunchCandidate.PlayFromSearch(AllowedMediaApp.YOUTUBE_MUSIC, "Clair de Lune")),
            gateway.started,
        )
        assertEquals("playback_requested", out.getString("status"))
        assertEquals("YouTube Music", out.getString("app"))
        assertEquals("music", out.getString("kind"))
        assertEquals("Clair de Lune", out.getString("query"))
        assertTrue(out.getString("instruction").contains("never say it is playing"))
        assertEquals(1, gateway.toasts.size)
        assertTrue(gateway.toasts.single().contains("isn't guaranteed"))
    }

    @Test
    fun music_fallsBackToScopedYouTubeMusicSearchWhenPlayFromSearchUnsupported() {
        val gateway = FakeMediaGateway().apply {
            resolver = { it !is MediaLaunchCandidate.PlayFromSearch }
        }
        val out = successOutput(execute(args("music", "lofi"), gateway))
        assertEquals(
            listOf(
                MediaLaunchCandidate.PlayFromSearch(AllowedMediaApp.YOUTUBE_MUSIC, "lofi"),
                MediaLaunchCandidate.HttpsSearch(AllowedMediaApp.YOUTUBE_MUSIC, MediaSearchSite.YOUTUBE_MUSIC, "lofi"),
            ),
            gateway.resolved,
        )
        assertEquals(
            MediaLaunchCandidate.HttpsSearch(AllowedMediaApp.YOUTUBE_MUSIC, MediaSearchSite.YOUTUBE_MUSIC, "lofi"),
            gateway.started.single(),
        )
        assertEquals("opened_app_search", out.getString("status"))
        assertEquals("app", out.getString("target"))
    }

    @Test
    fun music_fallsBackToWebSearchWithoutClaimingAppIsMissing() {
        val gateway = FakeMediaGateway().apply { resolver = { it.app == null } }
        val out = successOutput(execute(args("music", "jazz"), gateway))
        assertEquals(
            MediaLaunchCandidate.HttpsSearch(null, MediaSearchSite.YOUTUBE_MUSIC, "jazz"),
            gateway.started.single(),
        )
        assertEquals(3, gateway.resolved.size)
        assertEquals("opened_web_search", out.getString("status"))
        assertEquals("web", out.getString("target"))
        val instruction = out.getString("instruction").lowercase()
        assertFalse(instruction.contains("not installed"))
        assertFalse(instruction.contains("missing"))
        assertTrue(instruction.contains("unavailable or did not accept"))
    }

    @Test
    fun video_usesScopedYouTubeSearchFirst() {
        val gateway = FakeMediaGateway()
        val out = successOutput(execute(args("video", "how to tie a tie"), gateway))
        assertEquals(
            MediaLaunchCandidate.HttpsSearch(AllowedMediaApp.YOUTUBE, MediaSearchSite.YOUTUBE, "how to tie a tie"),
            gateway.started.single(),
        )
        assertEquals("opened_app_search", out.getString("status"))
        assertEquals("YouTube", out.getString("app"))
        assertEquals("video", out.getString("kind"))
    }

    @Test
    fun video_fallsBackToUnscopedWebSearchWhenYouTubeUnavailable() {
        val gateway = FakeMediaGateway().apply { resolver = { it.app == null } }
        val out = successOutput(execute(args("video", "cats"), gateway))
        assertEquals(
            MediaLaunchCandidate.HttpsSearch(null, MediaSearchSite.YOUTUBE, "cats"),
            gateway.started.single(),
        )
        assertEquals("opened_web_search", out.getString("status"))
    }

    @Test
    fun noAppAndNoBrowser_returnsStructuredErrorWithoutLaunch() {
        val gateway = FakeMediaGateway().apply { resolver = { false } }
        val err = error(execute(args("music", "anything"), gateway))
        assertEquals("no_handler", err.getString("code"))
        assertTrue(gateway.started.isEmpty())
        assertTrue(gateway.toasts.isEmpty())
    }

    @Test
    fun locked_failsSafelyWithUnlockGuidance() {
        val gateway = FakeMediaGateway().apply { state = MediaForegroundState.LOCKED }
        val err = error(execute(args("music", "x"), gateway))
        assertEquals("device_locked", err.getString("code"))
        assertTrue(err.getString("message").contains("Unlock your phone, open Live Ninja, then ask again"))
        assertTrue(gateway.resolved.isEmpty())
        assertTrue(gateway.started.isEmpty())
    }

    @Test
    fun backgroundOrNoResumedActivity_failsWithOpenAppGuidance() {
        val gateway = FakeMediaGateway().apply { state = MediaForegroundState.UNAVAILABLE }
        val err = error(execute(args("video", "x"), gateway))
        assertEquals("app_not_foreground", err.getString("code"))
        assertTrue(err.getString("message").contains("open Live Ninja"))
        assertTrue(gateway.started.isEmpty())
    }

    @Test
    fun lockBetweenResolveAndStart_preventsLaunch() {
        val gateway = FakeMediaGateway()
        gateway.resolver = {
            gateway.state = MediaForegroundState.LOCKED
            true
        }
        val err = error(execute(args("music", "x"), gateway))
        assertEquals("device_locked", err.getString("code"))
        assertTrue(gateway.started.isEmpty())
    }

    @Test
    fun launcherSecurityException_failsClosedWithoutFallbackOrLeakedDetails() {
        val gateway = FakeMediaGateway().apply {
            starter = {
                if (it is MediaLaunchCandidate.PlayFromSearch) throw SecurityException("internal secret detail")
            }
        }
        val result = execute(args("music", "x"), gateway)
        val err = error(result)
        assertEquals("forbidden", err.getString("code"))
        assertEquals("Android blocked opening a media app or browser for that search.", err.getString("message"))
        assertFalse(result.output.contains("internal secret detail"))
        assertFalse(result.output.contains("SecurityException"))
        assertEquals("no other candidate is even resolved", 1, gateway.resolved.size)
        assertEquals("exactly one start attempt", 1, gateway.startAttempts.get())
        assertTrue(gateway.started.isEmpty())
        assertTrue(gateway.toasts.isEmpty())
    }

    @Test
    fun resolverSecurityException_failsClosedBeforeAnyStart() {
        val gateway = FakeMediaGateway().apply {
            resolver = { throw SecurityException("resolver secret detail") }
        }
        val result = execute(args("music", "x"), gateway)
        val err = error(result)
        assertEquals("forbidden", err.getString("code"))
        assertEquals("Android blocked opening a media app or browser for that search.", err.getString("message"))
        assertFalse(result.output.contains("resolver secret detail"))
        assertFalse(result.output.contains("SecurityException"))
        assertEquals("one resolve attempt, no fallback", 1, gateway.resolved.size)
        assertEquals(0, gateway.startAttempts.get())
        assertTrue(gateway.started.isEmpty())
        assertTrue(gateway.toasts.isEmpty())
    }

    @Test
    fun videoLauncherSecurityException_neverFallsBackToBrowser() {
        val gateway = FakeMediaGateway().apply { starter = { throw SecurityException("private detail") } }
        val result = execute(args("video", "x"), gateway)
        val err = error(result)
        assertEquals("forbidden", err.getString("code"))
        assertFalse(result.output.contains("private detail"))
        assertEquals(1, gateway.resolved.size)
        assertEquals(1, gateway.startAttempts.get())
        assertTrue(gateway.started.isEmpty())
        assertTrue(gateway.toasts.isEmpty())
    }

    @Test
    fun activityNotFoundStyleFailure_movesToNextCandidate() {
        val gateway = FakeMediaGateway().apply {
            starter = { if (it.app != null) throw RuntimeException("No Activity found to handle Intent") }
        }
        val result = execute(args("video", "x"), gateway)
        val out = successOutput(result)
        assertEquals("opened_web_search", out.getString("status"))
        assertFalse(result.output.contains("No Activity found"))
    }

    @Test
    fun foregroundLostAfterSuccessfulStart_neverLaunchesFallback() {
        val gateway = FakeMediaGateway()
        gateway.starter = { gateway.state = MediaForegroundState.UNAVAILABLE }
        val out = successOutput(execute(args("music", "x"), gateway))
        assertEquals(1, gateway.started.size)
        assertEquals("playback_requested", out.getString("status"))
    }

    @Test
    fun unicodeAndReservedCharacters_reachGatewayUnchanged() {
        val raw = """{"kind":"video","query":" Beyoncé — AC\/DC & \"Live\" #1 ?x=y+z 100% 🎵 "}"""
        val expected = " Beyoncé — AC/DC & \"Live\" #1 ?x=y+z 100% 🎵 "
        val gateway = FakeMediaGateway()
        val out = successOutput(execute(raw, gateway))
        val candidate = gateway.started.single() as MediaLaunchCandidate.HttpsSearch
        // Natural query (including surrounding spaces) is preserved exactly;
        // URI encoding happens only in the Android gateway's Uri.Builder.
        assertEquals(expected, candidate.query)
        assertEquals("www.youtube.com", candidate.site.host)
        assertEquals("/results", candidate.site.path)
        assertEquals("search_query", candidate.site.queryParameter)
        assertEquals(expected, out.getString("query"))
    }

    @Test
    fun staleSession_neverLaunches() {
        val gateway = FakeMediaGateway()
        val err = error(execute(args("music", "x"), gateway) { false })
        assertEquals("session_changed", err.getString("code"))
        assertTrue(gateway.started.isEmpty())

        var current = true
        val racing = FakeMediaGateway().apply {
            resolver = {
                current = false
                true
            }
        }
        val raced = error(execute(args("music", "x"), racing) { current })
        assertEquals("session_changed", raced.getString("code"))
        assertTrue(racing.started.isEmpty())
    }

    @Test
    fun cancellationIsNeverSwallowed() {
        val gateway = FakeMediaGateway().apply { starter = { throw CancellationException("cancelled") } }
        try {
            execute(args("music", "x"), gateway)
            fail("expected cancellation to propagate")
        } catch (_: CancellationException) {
            // expected
        }
    }

    @Test
    fun malformedUnknownTrailingAndMistypedArguments_areRejectedBeforeAnyPlatformCall() {
        val bad = listOf(
            "",
            "null",
            "[]",
            """"music"""",
            """{"kind":"music"}""",
            """{"query":"x"}""",
            """{"kind":"podcast","query":"x"}""",
            """{"kind":"Music","query":"x"}""",
            """{"kind":1,"query":"x"}""",
            """{"kind":"music","query":42}""",
            """{"kind":"music","query":true}""",
            """{"kind":"music","query":null}""",
            """{"kind":"music","query":["x"]}""",
            """{"kind":"music","query":{"q":"x"}}""",
            """{"kind":"music","query":""}""",
            """{"kind":"music","query":"   "}""",
            """{"kind":"music","query":"x","url":"https://evil.example"}""",
            """{"kind":"music","query":"x","package":"com.evil.app"}""",
            """{"kind":"music","query":"x","kind":"video"}""",
            """{"kind":"music","query":"x"} {}""",
            """{"kind":"music","query":"x"}garbage""",
            """{"kind":"music","query":"x",}""",
            """{'kind':'music','query':'x'}""",
            """{kind:"music",query:"x"}""",
            "{\"kind\":\"music\",\"query\":\"a\u0001b\"}",
            """{"kind":"music","query":"a\nb"}""",
            """{"kind":"music","query":"a\u0000b"}""",
            """{"kind":"music","query":"a\u0085b"}""",
            """{"kind":"music","query":"\ud800"}""",
            """{"kind":"music","query":"x"""",
            """{"kind":"music","query":"x\q"}""",
            // Trailing NUL after the document, built at runtime from a textual escape.
            "{\"kind\":\"music\",\"query\":\"x\"}\u0000",
        )
        for (arguments in bad) {
            val gateway = FakeMediaGateway()
            val err = error(execute(arguments, gateway))
            assertEquals("rejected: $arguments", "invalid_args", err.getString("code"))
            assertEquals(0, gateway.foregroundChecks.get())
            assertTrue(gateway.resolved.isEmpty())
            assertTrue(gateway.started.isEmpty())
        }
    }

    @Test
    fun unicodeEscapes_rejectNonAsciiHexDigitsBeforeAnyPlatformCall() {
        val rejected = listOf(
            "００４１", // fullwidth digits "0041"
            "٠٠٤١", // Arabic-Indic digits "0041"
            "00٤١", // mixed ASCII and Arabic-Indic digits
            "00４Ａ", // fullwidth digit and fullwidth uppercase letter
            "00ｅ９", // fullwidth lowercase letter and fullwidth digit
            "ＡＡＡＡ", // fullwidth uppercase letters
            "00eｙ", // fullwidth lowercase y
        )
        for (hex in rejected) {
            val arguments = queryWithUnicodeEscape(hex)
            val gateway = FakeMediaGateway()
            val err = error(execute(arguments, gateway))
            assertEquals("rejected: $arguments", "invalid_args", err.getString("code"))
            assertEquals(0, gateway.foregroundChecks.get())
            assertTrue(gateway.resolved.isEmpty())
            assertTrue(gateway.started.isEmpty())
        }
    }

    @Test
    fun unicodeEscapes_acceptAsciiUpperAndLowerHexDigits() {
        val accepted = listOf(
            "0041" to "aAb",
            "00e9" to "aéb",
            "00E9" to "aéb",
            "4e2D" to "a中b",
            "20ac" to "a€b",
        )
        for ((hex, expected) in accepted) {
            val gateway = FakeMediaGateway()
            val out = successOutput(execute(queryWithUnicodeEscape(hex), gateway))
            assertEquals("escape $hex", expected, out.getString("query"))
            assertEquals(1, gateway.started.size)
        }
    }

    @Test
    fun surroundingJsonWhitespaceIsAccepted() {
        val gateway = FakeMediaGateway()
        successOutput(execute(" \n\t{ \"kind\" : \"music\" , \"query\" : \"x\" }\r\n ", gateway))
        assertEquals(1, gateway.started.size)
    }

    @Test
    fun queryLimitCountsUnicodeCodePoints() {
        val note = "🎵"
        successOutput(execute(args("music", note.repeat(500)), FakeMediaGateway()))
        successOutput(execute(args("music", "a".repeat(500)), FakeMediaGateway()))
        assertEquals("invalid_args", error(execute(args("music", "a".repeat(501)), FakeMediaGateway())).getString("code"))
        assertEquals("invalid_args", error(execute(args("music", note.repeat(501)), FakeMediaGateway())).getString("code"))
    }

    @Test
    fun rawArgumentBoundRejectsOversizeButAdmitsFullyEscapedMaximumQuery() {
        val padded = args("music", "x") + " ".repeat(9_000)
        assertEquals("invalid_args", error(execute(padded, FakeMediaGateway())).getString("code"))

        val escaped = "{\"kind\":\"music\",\"query\":\"" + "\\ud83c\\udfb5".repeat(500) + "\"}"
        assertTrue(escaped.length <= PlayMediaArguments.MAX_ARGUMENT_BYTES)
        val out = successOutput(execute(escaped, FakeMediaGateway()))
        assertEquals("🎵".repeat(500), out.getString("query"))
    }

    @Test
    fun duplicateCallId_reusesLocalToolCallResultsWithoutSecondLaunch() = runBlocking {
        val gateway = FakeMediaGateway()
        val handler = DeviceMediaToolHandler(gateway, Dispatchers.Unconfined)
        val results = LocalToolCallResults()
        val arguments = args("music", "x")
        val first = results.getOrExecute("dup") { handler.execute("dup", arguments) { true }.output }
        val second = results.getOrExecute("dup") { handler.execute("dup", arguments) { true }.output }
        assertTrue(first.shouldRespond)
        assertFalse(second.shouldRespond)
        assertEquals(first.output, second.output)
        assertEquals(1, gateway.started.size)
    }

    @Test
    fun chipSummaryIsMeaningfulAndHonest() {
        val ok = execute(args("music", "x"), FakeMediaGateway()).output
        assertEquals("Asked YouTube Music to play (not confirmed)", playMediaChipSummary(ok))
        val web = execute(args("video", "x"), FakeMediaGateway().apply { resolver = { it.app == null } }).output
        assertEquals("Opened web search (not confirmed)", playMediaChipSummary(web))
        val failed = execute(args("video", "x"), FakeMediaGateway().apply { resolver = { false } }).output
        assertEquals(
            "Neither the YouTube apps nor a web browser on this device could open that search.",
            playMediaChipSummary(failed),
        )
    }
}
