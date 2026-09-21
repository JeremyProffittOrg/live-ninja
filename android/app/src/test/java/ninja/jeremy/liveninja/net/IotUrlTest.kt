package ninja.jeremy.liveninja.net

import java.net.URLEncoder
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Test

class IotUrlTest {

    @Test
    fun signatureStaysOffTheUrlUntilSigningIsRequired() {
        val url = iotSocketUrl(
            "example.iot.us-east-1.amazonaws.com",
            "live-ninja-iot",
            "abc+def/ghi=",
            false,
        )
        assertEquals(
            "wss://example.iot.us-east-1.amazonaws.com/mqtt?x-amz-customauthorizer-name=live-ninja-iot",
            url,
        )
        assertFalse(url.contains("signature"))
    }

    @Test
    fun signatureIsQueryEncodedWhenRequired() {
        val raw = "abc+def/ghi="
        val url = iotSocketUrl(
            "example.iot.us-east-1.amazonaws.com",
            "live-ninja-iot",
            raw,
            true,
        )
        val enc = URLEncoder.encode(raw, Charsets.UTF_8.name())
        assertEquals(
            "wss://example.iot.us-east-1.amazonaws.com/mqtt?x-amz-customauthorizer-name=live-ninja-iot&x-amz-customauthorizer-signature=$enc",
            url,
        )
    }
}
