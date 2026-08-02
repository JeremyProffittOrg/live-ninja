/**
 * Cross-device change notifications over MQTT (plan.md §6 WS-3 M3.3/M3.4).
 *
 * Every device signed into this account subscribes to one user-scoped topic.
 * When one of them changes shared state — a document, a memory entity, a plan —
 * the server publishes a small notification and the others learn immediately
 * instead of on their next poll.
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
 */

import { MqttClient } from './mqtt.mjs';

const CREDENTIALS_PATH = '/api/v1/iot/credentials';

/** Refresh this long before the token expires, so a reconnect never races it. */
const REFRESH_MARGIN_MS = 60_000;

/** Presence is a retained message, so a device that joins sees who is already here. */
const PRESENCE_QOS_OPTS = { retain: true };

/**
 * Starts the live-event connection.
 *
 * @param {object} opts
 * @param {(ev: object) => void} opts.onChange  a doc/memory event from ANOTHER device
 * @param {(peers: Map<string,object>) => void} [opts.onPresence]
 * @param {() => string} [opts.persona]  current persona label, published with presence
 * @returns {{stop: () => void, connected: () => boolean}}
 */
export function startLiveEvents({ onChange, onPresence, persona } = {}) {
  let client = null;
  let refreshTimer = null;
  let stopped = false;
  /** This device's presence topic, kept so stop() can clear it deliberately. */
  let presenceTopic = '';
  const peers = new Map();

  const log = (...args) => console.info('[liveevents]', ...args);

  async function fetchCredentials() {
    const resp = await fetch(CREDENTIALS_PATH, { credentials: 'same-origin' });
    if (!resp.ok) throw new Error(`credentials ${resp.status}`);
    return resp.json();
  }

  function handleMessage(topic, raw, creds) {
    let ev;
    try {
      ev = JSON.parse(raw);
    } catch {
      return; // a malformed payload is not worth tearing the session down for
    }

    // Presence is its own topic and its own bookkeeping.
    if (topic.includes('/presence/')) {
      const deviceId = topic.slice(topic.lastIndexOf('/') + 1);
      // An empty retained payload is how a device clears itself — either its
      // Last Will fired, or it published one on a clean exit.
      if (!raw || raw === '{}') peers.delete(deviceId);
      else peers.set(deviceId, ev);
      onPresence && onPresence(new Map(peers));
      return;
    }

    // The comparison that stops a device announcing its own edit. Compared
    // against the value the SERVER said it would stamp for us, not against a
    // locally derived id — those are not guaranteed to be the same string.
    if (ev.actorDeviceId && creds.actorDeviceId && ev.actorDeviceId === creds.actorDeviceId) return;

    onChange && onChange(ev);
  }

  function publishPresence(creds) {
    if (!client || !client.connected) return;
    client.publish(
      creds.presenceTopic,
      JSON.stringify({ deviceId: creds.actorDeviceId || creds.clientId, persona: persona ? persona() : '' }),
      PRESENCE_QOS_OPTS,
    );
  }

  async function connect() {
    if (stopped) return;
    let creds;
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
        client.subscribe([creds.topicFilter]);
        publishPresence(creds);
        scheduleRefresh(creds);
      },
      onMessage: (topic, payload) => handleMessage(topic, payload, creds),
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
    stop() {
      stopped = true;
      clearTimeout(refreshTimer);
      if (client) {
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
