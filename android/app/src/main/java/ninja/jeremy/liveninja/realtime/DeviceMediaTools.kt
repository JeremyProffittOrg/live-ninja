package ninja.jeremy.liveninja.realtime

import android.app.KeyguardManager
import android.app.SearchManager
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Looper
import android.provider.MediaStore
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.lifecycle.Lifecycle
import java.lang.ref.WeakReference
import javax.inject.Inject
import javax.inject.Singleton
import kotlin.coroutines.CoroutineContext
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONException
import org.json.JSONObject

/** Server-advertised name intercepted locally before [ToolCallRouter]. */
const val PLAY_MEDIA_TOOL_NAME = "play_media"

/**
 * play_media is a one-shot HANDOFF: Live Ninja asks a fixed, allow-listed
 * media app (or, honestly labelled, a web search) to take over, then the voice
 * conversation ends so the media can be heard. It never controls the third
 * party afterwards and never claims playback was confirmed.
 */
internal enum class MediaKind(val wireName: String) {
    MUSIC("music"),
    VIDEO("video"),
    ;

    companion object {
        fun fromWireName(name: String): MediaKind? = entries.firstOrNull { it.wireName == name }
    }
}

internal data class MediaRequest(val kind: MediaKind, val query: String)

/** The ONLY packages play_media may target. No arbitrary package names. */
internal enum class AllowedMediaApp(val packageName: String, val displayName: String) {
    YOUTUBE_MUSIC("com.google.android.apps.youtube.music", "YouTube Music"),
    YOUTUBE("com.google.android.youtube", "YouTube"),
}

/** The ONLY HTTPS search endpoints play_media may open. No arbitrary URLs. */
internal enum class MediaSearchSite(val host: String, val path: String, val queryParameter: String) {
    YOUTUBE_MUSIC("music.youtube.com", "/search", "q"),
    YOUTUBE("www.youtube.com", "/results", "search_query"),
}

internal enum class MediaRoute(val wireName: String) {
    PLAYBACK_REQUESTED("playback_requested"),
    OPENED_APP_SEARCH("opened_app_search"),
    OPENED_WEB_SEARCH("opened_web_search"),
}

/**
 * Pure description of one launch attempt. The Android gateway turns it into an
 * Intent (Uri.Builder does the percent-encoding); tests inspect it directly.
 */
internal sealed interface MediaLaunchCandidate {
    val app: AllowedMediaApp?
    val query: String
    val route: MediaRoute

    /** MediaStore.INTENT_ACTION_MEDIA_PLAY_FROM_SEARCH scoped to [app]. */
    data class PlayFromSearch(
        override val app: AllowedMediaApp,
        override val query: String,
    ) : MediaLaunchCandidate {
        override val route: MediaRoute get() = MediaRoute.PLAYBACK_REQUESTED
    }

    /** HTTPS VIEW of a fixed search page; package-scoped when [app] is non-null. */
    data class HttpsSearch(
        override val app: AllowedMediaApp?,
        val site: MediaSearchSite,
        override val query: String,
    ) : MediaLaunchCandidate {
        override val route: MediaRoute
            get() = if (app != null) MediaRoute.OPENED_APP_SEARCH else MediaRoute.OPENED_WEB_SEARCH
    }
}

internal object MediaLaunchPlanner {
    fun plan(request: MediaRequest): List<MediaLaunchCandidate> = when (request.kind) {
        MediaKind.MUSIC -> listOf(
            MediaLaunchCandidate.PlayFromSearch(AllowedMediaApp.YOUTUBE_MUSIC, request.query),
            MediaLaunchCandidate.HttpsSearch(
                AllowedMediaApp.YOUTUBE_MUSIC,
                MediaSearchSite.YOUTUBE_MUSIC,
                request.query,
            ),
            MediaLaunchCandidate.HttpsSearch(null, MediaSearchSite.YOUTUBE_MUSIC, request.query),
        )

        MediaKind.VIDEO -> listOf(
            MediaLaunchCandidate.HttpsSearch(
                AllowedMediaApp.YOUTUBE,
                MediaSearchSite.YOUTUBE,
                request.query,
            ),
            MediaLaunchCandidate.HttpsSearch(null, MediaSearchSite.YOUTUBE, request.query),
        )
    }
}

internal enum class MediaForegroundState { READY, LOCKED, UNAVAILABLE }

/**
 * Narrow, fakeable platform seam. Every method is called on the main thread
 * by [DeviceMediaToolHandler]. [canResolve] and [start] may throw. A
 * SecurityException fails the whole attempt closed (no further candidates);
 * other failures (e.g. ActivityNotFoundException) move to the next safe
 * candidate.
 */
internal interface MediaLaunchGateway {
    fun foregroundState(): MediaForegroundState
    fun canResolve(candidate: MediaLaunchCandidate): Boolean
    fun start(candidate: MediaLaunchCandidate)
    fun showHandoffToast(message: String)
}

/** [launched] is true only when an activity start actually succeeded. */
data class DeviceMediaToolResult(val output: String, val launched: Boolean)

/**
 * Weak reference to the MainActivity while it is RESUMED (attached in onResume,
 * detached in onPause). Media launches only ever start from this activity.
 */
@Singleton
class ResumedMainActivityRegistry @Inject constructor() {
    private val lock = Any()
    private var resumed: WeakReference<ComponentActivity>? = null

    fun attach(activity: ComponentActivity) {
        synchronized(lock) { resumed = WeakReference(activity) }
    }

    fun detach(activity: ComponentActivity) {
        synchronized(lock) {
            val current = resumed?.get()
            if (current == null || current === activity) resumed = null
        }
    }

    fun current(): ComponentActivity? = synchronized(lock) { resumed?.get() }
}

/** Hilt-provided executor for the device-local [PLAY_MEDIA_TOOL_NAME] tool. */
@Singleton
class DeviceMediaToolExecutor internal constructor(
    private val handler: DeviceMediaToolHandler,
) {
    @Inject
    constructor(registry: ResumedMainActivityRegistry) : this(
        DeviceMediaToolHandler(AndroidMediaLaunchGateway(registry), Dispatchers.Main.immediate),
    )

    /**
     * [isSessionCurrent] is re-evaluated on the main thread immediately before
     * every activity start, so a binding that went stale during the dispatcher
     * hop can never launch anything.
     */
    suspend fun execute(
        callId: String,
        argumentsJson: String,
        isSessionCurrent: () -> Boolean,
    ): DeviceMediaToolResult = handler.execute(callId, argumentsJson, isSessionCurrent)
}

private const val MEDIA_FOCUS_UNSTRUCTURED = "vnd.android.cursor.item/*"

/**
 * Builds the platform Intent for one allow-listed candidate. Pure
 * construction: it never resolves or starts anything.
 */
internal fun buildMediaLaunchIntent(candidate: MediaLaunchCandidate): Intent = when (candidate) {
    is MediaLaunchCandidate.PlayFromSearch ->
        Intent(MediaStore.INTENT_ACTION_MEDIA_PLAY_FROM_SEARCH)
            .setPackage(candidate.app.packageName)
            .putExtra(MediaStore.EXTRA_MEDIA_FOCUS, MEDIA_FOCUS_UNSTRUCTURED)
            .putExtra(SearchManager.QUERY, candidate.query)

    is MediaLaunchCandidate.HttpsSearch -> {
        val uri = Uri.Builder()
            .scheme("https")
            .authority(candidate.site.host)
            .path(candidate.site.path)
            .appendQueryParameter(candidate.site.queryParameter, candidate.query)
            .build()
        Intent(Intent.ACTION_VIEW, uri)
            .addCategory(Intent.CATEGORY_BROWSABLE)
            .also { intent -> candidate.app?.let { intent.setPackage(it.packageName) } }
    }
}

private class AndroidMediaLaunchGateway(
    private val registry: ResumedMainActivityRegistry,
) : MediaLaunchGateway {

    override fun foregroundState(): MediaForegroundState {
        if (Looper.myLooper() !== Looper.getMainLooper()) return MediaForegroundState.UNAVAILABLE
        val activity = registry.current() ?: return MediaForegroundState.UNAVAILABLE
        if (activity.isFinishing || activity.isDestroyed) return MediaForegroundState.UNAVAILABLE
        if (!activity.lifecycle.currentState.isAtLeast(Lifecycle.State.RESUMED)) {
            return MediaForegroundState.UNAVAILABLE
        }
        // Fail safe: an unknown keyguard state is treated as locked.
        val keyguard = activity.getSystemService(KeyguardManager::class.java)
            ?: return MediaForegroundState.LOCKED
        return if (keyguard.isKeyguardLocked) MediaForegroundState.LOCKED else MediaForegroundState.READY
    }

    @Suppress("DEPRECATION")
    override fun canResolve(candidate: MediaLaunchCandidate): Boolean {
        val activity = registry.current() ?: return false
        return activity.packageManager.resolveActivity(
            buildMediaLaunchIntent(candidate),
            PackageManager.MATCH_DEFAULT_ONLY,
        ) != null
    }

    override fun start(candidate: MediaLaunchCandidate) {
        val activity = registry.current() ?: throw IllegalStateException("no resumed activity")
        activity.startActivity(buildMediaLaunchIntent(candidate))
    }

    override fun showHandoffToast(message: String) {
        val activity = registry.current() ?: return
        Toast.makeText(activity.applicationContext, message, Toast.LENGTH_LONG).show()
    }
}

internal class DeviceMediaToolHandler(
    private val gateway: MediaLaunchGateway,
    private val mainContext: CoroutineContext,
) {
    suspend fun execute(
        callId: String,
        argumentsJson: String,
        isSessionCurrent: () -> Boolean,
    ): DeviceMediaToolResult {
        val request = try {
            PlayMediaArguments.parse(argumentsJson)
        } catch (e: InvalidMediaArguments) {
            return failure(callId, "invalid_args", e.message ?: "invalid play_media arguments")
        }
        if (!isSessionCurrent()) return failure(callId, "session_changed", SESSION_CHANGED_MESSAGE)
        val candidates = MediaLaunchPlanner.plan(request)
        return withContext(mainContext) {
            launchOnMain(callId, request, candidates, isSessionCurrent)
        }
    }

    /** Runs entirely on the main thread with no suspension points. */
    private fun launchOnMain(
        callId: String,
        request: MediaRequest,
        candidates: List<MediaLaunchCandidate>,
        isSessionCurrent: () -> Boolean,
    ): DeviceMediaToolResult {
        guard(callId, isSessionCurrent)?.let { return it }
        for (candidate in candidates) {
            val resolvable = try {
                gateway.canResolve(candidate)
            } catch (e: CancellationException) {
                throw e
            } catch (_: SecurityException) {
                // Fail closed: a permission denial is never retried against
                // another app or the browser.
                return failure(callId, "forbidden", FORBIDDEN_MESSAGE)
            } catch (_: RuntimeException) {
                false
            }
            if (!resolvable) continue

            // Re-check immediately before this start: foreground, keyguard and
            // session may all have changed since the previous candidate.
            guard(callId, isSessionCurrent)?.let { return it }

            val started = try {
                gateway.start(candidate)
                true
            } catch (e: CancellationException) {
                throw e
            } catch (_: SecurityException) {
                // Fail closed: no fallback launch after a permission denial.
                return failure(callId, "forbidden", FORBIDDEN_MESSAGE)
            } catch (_: RuntimeException) {
                // e.g. ActivityNotFoundException racing a package change.
                false
            }
            if (!started) continue

            // A successful start ends the attempt: later foreground changes
            // (our own onPause) can never trigger a second fallback launch.
            try {
                gateway.showHandoffToast(toastFor(candidate))
            } catch (e: CancellationException) {
                throw e
            } catch (_: RuntimeException) {
                // The handoff already happened; a missing toast is not a failure.
            }
            return DeviceMediaToolResult(successOutput(callId, request, candidate), launched = true)
        }
        return failure(callId, "no_handler", NO_HANDLER_MESSAGE)
    }

    private fun guard(callId: String, isSessionCurrent: () -> Boolean): DeviceMediaToolResult? {
        if (!isSessionCurrent()) return failure(callId, "session_changed", SESSION_CHANGED_MESSAGE)
        val state = try {
            gateway.foregroundState()
        } catch (e: CancellationException) {
            throw e
        } catch (_: RuntimeException) {
            MediaForegroundState.UNAVAILABLE
        }
        return when (state) {
            MediaForegroundState.READY -> null
            MediaForegroundState.LOCKED -> failure(callId, "device_locked", LOCKED_MESSAGE)
            MediaForegroundState.UNAVAILABLE -> failure(callId, "app_not_foreground", UNAVAILABLE_MESSAGE)
        }
    }

    private fun failure(callId: String, code: String, message: String) =
        DeviceMediaToolResult(mediaErrorOutput(callId, code, message), launched = false)

    private fun successOutput(
        callId: String,
        request: MediaRequest,
        candidate: MediaLaunchCandidate,
    ): String {
        val appLabel = candidate.app?.displayName
        val webHost = (candidate as? MediaLaunchCandidate.HttpsSearch)?.site?.host
        return JSONObject()
            .put("tool", PLAY_MEDIA_TOOL_NAME)
            .put("callId", callId)
            .put("ok", true)
            .put(
                "output",
                JSONObject()
                    .put("app", appLabel ?: "web search ($webHost)")
                    .put("target", if (candidate.app != null) "app" else "web")
                    .put("kind", request.kind.wireName)
                    .put("query", request.query)
                    .put("status", candidate.route.wireName)
                    .put("playbackConfirmed", false)
                    .put("conversationEnding", true)
                    .put("instruction", instructionFor(candidate)),
            )
            .toString()
    }

    private fun instructionFor(candidate: MediaLaunchCandidate): String {
        val app = candidate.app?.displayName
        return when (candidate.route) {
            MediaRoute.PLAYBACK_REQUESTED ->
                "Live Ninja asked $app to search for and play the user's request. Android does not " +
                    "report whether playback actually started, so never say it is playing; say it " +
                    "was requested. The voice conversation ends now so the media can be heard."

            MediaRoute.OPENED_APP_SEARCH ->
                "Live Ninja opened a search for the request in $app. Playback was not started or " +
                    "confirmed; the user may need to pick a result. The voice conversation ends now."

            MediaRoute.OPENED_WEB_SEARCH ->
                "The app route was unavailable or did not accept the request on this device, so " +
                    "Live Ninja opened a web search link instead. It may open in a browser or an " +
                    "app. Playback was not started or confirmed; the user may need to pick a " +
                    "result. The voice conversation ends now."
        }
    }

    private fun toastFor(candidate: MediaLaunchCandidate): String {
        val app = candidate.app?.displayName
        return when (candidate.route) {
            MediaRoute.PLAYBACK_REQUESTED ->
                "Asked $app to play your request. Playback isn't guaranteed."

            MediaRoute.OPENED_APP_SEARCH ->
                "Opened $app search. Pick a result to play; playback isn't guaranteed."

            MediaRoute.OPENED_WEB_SEARCH ->
                "Opened a web search. Pick a result to play; playback isn't guaranteed."
        }
    }

    private companion object {
        const val LOCKED_MESSAGE =
            "Media can't be started while the phone is locked. Unlock your phone, open Live " +
                "Ninja, then ask again."
        const val UNAVAILABLE_MESSAGE =
            "Live Ninja must be open on screen to start media. Unlock your phone, open Live " +
                "Ninja, then ask again."
        const val SESSION_CHANGED_MESSAGE =
            "The conversation ended before media could be opened."
        const val NO_HANDLER_MESSAGE =
            "Neither the YouTube apps nor a web browser on this device could open that search."
        const val FORBIDDEN_MESSAGE =
            "Android blocked opening a media app or browser for that search."
    }
}

private fun mediaErrorOutput(callId: String, code: String, message: String): String =
    JSONObject()
        .put("tool", PLAY_MEDIA_TOOL_NAME)
        .put("callId", callId)
        .put("ok", false)
        .put("error", JSONObject().put("code", code).put("message", message))
        .toString()

/** Result for a coordinator constructed without the injected executor. */
internal fun playMediaUnavailableOutput(callId: String): String =
    mediaErrorOutput(callId, "not_supported", "Media playback isn't available in this build of Live Ninja.")

/** Meaningful, honest tool-chip text for a play_media output. */
internal fun playMediaChipSummary(output: String): String = try {
    val json = JSONObject(output)
    if (json.optBoolean("ok")) {
        val out = json.optJSONObject("output")
        val app = out?.optString("app").orEmpty()
        when (out?.optString("status")) {
            MediaRoute.PLAYBACK_REQUESTED.wireName -> "Asked $app to play (not confirmed)"
            MediaRoute.OPENED_APP_SEARCH.wireName -> "Opened $app search (not confirmed)"
            MediaRoute.OPENED_WEB_SEARCH.wireName -> "Opened web search (not confirmed)"
            else -> "Media handoff (not confirmed)"
        }
    } else {
        json.optJSONObject("error")?.optString("message").orEmpty().ifEmpty { "failed" }
    }
} catch (_: JSONException) {
    "Media handoff"
}

internal class InvalidMediaArguments(message: String) : IllegalArgumentException(message)

/**
 * Strict argument contract, mirroring the backend schema:
 * `{"kind": "music"|"video", "query": string}` — both required, nothing else.
 *
 * The raw document is bounded to [MAX_ARGUMENT_BYTES] UTF-8 bytes before
 * parsing; that still admits a 500-code-point query even if every code point
 * is an escaped surrogate pair (12 bytes each, ~6 KB). The query is otherwise
 * preserved exactly (no trimming or normalisation).
 */
internal object PlayMediaArguments {
    const val MAX_ARGUMENT_BYTES = 8 * 1024
    const val MAX_QUERY_CODE_POINTS = 500
    private val ALLOWED_KEYS = setOf("kind", "query")

    fun parse(argumentsJson: String): MediaRequest {
        // UTF-8 bytes >= UTF-16 units, so the cheap length check is a sound prefilter.
        if (argumentsJson.length > MAX_ARGUMENT_BYTES || utf8Length(argumentsJson) > MAX_ARGUMENT_BYTES) {
            throw InvalidMediaArguments("arguments are too large")
        }
        val document = try {
            StrictJsonParser.parseDocument(argumentsJson)
        } catch (_: StrictJsonException) {
            throw InvalidMediaArguments("arguments must be a single well-formed JSON object")
        }
        val members = (document as? StrictJsonValue.Obj)?.members
            ?: throw InvalidMediaArguments("arguments must be a JSON object")
        if (members.keys.any { it !in ALLOWED_KEYS }) {
            throw InvalidMediaArguments("unexpected argument for tool play_media; only kind and query are allowed")
        }

        val kindValue = members["kind"] ?: throw InvalidMediaArguments("missing required argument \"kind\"")
        val kindText = (kindValue as? StrictJsonValue.Text)?.value
            ?: throw InvalidMediaArguments("kind must be a string")
        val kind = MediaKind.fromWireName(kindText)
            ?: throw InvalidMediaArguments("kind must be \"music\" or \"video\"")

        val queryValue = members["query"] ?: throw InvalidMediaArguments("missing required argument \"query\"")
        val query = (queryValue as? StrictJsonValue.Text)?.value
            ?: throw InvalidMediaArguments("query must be a string")
        validateQuery(query)
        return MediaRequest(kind, query)
    }

    private fun validateQuery(query: String) {
        if (query.isEmpty()) throw InvalidMediaArguments("query must be a non-empty string")
        if (query.isBlank()) throw InvalidMediaArguments("query must contain searchable text")
        var codePoints = 0
        var i = 0
        while (i < query.length) {
            val c = query[i]
            val codePoint: Int
            if (Character.isHighSurrogate(c)) {
                if (i + 1 >= query.length || !Character.isLowSurrogate(query[i + 1])) {
                    throw InvalidMediaArguments("query must be valid Unicode text")
                }
                codePoint = Character.toCodePoint(c, query[i + 1])
                i += 2
            } else if (Character.isLowSurrogate(c)) {
                throw InvalidMediaArguments("query must be valid Unicode text")
            } else {
                codePoint = c.code
                i++
            }
            if (Character.getType(codePoint) == Character.CONTROL.toInt()) {
                throw InvalidMediaArguments("query must not contain control characters")
            }
            codePoints++
            if (codePoints > MAX_QUERY_CODE_POINTS) {
                throw InvalidMediaArguments("query must be at most $MAX_QUERY_CODE_POINTS characters")
            }
        }
    }

    private fun utf8Length(text: String): Int {
        var bytes = 0
        var i = 0
        while (i < text.length) {
            val c = text[i]
            when {
                c.code < 0x80 -> bytes += 1
                c.code < 0x800 -> bytes += 2
                Character.isHighSurrogate(c) && i + 1 < text.length && Character.isLowSurrogate(text[i + 1]) -> {
                    bytes += 4
                    i++
                }
                else -> bytes += 3
            }
            i++
        }
        return bytes
    }
}

internal sealed interface StrictJsonValue {
    data class Text(val value: String) : StrictJsonValue
    data class Number(val raw: String) : StrictJsonValue
    data class Bool(val value: Boolean) : StrictJsonValue
    object Null : StrictJsonValue
    data class Obj(val members: Map<String, StrictJsonValue>) : StrictJsonValue
    data class Arr(val items: List<StrictJsonValue>) : StrictJsonValue
}

internal class StrictJsonException(message: String) : Exception(message)

/**
 * Minimal RFC 8259 parser: one value per document, only JSON whitespace
 * around it, no trailing data, no duplicate keys, no raw control characters
 * in strings, no lenient extensions (single quotes, bare keys, comments,
 * trailing commas). Nesting is bounded to keep recursion shallow.
 */
internal class StrictJsonParser private constructor(private val text: String) {
    private var pos = 0

    companion object {
        private const val MAX_DEPTH = 16

        fun parseDocument(text: String): StrictJsonValue {
            val parser = StrictJsonParser(text)
            parser.skipWhitespace()
            val value = parser.readValue(0)
            parser.skipWhitespace()
            if (parser.pos != text.length) throw StrictJsonException("trailing data after JSON document")
            return value
        }

        /** RFC 8259 HEXDIG: ASCII 0-9, a-f, A-F only (never other Unicode digits). */
        private fun asciiHexValue(c: Char): Int = when (c) {
            in '0'..'9' -> c - '0'
            in 'a'..'f' -> c - 'a' + 10
            in 'A'..'F' -> c - 'A' + 10
            else -> -1
        }
    }

    private fun skipWhitespace() {
        while (pos < text.length) {
            val c = text[pos]
            if (c == ' ' || c == '\t' || c == '\n' || c == '\r') pos++ else return
        }
    }

    private fun readValue(depth: Int): StrictJsonValue {
        if (depth > MAX_DEPTH) throw StrictJsonException("nesting too deep")
        if (pos >= text.length) throw StrictJsonException("unexpected end of input")
        val c = text[pos]
        return when {
            c == '{' -> readObject(depth)
            c == '[' -> readArray(depth)
            c == '"' -> StrictJsonValue.Text(readString())
            c == 't' -> readLiteral("true", StrictJsonValue.Bool(true))
            c == 'f' -> readLiteral("false", StrictJsonValue.Bool(false))
            c == 'n' -> readLiteral("null", StrictJsonValue.Null)
            c == '-' || c in '0'..'9' -> readNumber()
            else -> throw StrictJsonException("unexpected character")
        }
    }

    private fun readObject(depth: Int): StrictJsonValue.Obj {
        pos++ // '{'
        val members = LinkedHashMap<String, StrictJsonValue>()
        skipWhitespace()
        if (pos < text.length && text[pos] == '}') {
            pos++
            return StrictJsonValue.Obj(members)
        }
        while (true) {
            skipWhitespace()
            if (pos >= text.length || text[pos] != '"') throw StrictJsonException("expected object key")
            val key = readString()
            skipWhitespace()
            expect(':')
            skipWhitespace()
            val value = readValue(depth + 1)
            if (members.put(key, value) != null) throw StrictJsonException("duplicate key")
            skipWhitespace()
            if (pos >= text.length) throw StrictJsonException("unterminated object")
            when (text[pos]) {
                ',' -> pos++
                '}' -> {
                    pos++
                    return StrictJsonValue.Obj(members)
                }
                else -> throw StrictJsonException("expected , or }")
            }
        }
    }

    private fun readArray(depth: Int): StrictJsonValue.Arr {
        pos++ // '['
        val items = ArrayList<StrictJsonValue>()
        skipWhitespace()
        if (pos < text.length && text[pos] == ']') {
            pos++
            return StrictJsonValue.Arr(items)
        }
        while (true) {
            skipWhitespace()
            items += readValue(depth + 1)
            skipWhitespace()
            if (pos >= text.length) throw StrictJsonException("unterminated array")
            when (text[pos]) {
                ',' -> pos++
                ']' -> {
                    pos++
                    return StrictJsonValue.Arr(items)
                }
                else -> throw StrictJsonException("expected , or ]")
            }
        }
    }

    private fun readString(): String {
        pos++ // opening quote
        val out = StringBuilder()
        while (true) {
            if (pos >= text.length) throw StrictJsonException("unterminated string")
            val c = text[pos++]
            when {
                c == '"' -> return out.toString()
                c == '\\' -> readEscape(out)
                c.code < 0x20 -> throw StrictJsonException("raw control character in string")
                else -> out.append(c)
            }
        }
    }

    private fun readEscape(out: StringBuilder) {
        if (pos >= text.length) throw StrictJsonException("unterminated escape")
        when (val e = text[pos++]) {
            '"', '\\', '/' -> out.append(e)
            'b' -> out.append('\b')
            'f' -> out.append('\u000C')
            'n' -> out.append('\n')
            'r' -> out.append('\r')
            't' -> out.append('\t')
            'u' -> {
                if (pos + 4 > text.length) throw StrictJsonException("short unicode escape")
                var code = 0
                repeat(4) {
                    val digit = asciiHexValue(text[pos++])
                    if (digit < 0) throw StrictJsonException("bad unicode escape")
                    code = code * 16 + digit
                }
                out.append(code.toChar())
            }
            else -> throw StrictJsonException("bad escape")
        }
    }

    private fun readNumber(): StrictJsonValue.Number {
        val start = pos
        if (text[pos] == '-') pos++
        if (pos < text.length && text[pos] == '0') {
            pos++
        } else if (pos < text.length && text[pos] in '1'..'9') {
            while (pos < text.length && text[pos] in '0'..'9') pos++
        } else {
            throw StrictJsonException("bad number")
        }
        if (pos < text.length && text[pos] == '.') {
            pos++
            requireDigits()
        }
        if (pos < text.length && (text[pos] == 'e' || text[pos] == 'E')) {
            pos++
            if (pos < text.length && (text[pos] == '+' || text[pos] == '-')) pos++
            requireDigits()
        }
        return StrictJsonValue.Number(text.substring(start, pos))
    }

    private fun requireDigits() {
        val start = pos
        while (pos < text.length && text[pos] in '0'..'9') pos++
        if (pos == start) throw StrictJsonException("bad number")
    }

    private fun readLiteral(word: String, value: StrictJsonValue): StrictJsonValue {
        if (!text.startsWith(word, pos)) throw StrictJsonException("bad literal")
        pos += word.length
        return value
    }

    private fun expect(c: Char) {
        if (pos >= text.length || text[pos] != c) throw StrictJsonException("expected $c")
        pos++
    }
}
