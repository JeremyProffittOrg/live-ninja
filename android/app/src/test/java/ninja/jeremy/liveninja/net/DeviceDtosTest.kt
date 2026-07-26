package ninja.jeremy.liveninja.net

import kotlinx.serialization.decodeFromString
import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Test

class DeviceDtosTest {

    private val json = Json { ignoreUnknownKeys = true }

    @Test
    fun canonicalDevicesEnvelopeAndRfc3339LastSeenDecode() {
        val response = json.decodeFromString<DeviceListResponse>(
            """
            {
              "devices": [{
                "deviceId": "18c93d91-579a-4dc9-9f13-62e847d981dc",
                "name": "Kitchen tablet",
                "surface": "android",
                "createdAt": 1785070800,
                "lastSeenAt": "2026-07-26T13:00:00Z"
              }]
            }
            """.trimIndent(),
        )

        assertEquals(1, response.resolvedItems.size)
        assertEquals("Kitchen tablet", response.resolvedItems.single().displayName)
        assertEquals("2026-07-26T13:00:00Z", response.resolvedItems.single().lastSeenAt)
    }

    @Test
    fun registerAndRenameShareDeviceEnvelope() {
        val response = json.decodeFromString<DeviceRegistrationResponse>(
            """
            {
              "device": {
                "deviceId": "18c93d91-579a-4dc9-9f13-62e847d981dc",
                "name": "Renamed tablet",
                "surface": "android",
                "lastSeenAt": "2026-07-26T13:00:00Z"
              }
            }
            """.trimIndent(),
        )

        assertEquals("Renamed tablet", response.device.name)
    }

    @Test
    fun sectionEnvelopeCarriesDisplayMetadataAndCapabilities() {
        val response = json.decodeFromString<SettingsSectionEnvelope>(
            """
            {
              "section": "privacy",
              "version": 8,
              "devices": [{
                "deviceId": "18c93d91-579a-4dc9-9f13-62e847d981dc",
                "name": "Kitchen tablet",
                "surface": "android",
                "metadata": {"manufacturer":"Samsung","model":"SM-X510"},
                "capabilities": ["privacy","microphone"],
                "settings": {"privacy":{"storeAudio":false}}
              }]
            }
            """.trimIndent(),
        )

        val device = response.devices.single()
        assertEquals("Samsung", device.metadata["manufacturer"]?.toString()?.trim('"'))
        assertEquals(listOf("privacy", "microphone"), device.capabilities)
    }
}
