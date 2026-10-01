package ninja.jeremy.liveninja.location

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class LocationReportThrottleTest {
    private val interval = LocationReportThrottle.MIN_INTERVAL_MS

    @Test
    fun neverReportedIsDue() {
        assertTrue(LocationReportThrottle.isDue(nowMs = 1_000, lastReportAtMs = 0))
    }

    @Test
    fun atMostOncePerFifteenMinutes() {
        val last = 50_000_000L
        assertFalse(LocationReportThrottle.isDue(last + 1, last))
        assertFalse(LocationReportThrottle.isDue(last + interval - 1, last))
        assertTrue(LocationReportThrottle.isDue(last + interval, last))
    }

    @Test
    fun clockGoingBackwardsIsDue() {
        assertTrue(LocationReportThrottle.isDue(nowMs = 1_000, lastReportAtMs = 50_000_000))
    }
}
