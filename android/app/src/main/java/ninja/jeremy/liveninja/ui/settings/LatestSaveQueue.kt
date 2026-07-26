package ninja.jeremy.liveninja.ui.settings

import java.util.concurrent.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/**
 * Debounced latest-value queue keyed by a logical settings destination.
 *
 * One worker exists per key. Values submitted during the debounce window are
 * merged, and values submitted while a save is in flight wait behind it. A
 * failed save is merged back in front of any newer value so no user mutation
 * is silently dropped; [retry] explicitly starts another attempt.
 */
internal class LatestSaveQueue<K : Any, V : Any>(
    private val scope: CoroutineScope,
    private val debounceMillis: Long,
    private val merge: (older: V, newer: V) -> V,
    private val save: suspend (K, V) -> Boolean,
    private val onDrained: (K) -> Unit = {},
) {
    private data class Worker(
        val token: Long,
        val job: Job,
    )

    private val lock = Any()
    private val pending = mutableMapOf<K, V>()
    private val active = mutableMapOf<K, V>()
    private val workers = mutableMapOf<K, Worker>()
    private var nextToken = 0L

    fun submit(key: K, value: V) {
        synchronized(lock) {
            pending[key] = pending[key]?.let { merge(it, value) } ?: value
            startWorkerLocked(key)
        }
    }

    fun retry(key: K) {
        synchronized(lock) {
            if (pending.containsKey(key)) startWorkerLocked(key)
        }
    }

    fun retryWhere(predicate: (K) -> Boolean) {
        synchronized(lock) {
            pending.keys.filter(predicate).forEach(::startWorkerLocked)
        }
    }

    /** Cancel this destination and return the latest combined unsaved intent. */
    fun discard(key: K): V? {
        val (value, worker) = synchronized(lock) {
            val inFlight = active.remove(key)
            val waiting = pending.remove(key)
            val combined = when {
                inFlight != null && waiting != null -> merge(inFlight, waiting)
                inFlight != null -> inFlight
                else -> waiting
            }
            combined to workers.remove(key)
        }
        worker?.job?.cancel()
        return value
    }

    fun hasPending(key: K): Boolean = synchronized(lock) {
        pending.containsKey(key) || workers.containsKey(key)
    }

    fun hasWaiting(key: K): Boolean = synchronized(lock) {
        pending.containsKey(key)
    }

    private fun startWorkerLocked(key: K) {
        if (workers[key]?.job?.isActive == true) return
        val token = ++nextToken
        val job = scope.launch { runWorker(key, token) }
        workers[key] = Worker(token, job)
    }

    private suspend fun runWorker(key: K, token: Long) {
        try {
            delay(debounceMillis)
            while (true) {
                val value = synchronized(lock) {
                    pending.remove(key)?.also { active[key] = it }
                        ?: run {
                            finishWorkerLocked(key, token, drained = true)
                            null
                        }
                } ?: return

                val saved = try {
                    save(key, value)
                } catch (cancelled: CancellationException) {
                    throw cancelled
                } catch (_: Exception) {
                    false
                }
                if (!saved) {
                    synchronized(lock) {
                        active.remove(key)
                        pending[key] = pending[key]?.let { newer ->
                            merge(value, newer)
                        } ?: value
                        finishWorkerLocked(key, token, drained = false)
                    }
                    return
                }

                val hasMore = synchronized(lock) {
                    active.remove(key)
                    pending.containsKey(key).also { more ->
                        if (!more) finishWorkerLocked(key, token, drained = true)
                    }
                }
                if (!hasMore) return
                delay(debounceMillis)
            }
        } finally {
            synchronized(lock) {
                if (workers[key]?.token == token && !pending.containsKey(key)) {
                    active.remove(key)
                    workers.remove(key)
                }
            }
        }
    }

    /**
     * Caller must hold [lock]. Removing the worker in the same critical
     * section that observes an empty queue prevents a submit from seeing a
     * still-active worker and leaving its value stranded.
     */
    private fun finishWorkerLocked(key: K, token: Long, drained: Boolean) {
        if (workers[key]?.token != token) return
        workers.remove(key)
        if (drained && !pending.containsKey(key)) onDrained(key)
    }
}
