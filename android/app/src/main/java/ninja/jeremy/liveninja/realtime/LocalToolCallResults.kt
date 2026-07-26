package ninja.jeremy.liveninja.realtime

import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock

/**
 * Per-session single-flight and completed-result cache for device-local tools.
 *
 * Provider transports can redeliver the same function call while the first
 * camera capture is still running, or after a relative volume mutation has
 * completed. The call id is the provider's idempotency key: every delivery
 * awaits/reuses the same result, while the side effect executes exactly once.
 */
internal class LocalToolCallResults {
    data class Delivery(val output: String, val shouldRespond: Boolean)

    private val mutex = Mutex()
    private val results = HashMap<String, CompletableDeferred<String>>()

    suspend fun getOrExecute(callId: String, execute: suspend () -> String): Delivery {
        // An empty id cannot safely identify a retry. The transport should not
        // produce one, but executing normally is safer than coalescing unrelated
        // malformed calls under the same key.
        if (callId.isEmpty()) return Delivery(execute(), shouldRespond = true)

        var ownsExecution = false
        val result = mutex.withLock {
            results[callId] ?: CompletableDeferred<String>().also {
                results[callId] = it
                ownsExecution = true
            }
        }

        if (ownsExecution) {
            try {
                result.complete(execute())
            } catch (t: Throwable) {
                // Executors normally return a structured error result. If one
                // unexpectedly throws, do not permanently poison the call id:
                // all current waiters see the failure, and a later redelivery
                // can make a fresh attempt.
                mutex.withLock {
                    if (results[callId] === result) results.remove(callId)
                }
                result.completeExceptionally(t)
            }
        }

        // Exactly one delivery owns the provider response as well as the side
        // effect. Waiters and later retries reuse the result silently; sending
        // duplicate function outputs can make transports advance twice (and
        // Gemini has already consumed the call-id -> tool-name association).
        return Delivery(result.await(), shouldRespond = ownsExecution)
    }

    /** Start/end of a conversation: completed ids never cross session boundaries. */
    suspend fun reset() {
        mutex.withLock { results.clear() }
    }
}
