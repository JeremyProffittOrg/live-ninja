package ninja.jeremy.liveninja.ui.settings

import kotlinx.coroutines.delay
import kotlinx.coroutines.joinAll
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Test

class SettingsWriteCoordinatorTest {

    @Test
    fun writesAcrossSectionsObserveFreshGlobalVersionsInSequence() = runTest {
        val coordinator = SettingsWriteCoordinator()
        var globalVersion = 1
        var activeWrites = 0
        var maxActiveWrites = 0
        val observedVersions = mutableListOf<Int>()

        listOf("appearance", "privacy", "wakeWord").map {
            launch {
                coordinator.write {
                    activeWrites += 1
                    maxActiveWrites = maxOf(maxActiveWrites, activeWrites)
                    observedVersions += globalVersion
                    delay(1)
                    globalVersion += 1
                    activeWrites -= 1
                }
            }
        }.joinAll()

        assertEquals(listOf(1, 2, 3), observedVersions)
        assertEquals(4, globalVersion)
        assertEquals(1, maxActiveWrites)
    }
}
