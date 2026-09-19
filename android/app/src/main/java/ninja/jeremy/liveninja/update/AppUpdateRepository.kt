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
import kotlinx.serialization.json.Json
import ninja.jeremy.liveninja.config.BackendConfig
import okhttp3.OkHttpClient
import okhttp3.Request

@Singleton
class AppUpdateRepository @Inject constructor(
    @ApplicationContext private val context: Context,
    client: OkHttpClient,
    private val json: Json,
) {
    private val http = client.newBuilder()
        .readTimeout(5, TimeUnit.MINUTES)
        .writeTimeout(5, TimeUnit.MINUTES)
        .build()

    suspend fun fetchLatest(): AndroidLatestDto = withContext(Dispatchers.IO) {
        val req = Request.Builder()
            .url("${BackendConfig.BASE_URL}/v1/app/android/latest")
            .header("Cache-Control", "no-cache")
            .get()
            .build()
        http.newCall(req).execute().use { resp ->
            if (!resp.isSuccessful) {
                error("latest release HTTP ${resp.code}")
            }
            val body = resp.body?.string() ?: error("empty latest-release body")
            json.decodeFromString(AndroidLatestDto.serializer(), body)
        }
    }

    /**
     * Download [release] to cache, hashing as we go. Throws if the URL is
     * untrusted, the size is wrong, or the digest does not match.
     */
    suspend fun download(release: AndroidLatestDto, onProgress: (Long) -> Unit): File =
        withContext(Dispatchers.IO) {
            require(AndroidReleasePolicy.isTrustedApkUrl(release.url)) { "untrusted APK URL" }
            require(AndroidReleasePolicy.sha256MatchesUrl(release.url, release.sha256)) {
                "APK URL is not content-addressed"
            }
            val dir = File(context.cacheDir, "updates").also { it.mkdirs() }
            val dest = File(dir, "pending.apk")
            if (dest.exists()) dest.delete()
            val req = Request.Builder().url(release.url).get().build()
            http.newCall(req).execute().use { resp ->
                if (!resp.isSuccessful) error("APK HTTP ${resp.code}")
                val body = resp.body ?: error("empty APK body")
                val digest = MessageDigest.getInstance("SHA-256")
                dest.outputStream().use { out ->
                    val buf = ByteArray(64 * 1024)
                    var copied = 0L
                    body.byteStream().use { input ->
                        while (true) {
                            val n = input.read(buf)
                            if (n <= 0) break
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
                if (release.sizeBytes > 0 && dest.length() != release.sizeBytes) {
                    dest.delete()
                    error("APK size mismatch")
                }
            }
            dest
        }
}
