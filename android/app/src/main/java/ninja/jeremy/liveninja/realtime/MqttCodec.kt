package ninja.jeremy.liveninja.realtime

import java.io.ByteArrayOutputStream
import okio.ByteString

/**
 * Minimal MQTT 3.1.1 encoder/decoder (plan.md §6 WS-4 M4.1).
 *
 * Hand-rolled rather than taking `aws-iot-device-sdk-java-v2`, which pulls in
 * `aws-crt-android`'s native libraries. Release builds here are arm64-only and
 * this device family already has a live 16 KB page-alignment problem with the
 * native libs it ships (onnxruntime, WebRTC); adding another native dependency
 * compounds a known defect for a feature that needs one direction of a small
 * binary protocol. This is that protocol, and it is a direct port of
 * web/static/js/mqtt.mjs so the two clients cannot drift.
 *
 * Scope, deliberately: QoS 0 only, no session resumption, no outbound QoS>0
 * bookkeeping. Inbound PUBLISH is parsed; outbound PUBLISH exists for presence.
 *
 * Pure functions over bytes — no sockets here, so every rule below is testable
 * on the JVM without a broker.
 */
internal object MqttCodec {

    const val CONNECT = 1
    const val CONNACK = 2
    const val PUBLISH = 3
    const val SUBSCRIBE = 8
    const val SUBACK = 9
    const val PINGREQ = 12
    const val PINGRESP = 13
    const val DISCONNECT = 14

    private const val PROTOCOL_NAME = "MQTT"
    private const val PROTOCOL_LEVEL = 4 // 3.1.1

    /** One decoded control packet. */
    data class Packet(val type: Int, val flags: Int, val body: ByteArray) {
        // ByteArray needs these by hand; without them two equal packets compare
        // unequal and every test that uses assertEquals lies.
        override fun equals(other: Any?): Boolean =
            other is Packet && type == other.type && flags == other.flags &&
                body.contentEquals(other.body)

        override fun hashCode(): Int = (type * 31 + flags) * 31 + body.contentHashCode()
    }

    /** A decoded PUBLISH. */
    data class Publication(val topic: String, val payload: String)

    /** MQTT "remaining length": 7 bits per byte, high bit = continuation. */
    fun encodeLength(value: Int): ByteArray {
        require(value >= 0) { "mqtt: negative length" }
        val out = ByteArrayOutputStream()
        var n = value
        do {
            var byte = n % 128
            n /= 128
            if (n > 0) byte = byte or 0x80
            out.write(byte)
        } while (n > 0)
        return out.toByteArray()
    }

    /** Decoded remaining length plus how many bytes it occupied. */
    data class Length(val value: Int, val bytes: Int)

    /** Returns null when [buf] does not yet hold the whole length field. */
    fun decodeLength(buf: ByteArray, offset: Int): Length? {
        var multiplier = 1
        var value = 0
        var i = offset
        while (true) {
            if (i >= buf.size) return null
            val b = buf[i++].toInt() and 0xff
            value += (b and 0x7f) * multiplier
            if (b and 0x80 == 0) break
            multiplier *= 128
            require(multiplier <= 128 * 128 * 128) { "mqtt: malformed remaining length" }
        }
        return Length(value, i - offset)
    }

    /** MQTT UTF-8 string: 2-byte big-endian length, then the bytes. */
    private fun ByteArrayOutputStream.writeString(s: String) {
        val body = s.toByteArray(Charsets.UTF_8)
        require(body.size <= 0xffff) { "mqtt: string too long" }
        write(body.size ushr 8)
        write(body.size and 0xff)
        write(body)
    }

    private fun packet(type: Int, flags: Int, body: ByteArray): ByteArray {
        val out = ByteArrayOutputStream()
        out.write((type shl 4) or flags)
        out.write(encodeLength(body.size))
        out.write(body)
        return out.toByteArray()
    }

    /** The Last Will that clears presence when a socket dies uncleanly. */
    data class Will(val topic: String, val payload: String = "", val retain: Boolean = true)

    fun encodeConnect(
        clientId: String,
        username: String? = null,
        keepAliveSeconds: Int = 60,
        will: Will? = null,
    ): ByteArray {
        var flags = 0
        if (username != null) flags = flags or 0x80
        flags = flags or 0x02 // clean session: nothing here is worth resuming
        if (will != null) {
            flags = flags or 0x04
            if (will.retain) flags = flags or 0x20
            // will QoS stays 0
        }

        val body = ByteArrayOutputStream()
        body.writeString(PROTOCOL_NAME)
        body.write(PROTOCOL_LEVEL)
        body.write(flags)
        body.write(keepAliveSeconds ushr 8)
        body.write(keepAliveSeconds and 0xff)
        body.writeString(clientId)
        if (will != null) {
            body.writeString(will.topic)
            val payload = will.payload.toByteArray(Charsets.UTF_8)
            body.write(payload.size ushr 8)
            body.write(payload.size and 0xff)
            body.write(payload)
        }
        username?.let { body.writeString(it) }
        return packet(CONNECT, 0, body.toByteArray())
    }

    fun encodeSubscribe(packetId: Int, filters: List<String>): ByteArray {
        val body = ByteArrayOutputStream()
        body.write(packetId ushr 8)
        body.write(packetId and 0xff)
        for (f in filters) {
            body.writeString(f)
            body.write(0) // QoS 0
        }
        // Flags 0x02: bit 1 is reserved-must-be-1, and a broker rejects the
        // connection outright without it.
        return packet(SUBSCRIBE, 0x02, body.toByteArray())
    }

    fun encodePublish(topic: String, payload: String, retain: Boolean = false): ByteArray {
        val body = ByteArrayOutputStream()
        body.writeString(topic)
        body.write(payload.toByteArray(Charsets.UTF_8))
        return packet(PUBLISH, if (retain) 0x01 else 0x00, body.toByteArray())
    }

    fun encodePingreq(): ByteArray = packet(PINGREQ, 0, ByteArray(0))
    fun encodeDisconnect(): ByteArray = packet(DISCONNECT, 0, ByteArray(0))

    /** Decodes a PUBLISH body. QoS 0 only, so there is no packet id to skip. */
    fun decodePublish(body: ByteArray): Publication {
        val topicLen = ((body[0].toInt() and 0xff) shl 8) or (body[1].toInt() and 0xff)
        val topic = String(body, 2, topicLen, Charsets.UTF_8)
        val payload = String(body, 2 + topicLen, body.size - 2 - topicLen, Charsets.UTF_8)
        return Publication(topic, payload)
    }

    /**
     * Pulls whole packets out of a byte stream.
     *
     * A WebSocket message boundary is NOT a packet boundary: a broker may
     * coalesce several packets into one frame or split one across frames.
     * Treating a frame as a packet works in testing and then drops messages
     * under load, which is why this buffers and re-parses.
     */
    class Reader {
        private var buf = ByteArray(0)

        fun push(chunk: ByteString): List<Packet> = push(chunk.toByteArray())

        fun push(chunk: ByteArray): List<Packet> {
            buf = buf + chunk
            val packets = mutableListOf<Packet>()
            while (true) {
                if (buf.size < 2) break
                val header = decodeLength(buf, 1) ?: break
                val start = 1 + header.bytes
                val total = start + header.value
                if (buf.size < total) break
                packets += Packet(
                    type = (buf[0].toInt() and 0xff) ushr 4,
                    flags = buf[0].toInt() and 0x0f,
                    body = buf.copyOfRange(start, total),
                )
                buf = buf.copyOfRange(total, buf.size)
            }
            return packets
        }
    }
}
