/**
 * Minimal MQTT 3.1.1 client over a browser WebSocket (plan.md §6 WS-3 M3.1).
 *
 * Hand-rolled on purpose. This app has no JS bundler — modules are served as
 * plain .mjs through a stamped import map — and the page CSP forbids CDN
 * scripts, so using mqtt.js would mean committing a UMD bundle into static/js
 * and stamping it through the import map. What is actually needed here is one
 * direction of a small binary protocol: connect, subscribe, receive, ping.
 * That is this file.
 *
 * Scope, deliberately: QoS 0 only, no session resumption, no outbound QoS>0
 * bookkeeping, no topic aliasing. Inbound PUBLISH is parsed; outbound PUBLISH
 * exists solely for presence. Anything beyond that belongs to a real library,
 * and if this ever needs it, that is the moment to take the dependency.
 *
 * AWS specifics that shape the code:
 *  - IoT accepts credentials for MQTT-over-WebSocket in the CONNECT packet's
 *    user-name field, which is the only route a browser has (it cannot set
 *    WebSocket handshake headers). The access JWT goes there.
 *  - The WebSocket subprotocol must be "mqtt".
 *  - The custom authorizer is named in the query string.
 */

const PROTOCOL_NAME = 'MQTT';
const PROTOCOL_LEVEL = 4; // 3.1.1

/** Control packet types (high nibble of byte 1). */
const CONNECT = 1;
const CONNACK = 2;
const PUBLISH = 3;
const SUBSCRIBE = 8;
const SUBACK = 9;
const PINGREQ = 12;
const PINGRESP = 13;
const DISCONNECT = 14;

/** CONNACK return codes, in the words the server would use. */
const CONNACK_REASONS = [
  'accepted',
  'unacceptable protocol version',
  'client identifier rejected',
  'server unavailable',
  'bad user name or password',
  'not authorized',
];

/* ---------------------------------------------------------------- encoding */

/** MQTT "remaining length": 7 bits per byte, high bit = continuation. */
export function encodeLength(n) {
  if (n < 0) throw new RangeError('mqtt: negative length');
  const out = [];
  do {
    let byte = n % 128;
    n = Math.floor(n / 128);
    if (n > 0) byte |= 0x80;
    out.push(byte);
  } while (n > 0);
  return out;
}

/**
 * Decodes a remaining-length field.
 * @returns {{value:number, bytes:number}|null} null when more data is needed.
 */
export function decodeLength(bytes, offset) {
  let multiplier = 1;
  let value = 0;
  let i = offset;
  for (;;) {
    if (i >= bytes.length) return null; // incomplete — wait for more
    const b = bytes[i++];
    value += (b & 0x7f) * multiplier;
    if ((b & 0x80) === 0) break;
    multiplier *= 128;
    if (multiplier > 128 * 128 * 128) throw new Error('mqtt: malformed remaining length');
  }
  return { value, bytes: i - offset };
}

const utf8 = new TextEncoder();
const utf8Decode = new TextDecoder();

/** MQTT UTF-8 string: 2-byte big-endian length, then the bytes. */
function encodeString(s) {
  const body = utf8.encode(s);
  if (body.length > 0xffff) throw new RangeError('mqtt: string too long');
  return [body.length >> 8, body.length & 0xff, ...body];
}

function buildPacket(type, flags, payloadBytes) {
  return Uint8Array.from([
    (type << 4) | flags,
    ...encodeLength(payloadBytes.length),
    ...payloadBytes,
  ]);
}

/**
 * CONNECT. `username` carries the credential (the access JWT for AWS IoT);
 * `will` is the Last Will that clears presence when the socket drops without
 * a clean DISCONNECT — which is the whole reason presence works at all.
 */
export function encodeConnect({ clientId, username, password, keepAlive = 60, will }) {
  let flags = 0;
  if (username) flags |= 0x80;
  if (password) flags |= 0x40;
  flags |= 0x02; // clean session: nothing here is worth resuming
  if (will) {
    flags |= 0x04;
    if (will.retain) flags |= 0x20;
    // will QoS stays 0 (bits 3-4 clear)
  }

  const body = [
    ...encodeString(PROTOCOL_NAME),
    PROTOCOL_LEVEL,
    flags,
    keepAlive >> 8,
    keepAlive & 0xff,
    ...encodeString(clientId),
  ];
  if (will) {
    body.push(...encodeString(will.topic));
    const payload = utf8.encode(will.payload ?? '');
    body.push(payload.length >> 8, payload.length & 0xff, ...payload);
  }
  if (username) body.push(...encodeString(username));
  if (password) body.push(...encodeString(password));
  return buildPacket(CONNECT, 0, body);
}

/** SUBSCRIBE at QoS 0. */
export function encodeSubscribe(packetId, filters) {
  const body = [packetId >> 8, packetId & 0xff];
  for (const f of filters) body.push(...encodeString(f), 0);
  return buildPacket(SUBSCRIBE, 0x02, body); // bit 1 is reserved-must-be-1
}

/** PUBLISH at QoS 0 (no packet id, no acknowledgement). */
export function encodePublish(topic, payload, { retain = false } = {}) {
  const body = [...encodeString(topic), ...utf8.encode(payload)];
  return buildPacket(PUBLISH, retain ? 0x01 : 0x00, body);
}

export const encodePingreq = () => buildPacket(PINGREQ, 0, []);
export const encodeDisconnect = () => buildPacket(DISCONNECT, 0, []);

/* ---------------------------------------------------------------- decoding */

/**
 * Pulls whole packets out of a byte stream.
 *
 * A WebSocket message boundary is NOT a packet boundary: a broker may coalesce
 * several packets into one frame or split one across frames. Treating a frame
 * as a packet works in testing and then drops messages under load, so this
 * buffers and re-parses instead.
 */
export class PacketReader {
  #buf = new Uint8Array(0);

  /** @returns {Array<{type:number, flags:number, body:Uint8Array}>} */
  push(chunk) {
    const merged = new Uint8Array(this.#buf.length + chunk.length);
    merged.set(this.#buf, 0);
    merged.set(chunk, this.#buf.length);
    this.#buf = merged;

    const packets = [];
    for (;;) {
      if (this.#buf.length < 2) break;
      const header = decodeLength(this.#buf, 1);
      if (!header) break; // length field itself is incomplete
      const start = 1 + header.bytes;
      const total = start + header.value;
      if (this.#buf.length < total) break; // body incomplete
      packets.push({
        type: this.#buf[0] >> 4,
        flags: this.#buf[0] & 0x0f,
        body: this.#buf.slice(start, total),
      });
      this.#buf = this.#buf.slice(total);
    }
    return packets;
  }
}

/** Decodes a PUBLISH body into {topic, payload}. QoS 0 only. */
export function decodePublish(body) {
  const topicLen = (body[0] << 8) | body[1];
  const topic = utf8Decode.decode(body.slice(2, 2 + topicLen));
  return { topic, payload: utf8Decode.decode(body.slice(2 + topicLen)) };
}

/* ------------------------------------------------------------------ client */

/**
 * A tiny MQTT client over WebSocket.
 *
 * Not auto-reconnecting by design: the credential is a 15-minute JWT and the
 * IoT authorizer force-closes the connection after an hour, so "reconnect"
 * always means "get a fresh token first". The owner of that decision is the
 * caller, which is why this surfaces onClose and stops.
 */
export class MqttClient {
  #ws = null;
  #reader = new PacketReader();
  #packetId = 1;
  #pingTimer = null;
  #opts;

  constructor(opts) {
    this.#opts = opts; // {url, clientId, username, keepAlive, will, onMessage, onOpen, onClose, onError}
  }

  get connected() {
    return this.#ws !== null && this.#ws.readyState === WebSocket.OPEN;
  }

  connect() {
    const { url, onError } = this.#opts;
    // "mqtt" is the subprotocol AWS IoT requires on the WebSocket handshake.
    const ws = new WebSocket(url, ['mqtt']);
    ws.binaryType = 'arraybuffer';
    this.#ws = ws;

    ws.addEventListener('open', () => {
      ws.send(encodeConnect({
        clientId: this.#opts.clientId,
        username: this.#opts.username,
        keepAlive: this.#opts.keepAlive ?? 60,
        will: this.#opts.will,
      }));
    });

    ws.addEventListener('message', (e) => {
      for (const pkt of this.#reader.push(new Uint8Array(e.data))) this.#onPacket(pkt);
    });

    ws.addEventListener('error', () => onError && onError(new Error('mqtt: websocket error')));
    ws.addEventListener('close', (e) => {
      this.#stopPing();
      this.#ws = null;
      this.#opts.onClose && this.#opts.onClose(e);
    });
  }

  #onPacket(pkt) {
    switch (pkt.type) {
      case CONNACK: {
        // byte 0 is the session-present flag; byte 1 is the return code.
        const code = pkt.body[1];
        if (code !== 0) {
          const why = CONNACK_REASONS[code] || `refused (code ${code})`;
          this.#opts.onError && this.#opts.onError(new Error(`mqtt: connection ${why}`));
          this.close();
          return;
        }
        this.#startPing();
        this.#opts.onOpen && this.#opts.onOpen();
        break;
      }
      case PUBLISH: {
        const { topic, payload } = decodePublish(pkt.body);
        this.#opts.onMessage && this.#opts.onMessage(topic, payload);
        break;
      }
      case SUBACK:
      case PINGRESP:
        break; // nothing to do; absence of PINGRESP is handled by the socket
      default:
        break; // an unexpected type is not worth tearing a session down for
    }
  }

  subscribe(filters) {
    if (!this.connected) return;
    const id = this.#packetId++ & 0xffff || 1;
    this.#ws.send(encodeSubscribe(id, filters));
  }

  publish(topic, payload, opts) {
    if (!this.connected) return;
    this.#ws.send(encodePublish(topic, payload, opts));
  }

  #startPing() {
    const keepAlive = this.#opts.keepAlive ?? 60;
    this.#stopPing();
    // Half the keep-alive: the broker disconnects at 1.5x, so pinging at half
    // leaves room for one lost ping without losing the session.
    this.#pingTimer = setInterval(() => {
      if (this.connected) this.#ws.send(encodePingreq());
    }, (keepAlive / 2) * 1000);
  }

  #stopPing() {
    if (this.#pingTimer) clearInterval(this.#pingTimer);
    this.#pingTimer = null;
  }

  close() {
    this.#stopPing();
    if (this.connected) {
      // A clean DISCONNECT tells the broker NOT to fire the Last Will, which
      // is exactly right here: a deliberate close should clear presence by
      // publishing the empty retained message, not by the will firing.
      try { this.#ws.send(encodeDisconnect()); } catch { /* already gone */ }
    }
    if (this.#ws) this.#ws.close();
    this.#ws = null;
  }
}
