package ninja.jeremy.liveninja.ui.settings

import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock

/**
 * One write lane for the backend's document-wide optimistic version.
 *
 * Every caller performs its fresh GET inside [write], so mutations from
 * different accordions cannot all race from the same version and exhaust the
 * bounded 409 retry.
 */
internal class SettingsWriteCoordinator {
    private val mutex = Mutex()

    suspend fun <T> write(block: suspend () -> T): T =
        mutex.withLock { block() }
}
