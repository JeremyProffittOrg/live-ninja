package ninja.jeremy.liveninja.ui.state

import java.io.IOException
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.config.BackendConfig
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
) {
    private val _options = MutableStateFlow(BUILT_IN)
    val options: StateFlow<List<WakeWordOption>> = _options

    private val _lastFetchFailed = MutableStateFlow(false)
    val lastFetchFailed: StateFlow<Boolean> = _lastFetchFailed

    /**
     * Refresh from the backend snapshot. Merges server entries over the
     * built-in set (server wins on id collision); built-ins are kept so the
     * default phrase is always present.
     */
    suspend fun refresh() = withContext(Dispatchers.IO) {
        val request = Request.Builder()
            .url("${BackendConfig.BASE_URL}/static/wakewords/catalog.json")
            .get()
            .build()
        try {
            httpClient.newCall(request).execute().use { response ->
                if (!response.isSuccessful) {
                    _lastFetchFailed.value = true
                    return@withContext
                }
                val body = response.body?.string() ?: run {
                    _lastFetchFailed.value = true
                    return@withContext
                }
                val fetched = parseCatalog(body)
                if (fetched.isNotEmpty()) {
                    val merged = LinkedHashMap<String, WakeWordOption>()
                    BUILT_IN.forEach { merged[it.id] = it }
                    fetched.forEach { merged[it.id] = it }
                    _options.value = merged.values.toList()
                }
                _lastFetchFailed.value = false
            }
        } catch (_: IOException) {
            _lastFetchFailed.value = true
        }
    }

    fun optionFor(id: String): WakeWordOption? = _options.value.firstOrNull { it.id == id }

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
                val label = entry.optString("label", entry.optString("phrase", id))
                val engines = entry.optJSONArray("engines")?.let { arr ->
                    (0 until arr.length()).map { arr.optString(it) }
                } ?: listOf("openwakeword")
                add(
                    WakeWordOption(
                        id = id,
                        label = label,
                        description = entry.optString("description", ""),
                        engines = engines,
                    ),
                )
            }
        }
    }.getOrElse { emptyList() }

    companion object {
        /**
         * Shipped phrase set (mirrors mockups/android/07-wakeword-manager.html's
         * pre-trained model library). `hey-live-ninja` is the platform default
         * model every client bundles (wakeword-manifest.md fallback rule).
         */
        val BUILT_IN = listOf(
            WakeWordOption(
                id = "hey-live-ninja",
                label = "“Hey Live Ninja”",
                description = "Default phrase · bundled model, always available",
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
