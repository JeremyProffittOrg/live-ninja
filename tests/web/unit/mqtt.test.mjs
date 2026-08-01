// Byte-level tests for web/static/js/mqtt.mjs (plan.md §6 WS-3 M3.1).
//
// This is a hand-rolled binary protocol, so the tests assert BYTES, not
// behaviour-through-a-mock: a codec that is subtly wrong still "works" against
// a fake and then fails against a real broker.
//
// Run: node --test tests/web/unit/
import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  encodeLength, decodeLength, encodeConnect, encodeSubscribe,
  encodePublish, encodePingreq, encodeDisconnect, decodePublish, PacketReader,
} from '../../../web/static/js/mqtt.mjs';

test('remaining length uses the MQTT 3.1.1 varint', () => {
  // The boundaries from the spec's own table.
  assert.deepEqual(encodeLength(0), [0x00]);
  assert.deepEqual(encodeLength(127), [0x7f]);
  assert.deepEqual(encodeLength(128), [0x80, 0x01]);
  assert.deepEqual(encodeLength(16383), [0xff, 0x7f]);
  assert.deepEqual(encodeLength(16384), [0x80, 0x80, 0x01]);
  assert.deepEqual(encodeLength(2097151), [0xff, 0xff, 0x7f]);
});

test('remaining length round-trips', () => {
  for (const n of [0, 1, 127, 128, 300, 16383, 16384, 2097151]) {
    const bytes = Uint8Array.from([0x30, ...encodeLength(n)]);
    assert.equal(decodeLength(bytes, 1).value, n, `n=${n}`);
  }
});

test('decodeLength asks for more data rather than guessing', () => {
  // A continuation bit with nothing after it must not decode to something.
  assert.equal(decodeLength(Uint8Array.from([0x30, 0x80]), 1), null);
});

test('CONNECT carries the protocol header and the credential', () => {
  const pkt = encodeConnect({ clientId: 'web-1', username: 'jwt-here' });
  assert.equal(pkt[0], 0x10, 'CONNECT type, flags 0');

  // Variable header: "MQTT", level 4.
  assert.deepEqual([...pkt.slice(2, 8)], [0x00, 0x04, 0x4d, 0x51, 0x54, 0x54]);
  assert.equal(pkt[8], 4, 'protocol level 3.1.1');

  const flags = pkt[9];
  assert.equal(flags & 0x80, 0x80, 'username flag set');
  assert.equal(flags & 0x02, 0x02, 'clean session');
  assert.equal(flags & 0x04, 0, 'no will unless asked');

  // The credential must actually be in the packet — this is the whole reason
  // a browser can authenticate to AWS IoT at all.
  assert.ok(Buffer.from(pkt).includes(Buffer.from('jwt-here')));
});

test('CONNECT sets the will flags when a Last Will is given', () => {
  const pkt = encodeConnect({
    clientId: 'web-1',
    username: 'jwt',
    will: { topic: 'liveninja/user/u1/presence/dev-1', payload: '', retain: true },
  });
  const flags = pkt[9];
  assert.equal(flags & 0x04, 0x04, 'will flag');
  assert.equal(flags & 0x20, 0x20, 'will retain');
  assert.ok(Buffer.from(pkt).includes(Buffer.from('presence/dev-1')));
});

test('SUBSCRIBE sets the reserved bit brokers require', () => {
  const pkt = encodeSubscribe(7, ['liveninja/user/u1/#']);
  // 0x82: type 8, flags 0010. A broker rejects the connection without it.
  assert.equal(pkt[0], 0x82);
  const body = pkt.slice(2);
  assert.equal((body[0] << 8) | body[1], 7, 'packet id');
  assert.equal(body[body.length - 1], 0, 'requested QoS 0');
});

test('PUBLISH round-trips topic and payload', () => {
  const pkt = encodePublish('liveninja/user/u1/doc', '{"type":"doc"}');
  assert.equal(pkt[0] >> 4, 3);
  const header = decodeLength(pkt, 1);
  const body = pkt.slice(1 + header.bytes);
  const got = decodePublish(body);
  assert.equal(got.topic, 'liveninja/user/u1/doc');
  assert.deepEqual(JSON.parse(got.payload), { type: 'doc' });
});

test('PUBLISH retain flag is distinct from the default', () => {
  assert.equal(encodePublish('t', 'p')[0] & 0x01, 0);
  assert.equal(encodePublish('t', 'p', { retain: true })[0] & 0x01, 1);
});

test('PINGREQ and DISCONNECT are the two-byte packets', () => {
  assert.deepEqual([...encodePingreq()], [0xc0, 0x00]);
  assert.deepEqual([...encodeDisconnect()], [0xe0, 0x00]);
});

test('PacketReader splits several packets out of ONE frame', () => {
  // A broker may coalesce. Treating a frame as a packet drops messages under
  // exactly the load you care about.
  const frame = Uint8Array.from([
    ...encodePublish('a', '1'),
    ...encodePublish('b', '2'),
    ...encodePingreq(),
  ]);
  const packets = new PacketReader().push(frame);
  assert.equal(packets.length, 3);
  assert.equal(decodePublish(packets[0].body).topic, 'a');
  assert.equal(decodePublish(packets[1].body).topic, 'b');
  assert.equal(packets[2].type, 12);
});

test('PacketReader reassembles ONE packet split across frames', () => {
  const whole = encodePublish('liveninja/user/u1/doc', 'x'.repeat(300));
  const reader = new PacketReader();
  // Split mid-body, and again mid-length-field.
  assert.deepEqual(reader.push(whole.slice(0, 1)), []);
  assert.deepEqual(reader.push(whole.slice(1, 2)), []);
  assert.deepEqual(reader.push(whole.slice(2, 50)), []);
  const out = reader.push(whole.slice(50));
  assert.equal(out.length, 1);
  assert.equal(decodePublish(out[0].body).payload.length, 300);
});

test('a payload longer than 127 bytes uses a multi-byte length', () => {
  // The classic off-by-one in a hand-rolled codec.
  const pkt = encodePublish('t', 'y'.repeat(500));
  const header = decodeLength(pkt, 1);
  assert.ok(header.bytes > 1, 'multi-byte remaining length');
  const out = new PacketReader().push(pkt);
  assert.equal(decodePublish(out[0].body).payload.length, 500);
});

test('UTF-8 survives the round trip', () => {
  const pkt = encodePublish('liveninja/user/u1/doc', '{"summary":"updated — café ✅"}');
  const out = new PacketReader().push(pkt);
  assert.equal(JSON.parse(decodePublish(out[0].body).payload).summary, 'updated — café ✅');
});
