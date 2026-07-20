package ninja.jeremy.liveninja.wake

import android.content.Context
import dagger.hilt.android.qualifiers.ApplicationContext
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
import java.io.File
import java.io.IOException
import java.security.MessageDigest
import java.util.Locale
import java.util.Optional
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.config.BackendConfig
import okhttp3.OkHttpClient
import okhttp3.Request
import org.json.JSONObject

/**
 * Where the active wake model's bytes live.
 */
sealed interface WakeModelRef {
    /** Wake-word catalog id this model implements (settings.schema.json `wakeWord`). */
    val wakeWordId: String

    /** Packaged fallback shipped in src/main/assets (offline out-of-box). */
    data class Asset(override val wakeWordId: String, val assetPath: String) : WakeModelRef

    /** Backend-distributed model, SHA-256 verified, stored under filesDir. */
    data class Downloaded(override val wakeWordId: String, val file: File, val sha256: String) : WakeModelRef
}

sealed interface ModelSyncResult {
    /** Model already current, or freshly downloaded+verified and now active. */
    data class Active(val ref: WakeModelRef.Downloaded) : ModelSyncResult
    /** No auth available (signed out / auth module not wired) — packaged/cached model serves. */
    data object NoAuth : ModelSyncResult
    /** Manifest said the asset isn't for our runtime (engine/format mismatch); kept previous. */
    data class UnsupportedFormat(val engine: String, val format: String) : ModelSyncResult
    /** Download hash != manifest sha256; download discarded, kept previous. */
    data object VerifyFailed : ModelSyncResult
    /** Transport/HTTP failure; kept previous. */
    data class Failed(val message: String) : ModelSyncResult
}

/**
 * Fetches, SHA-256-verifies, stores, and hot-swaps wake-word models per
 * contracts/wakeword-manifest.md, with the packaged "hey jarvis" openWakeWord v0.1 trio in
 * assets as the offline out-of-box default.
 *
 * Layout on disk: `filesDir/wakeword/<engine>/<sha256>.<ext>` plus `active_<engine>.json`
 * recording the active model's id/sha/format. Downloads land in a `.part` temp file, are
 * hashed while streaming, and only atomically renamed into place when the hash matches the
 * manifest — a torn download can never become the active model. The old model stays active
 * until the new one is fully verified (contract's "no listening gap" rule); [headModel]
 * emitting is what triggers [OpenWakeWordEngine]'s live session swap.
 *
 * The melspectrogram + embedding feature models are phrase-independent, so only the phrase
 * *head* model is ever distributed/swapped; the feature models always load from assets.
 */
@Singleton
class ModelManager @Inject constructor(
    @ApplicationContext private val context: Context,
    private val http: OkHttpClient,
    private val tokenProvider: Optional<WakeTokenProvider>,
) {
    companion object {
        private const val TAG = "WakeModelManager"

        /** Phrase-independent oWW feature models, always from assets. */
        const val ASSET_MELSPECTROGRAM = "wakeword/melspectrogram.onnx"
        const val ASSET_EMBEDDING = "wakeword/embedding_model.onnx"

        /** Packaged default head model: public openWakeWord v0.1 "hey jarvis". */
        const val ASSET_DEFAULT_HEAD = "wakeword/hey_jarvis_v0.1.onnx"
        const val DEFAULT_ASSET_WAKE_WORD_ID = "hey-jarvis"

        /** Manifest `format` tags this client's runtimes understand (additive contract). */
        const val FORMAT_OWW_ONNX_ANDROID_V1 = "oww-onnx-android-v1"
        const val FORMAT_PPN_ANDROID_V1 = "ppn-android-v1"

        private val UNDERSTOOD_FORMATS = mapOf(
            WakePreferences.ENGINE_OPENWAKEWORD to setOf(FORMAT_OWW_ONNX_ANDROID_V1),
            WakePreferences.ENGINE_PORCUPINE to setOf(FORMAT_PPN_ANDROID_V1),
        )
        private val FORMAT_EXTENSIONS = mapOf(
            FORMAT_OWW_ONNX_ANDROID_V1 to "onnx",
            FORMAT_PPN_ANDROID_V1 to "ppn",
        )
    }

    private val baseDir = File(context.filesDir, "wakeword")
    private val syncMutex = Mutex()

    private val _headModel = MutableStateFlow(loadActive(WakePreferences.ENGINE_OPENWAKEWORD)
        ?: WakeModelRef.Asset(DEFAULT_ASSET_WAKE_WORD_ID, ASSET_DEFAULT_HEAD))

    /**
     * The currently active openWakeWord phrase-head model. Engines collect this while running
     * and atomically swap their inference session when it changes.
     */
    val headModel: StateFlow<WakeModelRef> = _headModel

    /** Active model for an arbitrary engine (Porcupine build consumes this). Null = none yet. */
    fun activeModel(engine: String): WakeModelRef? =
        if (engine == WakePreferences.ENGINE_OPENWAKEWORD) _headModel.value else loadActive(engine)

    /**
     * Sync the model for [wakeWordId]/[engine] from the backend manifest
     * (`GET /v1/wakeword/{id}/model?platform=android`, Session JWT).
     *
     * Contract sequence (wakeword-manifest.md §"Client verification + hot-swap"): fetch
     * manifest → sizeBytes pre-check → stream download hashing SHA-256 → compare → atomic
     * swap; on mismatch or unknown `format`, keep the previous model and report.
     */
    suspend fun sync(
        wakeWordId: String,
        engine: String = WakePreferences.ENGINE_OPENWAKEWORD,
    ): ModelSyncResult = withContext(Dispatchers.IO) {
        syncMutex.withLock {
            val token = tokenProvider.orElse(null)?.accessToken()
                ?: return@withLock ModelSyncResult.NoAuth

            val manifest = try {
                fetchManifest(wakeWordId, token)
            } catch (e: IOException) {
                LNLog.w(LogCategory.WAKE, TAG, "manifest fetch failed for $wakeWordId", e)
                return@withLock ModelSyncResult.Failed(e.message ?: "manifest fetch failed")
            } ?: return@withLock ModelSyncResult.Failed("manifest unavailable (training/404)")

            if (manifest.engine != engine ||
                UNDERSTOOD_FORMATS[engine]?.contains(manifest.format) != true
            ) {
                // Contract: unsupported format -> do not swap, keep previous, report.
                LNLog.w(
                    LogCategory.WAKE,
                    TAG,
                    "telemetry wakeword_model_unsupported_format id=$wakeWordId " +
                        "engine=${manifest.engine} format=${manifest.format}",
                )
                return@withLock ModelSyncResult.UnsupportedFormat(manifest.engine, manifest.format)
            }

            // Already have these exact bytes active? Nothing to do.
            val active = loadActive(engine)
            if (active is WakeModelRef.Downloaded &&
                active.sha256 == manifest.sha256 && active.file.exists()
            ) {
                return@withLock ModelSyncResult.Active(active)
            }

            val ext = FORMAT_EXTENSIONS.getValue(manifest.format)
            val engineDir = File(baseDir, engine).apply { mkdirs() }
            val target = File(engineDir, "${manifest.sha256}.$ext")

            if (!target.exists()) {
                val part = File(engineDir, "${manifest.sha256}.$ext.part")
                val computed = try {
                    downloadHashing(manifest.url, manifest.sizeBytes, part)
                } catch (e: IOException) {
                    part.delete()
                    LNLog.w(LogCategory.WAKE, TAG, "model download failed for $wakeWordId", e)
                    return@withLock ModelSyncResult.Failed(e.message ?: "download failed")
                }
                if (!computed.equals(manifest.sha256, ignoreCase = true)) {
                    part.delete()
                    LNLog.w(
                        LogCategory.WAKE,
                        TAG,
                        "telemetry wakeword_model_verify_failed id=$wakeWordId " +
                            "expected=${manifest.sha256} got=$computed",
                    )
                    return@withLock ModelSyncResult.VerifyFailed
                }
                if (!part.renameTo(target)) {
                    part.delete()
                    return@withLock ModelSyncResult.Failed("rename into place failed")
                }
            }

            val ref = WakeModelRef.Downloaded(wakeWordId, target, manifest.sha256.lowercase(Locale.US))
            storeActive(engine, ref, manifest.format)
            if (engine == WakePreferences.ENGINE_OPENWAKEWORD) _headModel.value = ref
            pruneStale(engineDir, keep = target.name)
            LNLog.i(LogCategory.WAKE, TAG, "wake model active: $wakeWordId sha=${manifest.sha256.take(12)} ($engine)")
            ModelSyncResult.Active(ref)
        }
    }

    // ---- manifest ----

    private data class Manifest(
        val id: String,
        val engine: String,
        val format: String,
        val url: String,
        val sha256: String,
        val sizeBytes: Long,
    )

    private fun fetchManifest(wakeWordId: String, token: String): Manifest? {
        val request = Request.Builder()
            .url("${BackendConfig.BASE_URL}/v1/wakeword/$wakeWordId/model?platform=android")
            .header("Authorization", "Bearer $token")
            .get()
            .build()
        http.newCall(request).execute().use { resp ->
            if (resp.code == 404 || resp.code == 409) return null // not ready / training
            if (!resp.isSuccessful) throw IOException("manifest HTTP ${resp.code}")
            val body = resp.body?.string() ?: throw IOException("empty manifest body")
            val json = JSONObject(body)
            return Manifest(
                id = json.getString("id"),
                engine = json.getString("engine"),
                format = json.getString("format"),
                url = json.getString("url"),
                sha256 = json.getString("sha256").lowercase(Locale.US),
                sizeBytes = json.optLong("sizeBytes", -1L),
            )
        }
    }

    /** Stream [url] into [dest], returning the SHA-256 hex of the exact bytes written. */
    private fun downloadHashing(url: String, expectedSize: Long, dest: File): String {
        val request = Request.Builder().url(url).get().build()
        http.newCall(request).execute().use { resp ->
            if (!resp.isSuccessful) throw IOException("model HTTP ${resp.code}")
            val body = resp.body ?: throw IOException("empty model body")
            // Cheap pre-check before hashing the world (contract sizeBytes semantics).
            val contentLength = body.contentLength()
            if (expectedSize > 0 && contentLength > 0 && contentLength != expectedSize) {
                throw IOException("size mismatch: manifest=$expectedSize http=$contentLength")
            }
            val digest = MessageDigest.getInstance("SHA-256")
            var written = 0L
            dest.outputStream().use { out ->
                val buf = ByteArray(64 * 1024)
                body.byteStream().use { input ->
                    while (true) {
                        val n = input.read(buf)
                        if (n < 0) break
                        digest.update(buf, 0, n)
                        out.write(buf, 0, n)
                        written += n
                    }
                }
            }
            if (expectedSize > 0 && written != expectedSize) {
                throw IOException("size mismatch after download: manifest=$expectedSize got=$written")
            }
            return digest.digest().joinToString("") { "%02x".format(it) }
        }
    }

    // ---- active-model bookkeeping ----

    private fun stateFile(engine: String) = File(baseDir, "active_$engine.json")

    private fun loadActive(engine: String): WakeModelRef.Downloaded? {
        val f = stateFile(engine)
        if (!f.exists()) return null
        return try {
            val json = JSONObject(f.readText())
            val file = File(json.getString("file"))
            if (!file.exists()) null
            else WakeModelRef.Downloaded(
                wakeWordId = json.getString("id"),
                file = file,
                sha256 = json.getString("sha256"),
            )
        } catch (e: Exception) {
            LNLog.w(LogCategory.WAKE, TAG, "corrupt active-model state for $engine; falling back", e)
            null
        }
    }

    private fun storeActive(engine: String, ref: WakeModelRef.Downloaded, format: String) {
        baseDir.mkdirs()
        val json = JSONObject()
            .put("id", ref.wakeWordId)
            .put("sha256", ref.sha256)
            .put("format", format)
            .put("file", ref.file.absolutePath)
        val tmp = File(baseDir, "active_$engine.json.tmp")
        tmp.writeText(json.toString())
        if (!tmp.renameTo(stateFile(engine))) {
            stateFile(engine).delete()
            tmp.renameTo(stateFile(engine))
        }
    }

    /** Keep only the active model file; old swapped-out models are dead weight. */
    private fun pruneStale(engineDir: File, keep: String) {
        engineDir.listFiles()?.forEach { f ->
            if (f.name != keep && !f.name.endsWith(".part")) f.delete()
        }
    }

    /** Open a packaged asset's bytes (feature models + default head). */
    fun readAsset(path: String): ByteArray =
        context.assets.open(path).use { it.readBytes() }
}
