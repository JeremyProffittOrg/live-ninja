// Tests for the pure parts of web/static/js/liveevents.mjs — the presence
// vocabulary, the turn-taking arbitration, and the message router
// (plan.md §6 WS-5 M5.1/M5.2).
//
// Three of these are regression tests for shipped bugs, not new coverage:
// the empty-payload presence clear sat below a JSON.parse that threw on it, a
// lock claim fell through to the change handler and would have made every
// device speak, and the roster was keyed on a field that is not the one the
// presence topic is built from. Each was one line, and nothing failed.
//
// Run: node --test "tests/web/unit/*.test.mjs"
import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  LOCK_TTL_MS,
  PRESENCE_STATES,
  createEventRouter,
  createSpeakingLock,
  lockWinner,
  presenceStateFor,
} from '../../../web/static/js/liveevents.mjs';

// The nine MicState values (web/static/js/mic.mjs), spelled out rather than
// imported: mic.mjs pulls in the WebRTC/realtime graph and there is no DOM
// here. A value added there without a mapping is meant to be caught by the
// last assertion in this test, not by an import.
const MIC_STATES = [
  'idle', 'requesting-mic', 'connecting', 'live-listening', 'live-thinking',
  'live-speaking', 'ending', 'denied', 'error',
];

// The lock topic sits UNDER the presence prefix on purpose. A tab left open
// across the deploy keeps running the pre-WS-5 module graph, and both old
// clients drop anything containing '/presence/' unread; the bare
// `.../speaking` it used to be fell through their change handler instead and
// made them announce an edit that never happened, on every claim, until
// reloaded. Everything the router tests below assert about ordering only means
// something while this string keeps that prefix.
const CREDS = {
  clientId: 'web-8f2c1a',
  actorDeviceId: 'dev-desk',
  presenceTopic: 'liveninja/user/u1/presence/web-8f2c1a',
  speakingTopic: 'liveninja/user/u1/presence/speaking',
  topicFilter: 'liveninja/user/u1/#',
};

test('every MicState maps to one of the five wire states', () => {
  for (const s of MIC_STATES) {
    assert.ok(PRESENCE_STATES.includes(presenceStateFor(s)), `${s} -> ${presenceStateFor(s)}`);
  }
  assert.equal(presenceStateFor('live-listening'), 'listening');
  assert.equal(presenceStateFor('live-thinking'), 'thinking');
  assert.equal(presenceStateFor('live-speaking'), 'speaking');
  assert.equal(presenceStateFor('requesting-mic'), 'connecting');
  // Everything that is not a live turn reads as idle to a peer — including the
  // failure states, which are this device's problem and not the roster's.
  for (const s of ['idle', 'ending', 'denied', 'error']) {
    assert.equal(presenceStateFor(s), 'idle', s);
  }
  // A state this build has never heard of must still be renderable.
  assert.equal(presenceStateFor('some-future-state'), 'idle');
});

test('lockWinner picks the lowest holder whatever order the claims arrived in', () => {
  const claims = [
    { holder: 'web-c', claimId: '0000000000000001' },
    { holder: 'web-a', claimId: 'ffffffffffffffff' },
    { holder: 'web-b', claimId: '0000000000000002' },
  ];
  // Every permutation, because arrival order genuinely differs per receiver
  // and a winner that depends on it is the one outcome that must never happen.
  const permutations = [
    [0, 1, 2], [0, 2, 1], [1, 0, 2], [1, 2, 0], [2, 0, 1], [2, 1, 0],
  ];
  for (const p of permutations) {
    assert.equal(lockWinner(p.map((i) => claims[i])).holder, 'web-a', p.join(''));
  }
  assert.equal(lockWinner([]), null);
});

test('lockWinner breaks a same-holder tie on the claim id', () => {
  const winner = lockWinner([
    { holder: 'web-a', claimId: 'bbbbbbbbbbbbbbbb' },
    { holder: 'web-a', claimId: 'aaaaaaaaaaaaaaaa' },
  ]);
  assert.equal(winner.claimId, 'aaaaaaaaaaaaaaaa');
});

test('an empty presence payload removes the peer', () => {
  // The regression test for the dead clear: JSON.parse('') throws, and while
  // the parse ran first every departure — Last Will AND clean exit, both of
  // which publish '' — returned from the catch before this branch was reached.
  const seen = [];
  const router = createEventRouter({ onPresence: (peers) => seen.push(new Map(peers)) });
  const topic = 'liveninja/user/u1/presence/web-peer';

  router.handleMessage(topic, JSON.stringify({ deviceId: 'web-peer', state: 'listening' }), CREDS);
  assert.equal(router.peers.size, 1);
  assert.equal(router.peers.get('web-peer').state, 'listening');

  router.handleMessage(topic, '', CREDS);
  assert.equal(router.peers.size, 0, "an empty payload is how a device says it's gone");
  assert.equal(seen.length, 2, 'the departure must reach onPresence too');
});

test('the roster is keyed by the topic segment, not by the payload', () => {
  // The topic's last segment is what the SERVER minted for that device. A peer
  // that lies in its own deviceId field must not be able to take over another
  // device's row.
  const router = createEventRouter({});
  router.handleMessage(
    'liveninja/user/u1/presence/web-real',
    JSON.stringify({ deviceId: 'web-someone-else', state: 'speaking' }),
    CREDS,
  );
  assert.deepEqual([...router.peers.keys()], ['web-real']);
});

test('a lock claim never reaches the change handler', () => {
  // The regression test for the nudge storm: the speaking topic does not
  // contain '/presence/', and a claim payload carries no actorDeviceId, so the
  // self-edit filter would have waved every claim through to onChange and made
  // every other device announce a change that never happened.
  const changes = [];
  const lock = createSpeakingLock({ publish: () => {} });
  const router = createEventRouter({ lock, onChange: (ev) => changes.push(ev) });

  router.handleMessage(
    CREDS.speakingTopic,
    JSON.stringify({ holder: 'web-peer', claimId: '00112233445566aa', ttlMs: LOCK_TTL_MS }),
    CREDS,
  );
  assert.deepEqual(changes, []);
  assert.equal(lock.heldByOther(CREDS.clientId), true, 'the claim must reach the lock instead');
});

test('the lock topic is matched before presence, not filed as a peer', () => {
  // The lock rides the presence prefix so an old tab ignores it across a
  // deploy, which means the presence branch WOULD match it on topic. Matched
  // second, a claim becomes a roster row for a device literally named
  // "speaking" and a release becomes that device leaving — and the lock, the
  // only reader that matters, never sees either.
  assert.ok(
    CREDS.speakingTopic.includes('/presence/'),
    'this ordering only earns its keep while the lock lives under the presence prefix',
  );

  const rosters = [];
  const changes = [];
  const lock = createSpeakingLock({ publish: () => {} });
  const router = createEventRouter({
    lock,
    onChange: (ev) => changes.push(ev),
    onPresence: (peers) => rosters.push(new Map(peers)),
  });

  router.handleMessage(
    CREDS.speakingTopic,
    JSON.stringify({ holder: 'web-peer', claimId: '00112233445566aa', ttlMs: LOCK_TTL_MS }),
    CREDS,
  );
  assert.equal(lock.heldByOther(CREDS.clientId), true, 'the claim belongs to the lock');
  assert.equal(router.peers.size, 0, 'a lock claim is not a device');
  assert.deepEqual(rosters, [], 'the roster must not even be told it changed');
  assert.deepEqual(changes, []);

  // And the release, which is the branch the empty-payload presence clear
  // would otherwise have taken as a departure.
  router.handleMessage(CREDS.speakingTopic, '', CREDS);
  assert.equal(lock.heldByOther(CREDS.clientId), false);
  assert.equal(router.peers.size, 0);
  assert.deepEqual(rosters, [], 'an empty lock payload is a release, not a departure');
});

test('an empty payload on the speaking topic releases the lock', () => {
  const lock = createSpeakingLock({ publish: () => {} });
  const router = createEventRouter({ lock });

  router.handleMessage(CREDS.speakingTopic, JSON.stringify({ holder: 'web-peer' }), CREDS);
  assert.equal(lock.heldByOther(CREDS.clientId), true);

  router.handleMessage(CREDS.speakingTopic, '', CREDS);
  assert.equal(lock.heldByOther(CREDS.clientId), false, 'a release frees the lock for everybody');
});

test('a real change from another device still reaches onChange', () => {
  const changes = [];
  const router = createEventRouter({ onChange: (ev) => changes.push(ev) });
  router.handleMessage(
    'liveninja/user/u1/doc',
    JSON.stringify({ type: 'doc', summary: 'edited plan.md', actorDeviceId: 'dev-tablet' }),
    CREDS,
  );
  assert.equal(changes.length, 1);

  // ...and this device's own edit still does not.
  router.handleMessage(
    'liveninja/user/u1/doc',
    JSON.stringify({ type: 'doc', summary: 'edited plan.md', actorDeviceId: CREDS.actorDeviceId }),
    CREDS,
  );
  assert.equal(changes.length, 1, 'a device must ignore its own changes');
});

test('a claim older than its ttl is treated as absent', () => {
  let clock = 1_000_000;
  const lock = createSpeakingLock({ publish: () => {}, now: () => clock });
  lock.observe({ holder: 'web-peer', claimId: 'aaaaaaaaaaaaaaaa', ttlMs: LOCK_TTL_MS });
  assert.equal(lock.heldByOther('web-self'), true);

  clock += LOCK_TTL_MS;
  // Nothing released it — the holder crashed, or its socket dropped before it
  // could. Every reader frees it on its own clock, which is the only reason
  // the fleet is not muted forever by one dead device.
  assert.equal(lock.heldByOther('web-self'), false);
});

test('a device defers to a live claim without publishing anything', async () => {
  const published = [];
  const lock = createSpeakingLock({ publish: (p) => published.push(p), settleMs: 0 });
  lock.observe({ holder: 'web-aaa', claimId: 'aaaaaaaaaaaaaaaa', ttlMs: LOCK_TTL_MS });

  assert.equal(await lock.claimTurn('web-zzz'), false);
  assert.deepEqual(published, [], 'a claim into a live lock is a packet nobody needs');
});

test('the lowest holder wins the settle window and releases when done', async () => {
  const published = [];
  const lock = createSpeakingLock({ publish: (p) => published.push(p), settleMs: 5 });

  const claiming = lock.claimTurn('web-aaa');
  // A peer with a higher client id claims inside the settle window.
  lock.observe({ holder: 'web-zzz', claimId: '0000000000000000', ttlMs: LOCK_TTL_MS });
  assert.equal(await claiming, true);
  assert.equal(published.length, 1, 'exactly one claim on the wire');
  assert.equal(JSON.parse(published[0]).holder, 'web-aaa');
  assert.equal(JSON.parse(published[0]).ttlMs, LOCK_TTL_MS);
  assert.match(JSON.parse(published[0]).claimId, /^[0-9a-f]{16}$/);

  lock.release('web-aaa');
  assert.deepEqual(published[1], '', 'the release is a zero-length payload');
});

test('a second claim while our own is settling defers instead of winning twice', async () => {
  const published = [];
  const lock = createSpeakingLock({ publish: (p) => published.push(p), settleMs: 5 });

  const first = lock.claimTurn('web-aaa');
  // A second change lands while the settle window is still open. Both claims
  // would carry the same holder, so both would win — two unprompted turns for
  // one moment.
  assert.equal(await lock.claimTurn('web-aaa'), false);
  assert.equal(await first, true);
  assert.equal(published.length, 1);

  // And nothing else may claim while this device is still speaking on it.
  assert.equal(await lock.claimTurn('web-aaa'), false);
  lock.release('web-aaa');
  assert.equal(await lock.claimTurn('web-aaa'), true, 'the next turn is claimable again');
  // Released, not abandoned: a held lock arms a 30-second auto-release timer,
  // and leaving one pending here would keep the test runner alive for it.
  lock.release('web-aaa');
});

test('a device that lost the window publishes no release', async () => {
  const published = [];
  const lock = createSpeakingLock({ publish: (p) => published.push(p), settleMs: 5 });

  const claiming = lock.claimTurn('web-zzz');
  lock.observe({ holder: 'web-aaa', claimId: '0000000000000000', ttlMs: LOCK_TTL_MS });
  assert.equal(await claiming, false);

  lock.release('web-zzz');
  assert.equal(published.length, 1, 'a loser never held the lock, so it must not free it');
});

test('a no-op release does not touch this device\'s own in-flight claim', async () => {
  const published = [];
  const lock = createSpeakingLock({ publish: (p) => published.push(p), settleMs: 5 });

  const claiming = lock.claimTurn('web-aaa');
  // conversation.mjs releases on EVERY responsedone, and realtime.mjs reports
  // one for tool-only responses too, so a release lands inside somebody else's
  // settle window as a matter of routine. It is documented as a no-op when
  // this device is not holding, and it has to be one in the registry as well:
  // deleting our own claim here loses the arbitration for a claim that is
  // already filed in every peer for the full ttl, and since we never started
  // holding, no release would ever be published to free them.
  lock.release('web-aaa');

  assert.equal(await claiming, true, 'the in-flight claim must still be able to win');
  assert.equal(published.length, 1, 'a no-op release puts nothing on the wire');
  assert.equal(lock.holding(), true);

  lock.release('web-aaa');
  assert.deepEqual(published[1], '', 'the real release still publishes');
});

test('abandoning an in-flight claim frees it instead of stranding the peers', async () => {
  const published = [];
  const lock = createSpeakingLock({ publish: (p) => published.push(p), settleMs: 20 });

  const claiming = lock.claimTurn('web-aaa');
  // The tab closes inside the settle window. The claim is already on the wire
  // and every peer has filed it, so without an explicit empty payload here
  // they all defer to a device that is gone for the whole 30-second expiry —
  // the exact wait stop() says it eliminates.
  lock.abandon('web-aaa');

  assert.equal(published.length, 2);
  assert.equal(JSON.parse(published[0]).holder, 'web-aaa');
  assert.equal(published[1], '', 'a clean stop frees the claim it published');
  assert.equal(await claiming, false, 'an abandoned claim must not come back and take the turn');
  assert.equal(lock.holding(), false);
  assert.equal(lock.heldByOther('web-zzz'), false, 'and nothing is left in the registry');
});

test('abandon frees a held lock and stays quiet when there is nothing to free', async () => {
  const published = [];
  const lock = createSpeakingLock({ publish: (p) => published.push(p), settleMs: 5 });

  // Nothing claimed, nothing held: an unconditional publish here would let any
  // device free a lock another device is holding, because a release carries no
  // holder and frees the lock for everybody.
  lock.abandon('web-aaa');
  assert.deepEqual(published, []);

  assert.equal(await lock.claimTurn('web-aaa'), true);
  lock.abandon('web-aaa');
  assert.equal(published.length, 2);
  assert.equal(published[1], '', 'a holder that stops still frees its turn');
  assert.equal(lock.holding(), false);
});
