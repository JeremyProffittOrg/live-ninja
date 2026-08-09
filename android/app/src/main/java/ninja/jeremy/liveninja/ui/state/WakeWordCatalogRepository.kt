package ninja.jeremy.liveninja.ui.state

import java.io.IOException
import java.util.Optional
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.config.BackendConfig
import ninja.jeremy.liveninja.wake.WakeTokenProvider
import okhttp3.OkHttpClient
import okhttp3.Request
import org.json.JSONArray
import org.json.JSONObject

/** One selectable wake-word catalog entry (populates the settings combobox — FR-K02). */
data class WakeWordOption(
    val id: String,
    val label: String,
    val description: String,
    val engines: List<String>,
)

/**
 * Wake-word catalog source for pickers (settings.schema.json `wakeWord`:
 * "Always a combobox selection over the shared catalog … never a free-typed
 * phrase").
 *
 * Primary source is the static CloudFront snapshot
 * `GET /static/wakewords/catalog.json` (api.md, Public). Until the M6 catalog
 * pipeline publishes it — or whenever the device is offline — the picker is
 * populated from [BUILT_IN], the platform's shipped phrase set, so the control
 * is never an empty or free-text field.
 */
@Singleton
class WakeWordCatalogRepository @Inject constructor(
    private val httpClient: OkHttpClient,
    private val tokenProvider: Optional<WakeTokenProvider>,
) {
    private val _options = MutableStateFlow(BUILT_IN)
    val options: StateFlow<List<WakeWordOption>> = _options

    private val _lastFetchFailed = MutableStateFlow(false)
    val lastFetchFailed: StateFlow<Boolean> = _lastFetchFailed

    /**
     * Refresh the picker. Merges server entries over the built-in set (server wins on id
     * collision); built-ins are kept so the default phrase is always present.
     *
     * Prefers the **authenticated** catalog `GET /api/v1/wakeword`, because that is the only
     * source that includes the phrases this user has actually trained. The static
     * `/static/wakewords/catalog.json` snapshot says so itself — "User-trained phrases and the
     * CloudFront-regenerated catalog pipeline arrive in M6; until then this static file is the
     * whole catalog" — and reading only that file is why a trained, `ready` phrase such as
     * "hey automatica" could never be selected on Android: the picker literally did not know it
     * existed, while the settings screen offered six phrases that have no model at all.
     *
     * Falls back to the static snapshot when signed out or when the authenticated call fails, so
     * the control is never empty.
     */
    suspend fun refresh() = withContext(Dispatchers.IO) {
        val token = tokenProvider.orElse(null)?.accessToken()
        if (token != null && fetchInto(AUTHED_CATALOG_URL, token)) return@withContext
        fetchInto(STATIC_CATALOG_URL, token = null)
        Unit
    }

    /** Fetch + merge one catalog source. Returns true when options were updated. */
    private fun fetchInto(url: String, token: String?): Boolean {
        val request = Request.Builder()
            .url(url)
            .apply { if (token != null) header("Authorization", "Bearer $token") }
            .get()
            .build()
        return try {
            httpClient.newCall(request).execute().use { response ->
                if (!response.isSuccessful) {
                    _lastFetchFailed.value = true
                    return false
                }
                val body = response.body?.string() ?: run {
                    _lastFetchFailed.value = true
                    return false
                }
                val fetched = parseCatalog(body)
                if (fetched.isEmpty()) {
                    _lastFetchFailed.value = true
                    return false
                }
                val merged = LinkedHashMap<String, WakeWordOption>()
                BUILT_IN.forEach { merged[it.id] = it }
                fetched.forEach { merged[it.id] = it }
                _options.value = collapseDuplicatePhrases(merged.values.toList(), fetched)
                _lastFetchFailed.value = false
                true
            }
        } catch (_: IOException) {
            _lastFetchFailed.value = true
            false
        }
    }

    fun optionFor(id: String): WakeWordOption? = _options.value.firstOrNull { it.id == id }

    /**
     * One phrase, one row. The shipped list carries the bare slug `hey-live-ninja` while a
     * trained model of the same phrase arrives under a user-scoped id (`hey-live-ninja-47df2e`),
     * so a signed-in user with that phrase trained would otherwise see "Hey Live Ninja" twice —
     * once as a placeholder promising a model and once as the real thing.
     *
     * The trained entry wins, because it is the one whose id resolves to a model directly
     * instead of relying on the server's slug fallback.
     */
    internal fun collapseDuplicatePhrases(
        all: List<WakeWordOption>,
        fromServer: List<WakeWordOption>,
    ): List<WakeWordOption> {
        val serverIds = fromServer.mapTo(HashSet()) { it.id }
        val byPhrase = LinkedHashMap<String, WakeWordOption>()
        for (option in all) {
            val key = normalizePhrase(option.label)
            val existing = byPhrase[key]
            if (existing == null) {
                byPhrase[key] = option
                continue
            }
            // Keep whichever came from the server catalog; otherwise keep the first seen.
            if (option.id in serverIds && existing.id !in serverIds) byPhrase[key] = option
        }
        return byPhrase.values.toList()
    }

    /** `“Hey Live Ninja”` and `hey live ninja` are the same phrase. */
    private fun normalizePhrase(label: String): String =
        label.lowercase().filter { it.isLetterOrDigit() || it == ' ' }.trim().replace(WHITESPACE, " ")

    /**
     * The authenticated catalog carries no `description`, so build an honest one. Saying whether
     * a phrase is trained and ready is the difference between the picker being a menu and being
     * a list of guesses — six of the shipped entries have no model behind them at all.
     */
    private fun describe(source: String, status: String): String = when {
        source == "builtin" -> "Built in · works offline, no training needed"
        status == "ready" -> "Trained · ready to use"
        status == "training" || status == "pending" -> "Training — not ready yet"
        status == "failed" -> "Training failed"
        else -> ""
    }

    private fun parseCatalog(body: String): List<WakeWordOption> = runCatching {
        val root = JSONObject(body)
        val entries: JSONArray = root.optJSONArray("entries")
            ?: root.optJSONArray("wakewords")
            ?: JSONArray()
        buildList {
            for (i in 0 until entries.length()) {
                val entry = entries.optJSONObject(i) ?: continue
                val id = entry.optString("id")
                if (id.isBlank()) continue

                // The authenticated catalog also carries the ESP32 WakeNet builtins
                // (wn9_hiesp, wn9_hilexin, wn9_alexa), which this device can never run. An
                // entry that names its platforms and omits android is dropped rather than
                // offered and then failing at model-fetch time.
                val platforms = entry.optJSONArray("platforms")?.let { arr ->
                    (0 until arr.length()).map { arr.optString(it) }
                }
                if (platforms != null && platforms.isNotEmpty() && ANDROID !in platforms) continue

                val label = entry.optString("label", entry.optString("phrase", id))
                // Static snapshot sends `engines: [...]`; the authenticated catalog sends a
                // single `engine`. Read both rather than silently defaulting a WakeNet phrase
                // to openwakeword.
                val engines = entry.optJSONArray("engines")?.let { arr ->
                    (0 until arr.length()).map { arr.optString(it) }
                }
                    ?: entry.optString("engine").takeIf { it.isNotBlank() }?.let { listOf(it) }
                    ?: listOf("openwakeword")
                add(
                    WakeWordOption(
                        id = id,
                        label = label,
                        description = entry.optString("description", "").ifBlank {
                            describe(entry.optString("source"), entry.optString("status"))
                        },
                        engines = engines,
                    ),
                )
            }
        }
    }.getOrElse { emptyList() }

    companion object {
        private const val ANDROID = "android"
        private val WHITESPACE = Regex("\\s+")

        /** Authenticated catalog: builtins PLUS this user's own trained phrases. */
        private const val AUTHED_CATALOG_URL = "${BackendConfig.BASE_URL}/api/v1/wakeword"

        /** M3 snapshot, builtins only. Fallback for signed-out/offline. */
        private const val STATIC_CATALOG_URL =
            "${BackendConfig.BASE_URL}/static/wakewords/catalog.json"

        /**
         * Shipped phrase set (mirrors mockups/android/07-wakeword-manager.html's
         * pre-trained model library).
         *
         * `hey-jarvis` is what this build actually bundles
         * (ModelManager.ASSET_DEFAULT_HEAD); `hey-live-ninja` is the platform default
         * per wakeword-manifest.md but needs its trained model synced before the
         * detector can match it. Entries must never claim a phrase is available when
         * no model backs it — that is exactly the WS-5 M21.3 defect.
         */
        val BUILT_IN = listOf(
            // The bundled asset really is openWakeWord's public "hey jarvis" v0.1
            // (ModelManager.ASSET_DEFAULT_HEAD). It is listed first and honestly,
            // because it is the only phrase that works out of the box.
            WakeWordOption(
                id = "hey-jarvis",
                label = "“Hey Jarvis”",
                description = "Bundled model · works offline, no training needed",
                engines = listOf("openwakeword"),
            ),
            WakeWordOption(
                id = "hey-live-ninja",
                label = "“Hey Live Ninja”",
                description = "Platform default · needs a trained model synced to this device",
                engines = listOf("openwakeword"),
            ),
            WakeWordOption(
                id = "hey-ninja",
                label = "“Hey Ninja”",
                description = "Casual short form · English (US)",
                engines = listOf("openwakeword"),
            ),
            WakeWordOption(
                id = "ninja-go",
                label = "“Ninja Go”",
                description = "Two-syllable, low false-trigger rate",
                engines = listOf("openwakeword"),
            ),
            WakeWordOption(
                id = "hey-assistant-pro",
                label = "“Hey Assistant Pro”",
                description = "Formal · English (US)",
                engines = listOf("openwakeword"),
            ),
            WakeWordOption(
                id = "okay-dojo",
                label = "“Okay Dojo”",
                description = "Themed alternate · English (US)",
                engines = listOf("openwakeword"),
            ),
        )
    }
}
