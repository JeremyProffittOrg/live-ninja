package ninja.jeremy.liveninja.wake

import org.junit.After
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class EnergyVadTest {

    private fun chunk(amplitude: Short): ShortArray = ShortArray(1280) { amplitude }

    @After
    fun resetChargingState() {
        // Shared process-wide flag: keep tests isolated.
        EnergyVad.chargingActive = false
    }

    @Test
    fun `silence keeps the gate closed`() {
        val vad = EnergyVad(thresholdRms = 200.0, hangoverMs = 1500)
        assertFalse(vad.accept(chunk(0), 0))
        assertFalse(vad.accept(chunk(50), 80))
        assertFalse(vad.isOpen)
        assertFalse(vad.gateJustOpened)
    }

    @Test
    fun `speech-level energy opens the gate and flags the transition once`() {
        val vad = EnergyVad(thresholdRms = 200.0, hangoverMs = 1500)
        assertFalse(vad.accept(chunk(0), 0))
        assertTrue(vad.accept(chunk(2000), 80))
        assertTrue(vad.gateJustOpened)
        assertTrue(vad.accept(chunk(2000), 160))
        assertFalse(vad.gateJustOpened) // only on the closed->open edge
    }

    @Test
    fun `gate stays open through the hangover then closes`() {
        val vad = EnergyVad(thresholdRms = 200.0, hangoverMs = 1500)
        assertTrue(vad.accept(chunk(2000), 0))
        // Quiet chunks within hangover: still open (phrase tail keeps flowing to inference).
        assertTrue(vad.accept(chunk(0), 800))
        assertTrue(vad.accept(chunk(0), 1500))
        // Past hangover: closed.
        assertFalse(vad.accept(chunk(0), 1581))
        assertFalse(vad.isOpen)
    }

    @Test
    fun `reopening after a close flags gateJustOpened again`() {
        val vad = EnergyVad(thresholdRms = 200.0, hangoverMs = 1500)
        assertTrue(vad.accept(chunk(2000), 0))
        assertFalse(vad.accept(chunk(0), 2000))
        assertTrue(vad.accept(chunk(2000), 4000))
        assertTrue(vad.gateJustOpened)
    }

    @Test
    fun `charging lowers the gate so mid-level energy opens it`() {
        // A constant chunk of amplitude A has RMS == A. 150 sits between the charging
        // gate (120) and the normal gate (200): closed on battery, open while charging.
        val vad = EnergyVad(hangoverMs = 1500) // defaults to THRESHOLD_RMS_NORMAL

        EnergyVad.chargingActive = false
        assertFalse(vad.accept(chunk(150), 0))

        EnergyVad.chargingActive = true
        assertTrue(vad.accept(chunk(150), 80))
        assertTrue(vad.gateJustOpened)
    }

    @Test
    fun `charging gate still rejects true silence`() {
        val vad = EnergyVad(hangoverMs = 1500)
        EnergyVad.chargingActive = true
        assertFalse(vad.accept(chunk(50), 0)) // 50 < 120 charging gate
        assertFalse(vad.isOpen)
    }

    @Test
    fun `reset closes the gate immediately`() {
        val vad = EnergyVad(thresholdRms = 200.0, hangoverMs = 1500)
        assertTrue(vad.accept(chunk(2000), 0))
        vad.reset()
        assertFalse(vad.isOpen)
        // Next quiet chunk stays closed despite being inside the old hangover window.
        assertFalse(vad.accept(chunk(0), 100))
    }
}
