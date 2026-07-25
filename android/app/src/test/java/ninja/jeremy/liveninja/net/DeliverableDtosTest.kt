package ninja.jeremy.liveninja.net

import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Test

class DeliverableDtosTest {
    private val json = Json {
        ignoreUnknownKeys = true
        explicitNulls = false
    }

    @Test
    fun backendListShapeMapsDeliverablesAndDeliverableIdForFiles() {
        val response = json.decodeFromString<DeliverableListResponse>(
            """
            {
              "deliverables": [
                {
                  "deliverableId": "camera-deliv-1",
                  "name": "photo-20260725-183000-abcd1234.jpg",
                  "kind": "file",
                  "status": "ready",
                  "contentType": "image/jpeg",
                  "sizeBytes": 456789,
                  "createdAt": "2026-07-25T22:30:00Z"
                }
              ],
              "nextCursor": "cursor-2"
            }
            """.trimIndent(),
        )

        assertEquals("cursor-2", response.nextCursor)
        val cameraFile = response.items.single()
        assertEquals("camera-deliv-1", cameraFile.id)
        assertEquals("photo-20260725-183000-abcd1234.jpg", cameraFile.displayName)
        assertEquals("ready", cameraFile.status)
        assertEquals("image/jpeg", cameraFile.contentType)
        assertEquals(456789L, cameraFile.sizeBytes)
    }
}
