package ninja.jeremy.liveninja.update

import dagger.hilt.android.qualifiers.ApplicationContext
import android.content.Context
import java.io.File
import java.security.MessageDigest
import java.util.concurrent.TimeUnit
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.ensureActive
import kotlinx.serialization.json.Json
import okhttp3.Authenticator
import okhttp3.CookieJar
import okhttp3.OkHttpClient
import okhttp3.Request

@Singleton
class AppUpdateRepository @Inject constructor(
    @ApplicationContext private val context: Context,
    client: OkHttpClient,
    private val json: Json,
) {
    private val http = client.newBuilder()
        // Distribution is public. Never forward account auth/cookies or follow
        // an APK redirect to a different origin (even another HTTPS origin).
        .apply { interceptors().clear(); networkInterceptors().clear() }
        .authenticator(Authenticator.NONE)
        .cookieJar(CookieJar.NO_COOKIES)
        .followRedirects(false)
        .followSslRedirects(false)
        .connectTimeout(20, TimeUnit.SECONDS)
        .callTimeout(5, TimeUnit.MINUTES)
        .readTimeout(5, TimeUnit.MINUTES)
        .writeTimeout(5, TimeUnit.MINUTES)
        .build()

    suspend fun fetchLatest(): AndroidLatestDto = withContext(Dispatchers.IO) {
        val req = Request.Builder()
            .url(AndroidReleasePolicy.LATEST_URL)
            .header("Cache-Control", "no-cache")
            .get()
            .build()
        http.newCall(req).execute().use { resp ->
            if (!resp.isSuccessful) {
                error("latest release HTTP ${resp.code}")
            }
            val source = resp.body?.source() ?: error("empty latest-release body")
            require(!source.request(MAX_MANIFEST_BYTES + 1)) { "latest-release body is too large" }
            val body = source.readUtf8()
            json.decodeFromString(AndroidLatestDto.serializer(), body)
        }
    }

    /**
     * Download [release] to cache, hashing as we go. Throws if the URL is
     * untrusted, the size is wrong, or the digest does not match.
     */
    suspend fun download(release: AndroidLatestDto, onProgress: (Long) -> Unit): File =
        withContext(Dispatchers.IO) {
            AndroidReleasePolicy.metadataError(release)?.let { error(it) }
            val dir = File(context.cacheDir, UPDATE_DIR).also { it.mkdirs() }
            val dest = File(dir, "${release.sha256.lowercase()}.apk")
            // Reuse an earlier verified download of the same content-addressed
            // build (a declined or interrupted install must not cost another
            // full download on every check), but re-hash it first.
            if (dest.exists()) {
                if (sha256Of(dest).equals(release.sha256, ignoreCase = true) &&
                    dest.length() == release.sizeBytes
                ) {
                    return@withContext dest
                }
                dest.delete()
            }
            dir.listFiles()?.forEach { if (it != dest) it.delete() }
            val req = Request.Builder().url(release.url).get().build()
            try { http.newCall(req).execute().use { resp ->
                if (!resp.isSuccessful) error("APK HTTP ${resp.code}")
                val body = resp.body ?: error("empty APK body")
                require(body.contentLength() < 0 || body.contentLength() == release.sizeBytes) { "APK size mismatch" }
                val digest = MessageDigest.getInstance("SHA-256")
                dest.outputStream().use { out ->
                    val buf = ByteArray(64 * 1024)
                    var copied = 0L
                    body.byteStream().use { input ->
                        while (true) {
                            currentCoroutineContext().ensureActive()
                            val n = input.read(buf)
                            if (n <= 0) break
                            require(copied + n <= release.sizeBytes) { "APK exceeds published size" }
                            out.write(buf, 0, n)
                            digest.update(buf, 0, n)
                            copied += n
                            onProgress(copied)
                        }
                    }
                }
                val got = digest.digest().joinToString("") { "%02x".format(it) }
                if (!got.equals(release.sha256, ignoreCase = true)) {
                    dest.delete()
                    error("APK SHA-256 mismatch")
                }
                if (dest.length() != release.sizeBytes) {
                    dest.delete()
                    error("APK size mismatch")
                }
            } } catch (t: Throwable) { dest.delete(); throw t }
            dest
        }

    /** Drop cached APKs once the installed build is current. */
    fun clearCache() {
        File(context.cacheDir, UPDATE_DIR).listFiles()?.forEach { it.delete() }
    }

    private fun sha256Of(file: File): String {
        val digest = MessageDigest.getInstance("SHA-256")
        file.inputStream().use { input ->
            val buf = ByteArray(64 * 1024)
            while (true) {
                val n = input.read(buf)
                if (n <= 0) break
                digest.update(buf, 0, n)
            }
        }
        return digest.digest().joinToString("") { "%02x".format(it) }
    }

    private companion object {
        const val UPDATE_DIR = "updates"
        const val MAX_MANIFEST_BYTES = 64L * 1024
    }
}
