/**
 * Cross-device change notifications, presence and turn-taking over MQTT
 * (plan.md §6 WS-3 M3.3/M3.4, §6 WS-5 M5.1/M5.2).
 *
 * Every device signed into this account subscribes to one user-scoped topic.
 * When one of them changes shared state — a document, a memory entity, a plan —
 * the server publishes a small notification and the others learn immediately
 * instead of on their next poll. Two client-published topics ride the same
 * subscription: a retained presence message per device (the roster), and one
 * per-account turn-taking lock (which device gets to say the change out loud).
 *
 * Three things this module is careful about, each because getting it wrong is
 * worse than not having the feature:
 *
 *  1. **It ignores its own edits.** The server stamps every event with the
 *     device that caused it, and the credential endpoint tells this client
 *     which value that will be for itself. Without that comparison the device
 *     that made a change announces that change back to the user, which reads
 *     as the assistant talking to itself.
 *  2. **It never speaks over a live turn.** An unprompted nudge while the
 *     assistant is mid-sentence is worse than a late one.
 *  3. **It fails silently.** This is a convenience layer. If IoT is
 *     unreachable, or the account has no credential, the page must behave
 *     exactly as it did before — every failure path here logs and stops.
 *
 * The pure parts — the state vocabulary, the arbitration, the message router
 * and the lock registry — are exported separately from the socket so they can
 * be tested without a broker (tests/web/unit/liveevents.test.mjs).
 */

import { MqttClient } from './mqtt.mjs';

const CREDENTIALS_PATH = '/api/v1/iot/credentials';

/** Refresh this long before the token expires, so a reconnect never races it. */
const REFRESH_MARGIN_MS = 60_000;

/** Presence is a retained message, so a device that joins sees who is already here. */
const PRESENCE_QOS_OPTS = { retain: true };

/**
 * The lock is deliberately NOT retained, which is the opposite of presence.
 * MQTT 3.1.1 has no message expiry, so a retained claim from a holder that
 * crashed before releasing would sit in the broker forever, and a device
 * connecting an hour later would arm its 30-second expiry from delivery and
 * fall silent over a lock whose holder is long gone.
 */
const LOCK_QOS_OPTS = { retain: false };

/** How long a claim binds if nothing releases it. Every reader expires it locally. */
export const LOCK_TTL_MS = 30_000;

/**
 * How long a claimer waits before deciding whether it won.
 *
 * It has to comfortably exceed a device→broker→device round trip through one
 * regional IoT endpoint (typically 30–120 ms) while staying well under the
 * ~1 s at which a spoken interjection stops feeling connected to the event
 * that caused it.
 */
export const LOCK_SETTLE_MS = 400;

/** Floor for a ttl read off the wire, so a hostile or buggy 0 cannot disable the lock. */
const LOCK_TTL_MIN_MS = 1000;

/**
 * Presence is republished on every state change, so it is throttled — but with
 * a guaranteed TRAILING publish. Presence is retained, and the last publish is
 * what a device joining later reads: dropping the trailing one would leave the
 * roster permanently showing whatever state this device passed through a
 * second ago.
 */
const PRESENCE_THROTTLE_MS = 1000;

/**
 * The five-value presence vocabulary shared with the Android client.
 *
 * Neither client's own enum goes on the wire: mic.mjs has nine states and
 * Android's has seven with no THINKING at all, so publishing either raw would
 * put one client's state machine on the wire and make the roster
 * untranslatable the first time either gained a state.
 */
export const PRESENCE_STATES = Object.freeze([
  'idle',
  'connecting',
  'listening',
  'thinking',
  'speaking',
]);

/**
 * MicState (web/static/js/mic.mjs) → the wire vocabulary above.
 *
 * The keys are spelled out rather than imported from mic.mjs on purpose: that
 * module pulls in the whole WebRTC/realtime graph, and this one is loaded by a
 * node test with no DOM. A MicState value added without a line here maps to
 * `idle`, which is the safe answer — a device wrongly shown as idle is a
 * cosmetic roster bug, whereas a raw enum value on the wire is a peer that
 * cannot render it.
 */
const MIC_STATE_TO_PRESENCE = Object.freeze({
  idle: 'idle',
  ending: 'idle',
  denied: 'idle',
  error: 'idle',
  'requesting-mic': 'connecting',
  connecting: 'connecting',
  'live-listening': 'listening',
  'live-thinking': 'thinking',
  'live-speaking': 'speaking',
});

/** @param {string} micState a MicState value @returns {string} one of PRESENCE_STATES */
export function presenceStateFor(micState) {
  return MIC_STATE_TO_PRESENCE[micState] || 'idle';
}

/**
 * Picks the winner from a set of observed claims: lexicographically smallest
 * `holder`, ties broken by smallest `claimId`.
 *
 * Deliberately NOT "first message wins". QoS 0 gives no ordering guarantee
 * across two publishers and arrival order genuinely differs per receiver, so
 * first-writer yields a DIFFERENT winner on different devices — the one
 * outcome that must never happen. A pure function of the observed payloads
 * gives every receiver the same answer whatever order the packets arrived in,
 * which is the property actually required.
 *
 * @param {Array<{holder: string, claimId?: string}>} claims
 * @returns {?{holder: string, claimId?: string}}
 */
export function lockWinner(claims) {
  let best = null;
  for (const c of claims || []) {
    if (!c || typeof c.holder !== 'string' || !c.holder) continue;
    if (!best) {
      best = c;
      continue;
    }
    if (c.holder < best.holder) best = c;
    else if (c.holder === best.holder && String(c.claimId || '') < String(best.claimId || '')) best = c;
  }
  return best;
}

function parseOrNull(raw) {
  try {
    return JSON.parse(raw);
  } catch {
    return null; // a malformed payload is not worth tearing the session down for
  }
}

/** 16 lowercase hex chars, fresh per claim — two claims are never equal. */
function newClaimId() {
  const bytes = new Uint8Array(8);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

/**
 * The turn-taking lock, as seen by one device.
 *
 * Expiry is a LOCAL duration measured from the moment a claim was received,
 * with no timestamp on the wire. An `expiresAt` epoch would only be meaningful
 * if every device agreed on "now", and a tablet with a wrong date — the exact
 * device class this repo already carries workarounds for — would publish
 * claims that are either already expired (the lock never binds) or effectively
 * permanent (one device mutes the fleet). Neither is detectable from the
 * payload. "For the next 30 seconds" is a duration, and every receiver can
 * measure a duration on its own clock without trusting anybody.
 *
 * @param {object} opts
 * @param {(payload: string) => void} opts.publish  puts a payload on the speaking topic
 * @param {() => number} [opts.now]                 injectable clock (tests)
 * @param {number} [opts.settleMs]
 */
export function createSpeakingLock({ publish, now = () => Date.now(), settleMs = LOCK_SETTLE_MS } = {}) {
  /** holder → the most recent claim seen from it. */
  const claims = new Map();
  let holding = false;
  let claiming = false;
  let holdTimer = null;
  /**
   * Which in-flight claim is the current one. Bumped every time a claim is
   * published and every time one is deliberately given up, so the half of
   * claimTurn that resumes after the settle window can tell whether the claim
   * it is about to arbitrate over is still this device's own. Without it an
   * abandoned claim's continuation would come back and take the turn anyway.
   */
  let claimEpoch = 0;

  function liveClaims() {
    for (const [holder, c] of claims) {
      if (now() - c.at >= c.ttlMs) claims.delete(holder);
    }
    return Array.from(claims.values());
  }

  /** Records a claim heard on the speaking topic — including this device's own echo. */
  function observe(claim) {
    if (!claim || typeof claim.holder !== 'string' || !claim.holder) return;
    const wanted = Number(claim.ttlMs);
    claims.set(claim.holder, {
      holder: claim.holder,
      claimId: typeof claim.claimId === 'string' ? claim.claimId : '',
      // Clamped, not trusted: the ttl arrives from another device and a wild
      // value would either disable the lock or wedge it for a very long time.
      ttlMs: Number.isFinite(wanted)
        ? Math.min(Math.max(wanted, LOCK_TTL_MIN_MS), LOCK_TTL_MS)
        : LOCK_TTL_MS,
      at: now(),
    });
  }

  /**
   * The release. It carries no holder because a zero-length payload has no
   * room for one — and none is needed: the lock is a single shared resource,
   * so "released" means free for everybody.
   */
  function clear() {
    claims.clear();
  }

  function heldByOther(self) {
    return liveClaims().some((c) => c.holder !== self);
  }

  /**
   * Claim, settle, arbitrate. Resolves true only if this device won the turn.
   *
   * This does not make the claim atomic and nothing here can: QoS 0 has no
   * acknowledgement, so a claim stalled longer than the settle window, or
   * dropped entirely by a socket that was not open, leaves its publisher
   * invisible and two devices can still speak. What it does guarantee is that
   * every device which SAW the same claims reaches the same verdict.
   */
  async function claimTurn(self) {
    // This device is already settling a claim, or already speaking on one it
    // won. A second claim would carry the same holder and therefore win too —
    // two unprompted turns for what the user experiences as one moment. The
    // caller defers instead, and the two changes merge into one sentence.
    if (claiming || holding) return false;

    // Somebody else is already holding: defer without publishing anything. A
    // claim into a live lock is a packet every peer has to arbitrate over for
    // an answer that is already known.
    if (heldByOther(self)) return false;

    const claim = { holder: self, claimId: newClaimId(), ttlMs: LOCK_TTL_MS };
    const epoch = (claimEpoch += 1);
    // Counted locally as well as published: the broker echoes our own claim
    // back through the `#` subscription, but a dropped publish would otherwise
    // leave us arbitrating over a set that does not contain us.
    observe(claim);
    publish(JSON.stringify(claim));

    claiming = true;
    try {
      await delay(settleMs);
    } finally {
      // Only if this is still the claim in flight. abandon() already reopened
      // the gate; clearing it a second time here would let a THIRD claimTurn
      // start while a second one is mid-settle, which is the double-turn the
      // guard at the top exists to stop.
      if (claimEpoch === epoch) claiming = false;
    }

    // Given up while we were settling — a clean stop, most likely. The empty
    // payload is already on the wire, so taking the turn now would speak on a
    // lock this device has told every peer it released.
    if (claimEpoch !== epoch) return false;

    const winner = lockWinner(liveClaims());
    if (!winner || winner.holder !== self) return false;

    holding = true;
    clearTimeout(holdTimer);
    // The holder's own auto-release is a courtesy. A crashed holder runs no
    // timer at all, which is exactly why every reader expires the claim on its
    // own rather than waiting to be told.
    holdTimer = setTimeout(() => release(self), LOCK_TTL_MS);
    return true;
  }

  /**
   * Releases a turn this device WON. A device that lost never held it and
   * publishes nothing.
   *
   * The `holding` test comes first and covers the registry too, which is the
   * whole point: this is called on every completed response — including the
   * tool-only ones realtime.mjs also reports — so a release routinely lands
   * inside some *other* change's 400 ms settle window. Dropping our own claim
   * there would delete a claim that is already filed in every peer's registry
   * for the full ttl, so arbitration would hand this device a `false` and,
   * because it never started holding, nothing would ever publish the release
   * that frees the others. A no-op release has to be a no-op all the way down.
   * Giving a claim up on purpose is abandon().
   */
  function release(self) {
    if (!holding) return;
    clearTimeout(holdTimer);
    holdTimer = null;
    claims.delete(self);
    holding = false;
    publish('');
  }

  /**
   * Gives up whatever this device has on the wire, held or merely claimed.
   *
   * The second case is the one release() cannot serve: a claim inside its
   * settle window has already been published and every peer has already filed
   * it, so a tab that closes there leaves the fleet deferring to a device that
   * is gone for the full LOCK_TTL_MS. "Has a claim on the wire" and "holds the
   * lock" are different states, and only a deliberate teardown is entitled to
   * publish a release from the first one — an unconditional publish would let
   * any device free a lock some other device is holding, because a release is
   * fleet-wide and carries no holder.
   */
  function abandon(self) {
    if (holding) {
      release(self);
      return;
    }
    if (!claiming) return;
    // Invalidates the settle-window continuation of the claim being dropped,
    // and reopens the gate the same way its `finally` would have.
    claimEpoch += 1;
    claiming = false;
    claims.delete(self);
    publish('');
  }

  return {
    observe,
    clear,
    heldByOther,
    claimTurn,
    release,
    abandon,
    holding: () => holding,
    liveClaims,
  };
}

/**
 * Routes one inbound MQTT message to the roster, the lock, or the change
 * handler. Split out of the socket so the three branches can be tested
 * directly — each of them has been silently wrong once.
 *
 * @param {object} opts
 * @param {Map<string,object>} [opts.peers]  the roster this router maintains
 * @param {object} [opts.lock]               a createSpeakingLock registry
 * @param {(ev: object) => void} [opts.onChange]
 * @param {(peers: Map<string,object>) => void} [opts.onPresence]
 */
export function createEventRouter({ peers = new Map(), lock = null, onChange, onPresence } = {}) {
  function handleMessage(topic, raw, creds) {
    // The empty-payload tests come BEFORE the parse, and that ordering is the
    // whole point. Both the presence clear and the lock release are
    // zero-length payloads; JSON.parse('') throws, and while this check lived
    // below the parse every departure returned from the catch and no device
    // was ever removed from the roster.
    const empty = !raw || raw.length === 0;

    // The turn-taking lock (§6 WS-5 M5.2), and it is matched FIRST — ahead of
    // presence and ahead of the change fall-through — because both of the
    // branches below would otherwise swallow it, each in its own wrong way.
    //
    // The lock topic deliberately sits UNDER the presence prefix
    // (`.../presence/speaking`). That is what makes it invisible to a tab
    // still running the pre-WS-5 module graph across a deploy: both old
    // clients drop anything containing '/presence/' unread, whereas the old
    // bare `.../speaking` topic fell through to the change handler and turned
    // every claim into "[Automatic update] Another device just changed
    // something shared" for an edit that never happened. The price of that
    // prefix is this ordering — matched second, a claim would be filed as a
    // peer device literally named "speaking", and a release as that peer
    // leaving. Exact topic equality, so the branch cannot widen by accident.
    if (creds && creds.speakingTopic && topic === creds.speakingTopic) {
      if (!lock) return;
      if (empty) lock.clear();
      else lock.observe(parseOrNull(raw));
      return;
    }

    // Presence is its own topic and its own bookkeeping.
    if (topic.includes('/presence/')) {
      // Keyed by the TOPIC's trailing segment, never by the payload's own
      // deviceId: the segment is what the server minted for that device, so a
      // peer cannot take over somebody else's roster slot by lying in a field.
      const deviceId = topic.slice(topic.lastIndexOf('/') + 1);
      // An empty retained payload is how a device clears itself — either its
      // Last Will fired, or it published one on a clean exit.
      if (empty || raw === '{}') peers.delete(deviceId);
      else {
        const body = parseOrNull(raw);
        if (!body) return;
        peers.set(deviceId, body);
      }
      onPresence && onPresence(new Map(peers));
      return;
    }

    if (empty) return;
    const ev = parseOrNull(raw);
    if (!ev) return;

    // The comparison that stops a device announcing its own edit. Compared
    // against the value the SERVER said it would stamp for us, not against a
    // locally derived id — those are not guaranteed to be the same string.
    if (ev.actorDeviceId && creds.actorDeviceId && ev.actorDeviceId === creds.actorDeviceId) return;

    onChange && onChange(ev);
  }

  return { peers, handleMessage };
}

/**
 * Starts the live-event connection.
 *
 * @param {object} opts
 * @param {(ev: object) => void} opts.onChange  a doc/memory event from ANOTHER device
 * @param {(peers: Map<string,object>) => void} [opts.onPresence]
 * @param {() => string} [opts.persona]  current persona label, published with presence
 * @param {() => string} [opts.state]    current PRESENCE_STATES value for this device
 * @returns {{stop: () => void, connected: () => boolean, deviceId: () => string,
 *           publishPresence: () => void, claimSpeakingTurn: () => Promise<boolean>,
 *           releaseSpeakingTurn: () => void}}
 */
export function startLiveEvents({ onChange, onPresence, persona, state } = {}) {
  let client = null;
  let refreshTimer = null;
  let stopped = false;
  /** This device's presence topic, kept so stop() can clear it deliberately. */
  let presenceTopic = '';
  /**
   * The credential the live connection was minted with. Hoisted out of
   * connect() because presence is now republished from outside the connect
   * path — on every mic state change — and the publisher needs the topics.
   */
  let creds = null;
  const peers = new Map();
  let presenceTimer = null;
  let lastPresenceAt = 0;

  const log = (...args) => console.info('[liveevents]', ...args);

  const lock = createSpeakingLock({
    publish: (payload) => {
      if (!client || !client.connected || !creds || !creds.speakingTopic) return;
      client.publish(creds.speakingTopic, payload, LOCK_QOS_OPTS);
    },
  });

  const router = createEventRouter({ peers, lock, onChange, onPresence });

  async function fetchCredentials() {
    const resp = await fetch(CREDENTIALS_PATH, { credentials: 'same-origin' });
    if (!resp.ok) throw new Error(`credentials ${resp.status}`);
    return resp.json();
  }

  function sendPresence() {
    if (!client || !client.connected || !creds) return;
    lastPresenceAt = Date.now();
    client.publish(
      creds.presenceTopic,
      JSON.stringify({
        // The roster key, and the same string the server built the presence
        // topic's last segment from. NOT actorDeviceId: that is a different
        // value and may be empty, so a roster keyed on it would not line up
        // with the topic it arrived on.
        deviceId: creds.clientId,
        actorDeviceId: creds.actorDeviceId || '',
        persona: persona ? persona() : '',
        state: state ? state() : 'idle',
      }),
      PRESENCE_QOS_OPTS,
    );
  }

  /** Republishes presence, at most once per PRESENCE_THROTTLE_MS, trailing edge guaranteed. */
  function publishPresence() {
    if (stopped) return;
    const wait = PRESENCE_THROTTLE_MS - (Date.now() - lastPresenceAt);
    if (wait > 0) {
      if (!presenceTimer) {
        presenceTimer = setTimeout(() => {
          presenceTimer = null;
          publishPresence();
        }, wait);
      }
      return;
    }
    sendPresence();
  }

  async function connect() {
    if (stopped) return;
    try {
      creds = await fetchCredentials();
    } catch (err) {
      log('credentials unavailable; cross-device notifications are off', err.message);
      return; // not retried: a signed-out or unconfigured account is not a blip
    }

    // AWS IoT takes the custom authorizer's name from the query string, and
    // the token itself from the MQTT CONNECT user-name field (a browser cannot
    // set WebSocket handshake headers, so this is the only route).
    const url = `wss://${creds.endpoint}/mqtt?x-amz-customauthorizer-name=${encodeURIComponent(creds.authorizerName)}`;
    presenceTopic = creds.presenceTopic;

    client = new MqttClient({
      url,
      clientId: creds.clientId,
      username: creds.token,
      keepAlive: 60,
      // The Last Will clears this device's presence if the socket dies without
      // a clean disconnect — a crashed tab should not linger as "here".
      will: { topic: creds.presenceTopic, payload: '', retain: true },
      onOpen: () => {
        log('connected');
        // One subscription, covering both client-published topics. A narrower
        // filter for the lock would be REFUSED — the authorizer grants
        // `liveninja/user/<uid>/#` as a literal topicfilter resource — and AWS
        // signals a refused SUBSCRIBE by closing the connection, which onClose
        // below would treat as a normal expiry and reconnect into forever.
        client.subscribe([creds.topicFilter]);
        sendPresence();
        scheduleRefresh(creds);
      },
      onMessage: (topic, payload) => router.handleMessage(topic, payload, creds),
      onError: (err) => log('error', err.message),
      onClose: () => {
        if (stopped) return;
        // The token is short-lived and the authorizer force-closes after an
        // hour, so a close is expected. Reconnecting means getting a FRESH
        // credential, never reusing the old one.
        log('closed; reconnecting with a fresh credential');
        setTimeout(connect, 2000);
      },
    });
    client.connect();
  }

  function scheduleRefresh(creds) {
    clearTimeout(refreshTimer);
    const ms = Math.max(30_000, (creds.expiresInSeconds || 900) * 1000 - REFRESH_MARGIN_MS);
    refreshTimer = setTimeout(() => {
      // Closing triggers onClose, which reconnects with a new token. Doing it
      // this way keeps one reconnect path instead of two.
      if (client) client.close();
    }, ms);
  }

  connect();

  return {
    connected: () => !!(client && client.connected),
    /** This device's roster key, so a caller can tell its own row from a peer's. */
    deviceId: () => (creds && creds.clientId) || '',
    publishPresence,
    /**
     * Claims the turn for an unprompted announcement. Costs LOCK_SETTLE_MS on
     * the winning path, so only call it where a 400 ms delay is acceptable.
     */
    async claimSpeakingTurn() {
      if (!creds || !creds.speakingTopic || !client || !client.connected) {
        // No lock is reachable, so there is nothing to arbitrate. Granting is
        // the right degraded answer: refusing would mean a device that cannot
        // reach IoT never announces anything again — a silent regression of
        // the whole notification feature — whereas granting is exactly the
        // pre-lock behaviour it has always had.
        return true;
      }
      return lock.claimTurn(creds.clientId);
    },
    releaseSpeakingTurn() {
      if (creds) lock.release(creds.clientId);
    },
    stop() {
      stopped = true;
      clearTimeout(refreshTimer);
      clearTimeout(presenceTimer);
      presenceTimer = null;
      if (client) {
        // Free the lock before the socket goes: peers would otherwise wait out
        // the full 30-second expiry before anybody spoke again. abandon(), not
        // release() — a tab closed mid-claim has a claim on the wire that every
        // peer has already filed, and release() is correctly a no-op for a
        // device that never got as far as holding, so it would publish nothing
        // and leave exactly the 30-second wait this comment claims to remove.
        if (creds) lock.abandon(creds.clientId);
        // Clear presence deliberately rather than leaving the Last Will to do
        // it: a clean exit should not look like a crash to the other devices.
        if (presenceTopic) {
          try { client.publish(presenceTopic, '', PRESENCE_QOS_OPTS); } catch { /* already gone */ }
        }
        client.close();
      }
      client = null;
    },
  };
}
