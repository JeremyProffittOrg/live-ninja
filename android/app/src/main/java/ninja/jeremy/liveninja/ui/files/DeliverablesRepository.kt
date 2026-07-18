package ninja.jeremy.liveninja.ui.files

import java.io.IOException
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.config.BackendConfig
import ninja.jeremy.liveninja.net.AuthorizedClient
import ninja.jeremy.liveninja.net.DeliverableListResponse
import ninja.jeremy.liveninja.net.DeliverableZipRequest
import ninja.jeremy.liveninja.net.DeliverableZipResponse
import ninja.jeremy.liveninja.net.LiveNinjaApi
import okhttp3.OkHttpClient
import okhttp3.Request
import org.json.JSONObject

/**
 * Android access to the M9 Deliverables Store (contracts/api.md). List/zip/
 * delete ride the shared Retrofit [LiveNinjaApi]; download-URL resolution is
 * hand-rolled because `GET /api/v1/deliverables/{id}/download` answers with a
 * presigned **redirect** — we must capture the `Location` instead of following
 * it, both so DownloadManager gets a bare presigned URL it can fetch with no
 * headers, and so our Bearer header is never replayed against S3 (S3 rejects
 * requests carrying both header and query-string auth).
 */
@Singleton
class DeliverablesRepository @Inject constructor(
    private val api: LiveNinjaApi,
    @AuthorizedClient authorizedClient: OkHttpClient,
) {
    /** Same auth stack (Bearer + refresh-on-401), but redirects surface as responses. */
    private val noRedirectClient: OkHttpClient = authorizedClient.newBuilder()
        .followRedirects(false)
        .followSslRedirects(false)
        .build()

    suspend fun list(cursor: String? = null): DeliverableListResponse =
        api.listDeliverables(cursor = cursor, limit = PAGE_SIZE)

    suspend fun zip(ids: List<String>): DeliverableZipResponse =
        api.zipDeliverables(DeliverableZipRequest(ids = ids))

    suspend fun delete(id: String) {
        api.deleteDeliverable(id)
    }

    /**
     * Resolve the short-lived presigned GET URL for one deliverable.
     * Accepts either the canonical 30x `Location` redirect or a 200 JSON
     * `{"url": ...}` body (both presigned-URL server conventions), so the tab
     * keeps working whichever the backend workstream finalized.
     */
    suspend fun resolveDownloadUrl(id: String): String = withContext(Dispatchers.IO) {
        val request = Request.Builder()
            .url("${BackendConfig.BASE_URL}/api/v1/deliverables/$id/download")
            .get()
            .build()
        noRedirectClient.newCall(request).execute().use { resp ->
            when {
                resp.code in 300..399 ->
                    resp.header("Location")
                        ?: throw IOException("download redirect without Location")
                resp.isSuccessful -> {
                    val body = resp.body?.string()
                        ?: throw IOException("empty download response")
                    val url = runCatching { JSONObject(body).optString("url") }
                        .getOrDefault("")
                    url.ifBlank { throw IOException("no presigned url in response") }
                }
                else -> throw IOException("download HTTP ${resp.code}")
            }
        }
    }

    companion object {
        const val PAGE_SIZE = 50
    }
}
