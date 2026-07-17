package ninja.jeremy.liveninja.realtime

import java.io.IOException
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.config.BackendConfig
import ninja.jeremy.liveninja.net.AuthorizedClient
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONException
import org.json.JSONObject

/**
 * Routes realtime `function_call`s to the backend tool router
 * (`POST /api/v1/tools/invoke`, internal/webapp/api_routes.go
 * handleToolsInvoke) and returns the JSON string to feed back to the model
 * as the `function_call_output` item.
 *
 * Never throws: any transport/auth failure is folded into a
 * `{"ok":false,"error":{...}}` payload so the model always receives a
 * `function_call_output` and the session is never wedged waiting on a tool.
 */
@Singleton
class ToolCallRouter @Inject constructor(
    @AuthorizedClient private val httpClient: OkHttpClient,
) {

    suspend fun invoke(call: RealtimeEvent.FunctionCall): String = withContext(Dispatchers.IO) {
        val args = try {
            JSONObject(call.argumentsJson)
        } catch (_: JSONException) {
            JSONObject()
        }
        val body = JSONObject()
            .put("tool", call.name)
            .put("args", args)
            .put("callId", call.callId)
            // The model never reuses a call_id, so it doubles as the
            // side-effect idempotency key (retries below reuse it).
            .put("idempotencyKey", call.callId)

        val request = Request.Builder()
            .url(BackendConfig.TOOLS_INVOKE_URL)
            .post(body.toString().toRequestBody("application/json".toMediaType()))
            .build()

        try {
            httpClient.newCall(request).execute().use { response ->
                val text = response.body?.string().orEmpty()
                // The backend's Result shape ({tool, callId, ok, output, error})
                // is already what the model should see — pass it through as-is
                // whenever it is valid JSON, success or tool-level failure alike.
                try {
                    JSONObject(text).toString()
                } catch (_: JSONException) {
                    errorOutput(call, "http_${response.code}", "tool router returned a non-JSON body")
                }
            }
        } catch (e: IOException) {
            errorOutput(call, "transport_error", e.message ?: "tool invoke request failed")
        }
    }

    private fun errorOutput(call: RealtimeEvent.FunctionCall, code: String, message: String): String =
        JSONObject()
            .put("tool", call.name)
            .put("callId", call.callId)
            .put("ok", false)
            .put(
                "error",
                JSONObject().put("code", code).put("message", message),
            )
            .toString()
}
