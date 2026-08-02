package ninja.jeremy.liveninja.realtime

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Byte-level tests for [MqttCodec] (plan.md §6 WS-4 M4.1).
 *
 * This is a hand-rolled binary protocol, so the assertions are on BYTES rather
 * than on behaviour through a mock: a codec that is subtly wrong still passes
 * against a fake and then fails against a real broker. Deliberately the same
 * cases as tests/web/unit/mqtt.test.mjs, because the two clients are ports of
 * each other and drifting silently is the failure worth preventing.
 */
class MqttCodecTest {

    @Test
    fun `remaining length uses the MQTT 3_1_1 varint`() {
        // The boundaries from the spec's own table.
        assertArrayEquals(byteArrayOf(0x00), MqttCodec.encodeLength(0))
        assertArrayEquals(byteArrayOf(0x7f), MqttCodec.encodeLength(127))
        assertArrayEquals(byteArrayOf(0x80.toByte(), 0x01), MqttCodec.encodeLength(128))
        assertArrayEquals(byteArrayOf(0xff.toByte(), 0x7f), MqttCodec.encodeLength(16383))
        assertArrayEquals(
            byteArrayOf(0x80.toByte(), 0x80.toByte(), 0x01),
            MqttCodec.encodeLength(16384),
        )
    }

    @Test
    fun `remaining length round-trips`() {
        for (n in listOf(0, 1, 127, 128, 300, 16383, 16384, 2097151)) {
            val bytes = byteArrayOf(0x30) + MqttCodec.encodeLength(n)
            assertEquals("n=$n", n, MqttCodec.decodeLength(bytes, 1)!!.value)
        }
    }

    @Test
    fun `decodeLength asks for more data rather than guessing`() {
        // A continuation bit with nothing after it must not decode to anything.
        assertNull(MqttCodec.decodeLength(byteArrayOf(0x30, 0x80.toByte()), 1))
    }

    @Test
    fun `CONNECT carries the protocol header and the credential`() {
        val pkt = MqttCodec.encodeConnect(clientId = "and-1", username = "jwt-here")
        assertEquals(0x10, pkt[0].toInt() and 0xff)

        // Variable header: "MQTT", level 4.
        assertArrayEquals(
            byteArrayOf(0x00, 0x04, 0x4d, 0x51, 0x54, 0x54),
            pkt.copyOfRange(2, 8),
        )
        assertEquals(4, pkt[8].toInt())

        val flags = pkt[9].toInt() and 0xff
        assertEquals(0x80, flags and 0x80) // username present
        assertEquals(0x02, flags and 0x02) // clean session
        assertEquals(0, flags and 0x04) // no will unless asked

        // The credential must actually be in the packet — it is the only route
        // a client has to authenticate to AWS IoT over WebSockets.
        assertTrue(String(pkt, Charsets.UTF_8).contains("jwt-here"))
    }

    @Test
    fun `CONNECT sets the will flags when a Last Will is given`() {
        val pkt = MqttCodec.encodeConnect(
            clientId = "and-1",
            username = "jwt",
            will = MqttCodec.Will("liveninja/user/u1/presence/dev-1", "", retain = true),
        )
        val flags = pkt[9].toInt() and 0xff
        assertEquals(0x04, flags and 0x04)
        assertEquals(0x20, flags and 0x20)
        assertTrue(String(pkt, Charsets.UTF_8).contains("presence/dev-1"))
    }

    @Test
    fun `SUBSCRIBE sets the reserved bit brokers require`() {
        val pkt = MqttCodec.encodeSubscribe(7, listOf("liveninja/user/u1/#"))
        // 0x82: type 8, flags 0010. Without it the broker refuses the connection.
        assertEquals(0x82, pkt[0].toInt() and 0xff)
        val header = MqttCodec.decodeLength(pkt, 1)!!
        val body = pkt.copyOfRange(1 + header.bytes, pkt.size)
        assertEquals(7, ((body[0].toInt() and 0xff) shl 8) or (body[1].toInt() and 0xff))
        assertEquals(0, body.last().toInt()) // requested QoS 0
    }

    @Test
    fun `PUBLISH round-trips topic and payload`() {
        val pkt = MqttCodec.encodePublish("liveninja/user/u1/doc", """{"type":"doc"}""")
        val out = MqttCodec.Reader().push(pkt)
        assertEquals(1, out.size)
        val pub = MqttCodec.decodePublish(out[0].body)
        assertEquals("liveninja/user/u1/doc", pub.topic)
        assertEquals("""{"type":"doc"}""", pub.payload)
    }

    @Test
    fun `PUBLISH retain flag is distinct from the default`() {
        assertEquals(0, MqttCodec.encodePublish("t", "p")[0].toInt() and 0x01)
        assertEquals(1, MqttCodec.encodePublish("t", "p", retain = true)[0].toInt() and 0x01)
    }

    @Test
    fun `PINGREQ and DISCONNECT are the two-byte packets`() {
        assertArrayEquals(byteArrayOf(0xc0.toByte(), 0x00), MqttCodec.encodePingreq())
        assertArrayEquals(byteArrayOf(0xe0.toByte(), 0x00), MqttCodec.encodeDisconnect())
    }

    @Test
    fun `Reader splits several packets out of ONE frame`() {
        // A broker may coalesce. Treating a frame as a packet drops messages
        // under exactly the load you care about.
        val frame = MqttCodec.encodePublish("a", "1") +
            MqttCodec.encodePublish("b", "2") +
            MqttCodec.encodePingreq()
        val packets = MqttCodec.Reader().push(frame)
        assertEquals(3, packets.size)
        assertEquals("a", MqttCodec.decodePublish(packets[0].body).topic)
        assertEquals("b", MqttCodec.decodePublish(packets[1].body).topic)
        assertEquals(MqttCodec.PINGREQ, packets[2].type)
    }

    @Test
    fun `Reader reassembles ONE packet split across frames`() {
        val whole = MqttCodec.encodePublish("liveninja/user/u1/doc", "x".repeat(300))
        val reader = MqttCodec.Reader()
        assertTrue(reader.push(whole.copyOfRange(0, 1)).isEmpty())
        assertTrue(reader.push(whole.copyOfRange(1, 2)).isEmpty())
        assertTrue(reader.push(whole.copyOfRange(2, 50)).isEmpty())
        val out = reader.push(whole.copyOfRange(50, whole.size))
        assertEquals(1, out.size)
        assertEquals(300, MqttCodec.decodePublish(out[0].body).payload.length)
    }

    @Test
    fun `a payload longer than 127 bytes uses a multi-byte length`() {
        // The classic off-by-one in a hand-rolled codec.
        val pkt = MqttCodec.encodePublish("t", "y".repeat(500))
        assertTrue(MqttCodec.decodeLength(pkt, 1)!!.bytes > 1)
        val out = MqttCodec.Reader().push(pkt)
        assertEquals(500, MqttCodec.decodePublish(out[0].body).payload.length)
    }

    @Test
    fun `UTF-8 survives the round trip`() {
        val summary = """{"summary":"updated — café ✅"}"""
        val out = MqttCodec.Reader().push(MqttCodec.encodePublish("liveninja/user/u1/doc", summary))
        assertEquals(summary, MqttCodec.decodePublish(out[0].body).payload)
    }
}
