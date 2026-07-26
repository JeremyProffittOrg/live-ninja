// A browser installation has no reliable, privacy-safe hostname. Give it a
// stable random ID and a coarse human label without retaining the raw user
// agent or collecting fingerprinting inputs.

const STORAGE_KEY = 'ln.device.id';
let memoryDeviceID = '';

const UUID_RE =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

function randomUUID(cryptoImpl = globalThis.crypto) {
  if (cryptoImpl && typeof cryptoImpl.randomUUID === 'function') {
    return cryptoImpl.randomUUID();
  }
  if (cryptoImpl && typeof cryptoImpl.getRandomValues === 'function') {
    const bytes = new Uint8Array(16);
    cryptoImpl.getRandomValues(bytes);
    bytes[6] = (bytes[6] & 0x0f) | 0x40;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    const hex = [...bytes].map((byte) => byte.toString(16).padStart(2, '0'));
    return (
      hex.slice(0, 4).join('') +
      '-' +
      hex.slice(4, 6).join('') +
      '-' +
      hex.slice(6, 8).join('') +
      '-' +
      hex.slice(8, 10).join('') +
      '-' +
      hex.slice(10).join('')
    );
  }
  // This ID is an opaque installation key, not an authentication secret.
  const seed = `${Date.now()}-${Math.random()}-${Math.random()}`;
  let hash = 2166136261;
  for (const char of seed) {
    hash ^= char.charCodeAt(0);
    hash = Math.imul(hash, 16777619);
  }
  const tail = Math.abs(hash).toString(16).padStart(8, '0').slice(-8);
  return `00000000-0000-4000-8000-${tail.padStart(12, '0')}`;
}

export function getDeviceID(options = {}) {
  let storage = options.storage;
  if (!Object.prototype.hasOwnProperty.call(options, 'storage')) {
    try {
      storage = globalThis.localStorage;
    } catch {
      storage = null;
    }
  }
  const cryptoImpl = options.cryptoImpl || globalThis.crypto;
  if (UUID_RE.test(memoryDeviceID)) return memoryDeviceID;
  try {
    const saved = storage && storage.getItem(STORAGE_KEY);
    if (UUID_RE.test(saved || '')) {
      memoryDeviceID = saved;
      return memoryDeviceID;
    }
  } catch {
    /* private/storage-blocked mode falls back to this page lifetime */
  }

  memoryDeviceID = randomUUID(cryptoImpl);
  try {
    if (storage) storage.setItem(STORAGE_KEY, memoryDeviceID);
  } catch {
    /* storage-blocked mode still gets one stable ID for this page */
  }
  return memoryDeviceID;
}

export function rotateDeviceID(options = {}) {
  let storage = options.storage;
  if (!Object.prototype.hasOwnProperty.call(options, 'storage')) {
    try {
      storage = globalThis.localStorage;
    } catch {
      storage = null;
    }
  }
  const cryptoImpl = options.cryptoImpl || globalThis.crypto;
  memoryDeviceID = randomUUID(cryptoImpl);
  try {
    if (storage) storage.setItem(STORAGE_KEY, memoryDeviceID);
  } catch {
    /* storage-blocked mode still rotates this page's installation ID */
  }
  return memoryDeviceID;
}

function browserFamily(nav) {
  const brands = Array.isArray(nav?.userAgentData?.brands)
    ? nav.userAgentData.brands.map((entry) => String(entry?.brand || ''))
    : [];
  const brand = brands.find((name) => /Edge|Microsoft Edge/i.test(name))
    || brands.find((name) => /Google Chrome/i.test(name))
    || brands.find((name) => /Chromium/i.test(name));
  if (/Edge|Microsoft Edge/i.test(brand || '')) return 'Edge';
  if (/Google Chrome/i.test(brand || '')) return 'Chrome';
  if (/Chromium/i.test(brand || '')) return 'Chromium';

  // The raw UA is used only for local normalization and is never returned.
  const ua = String(nav?.userAgent || '');
  if (/Edg\//.test(ua)) return 'Edge';
  if (/Firefox\//.test(ua)) return 'Firefox';
  if (/CriOS\//.test(ua) || /Chrome\//.test(ua)) return 'Chrome';
  if (/Safari\//.test(ua) && !/Chrome\//.test(ua)) return 'Safari';
  return 'Web browser';
}

function platformFamily(nav) {
  const source = String(nav?.userAgentData?.platform || nav?.platform || nav?.userAgent || '');
  if (/Windows/i.test(source)) return 'Windows';
  if (/CrOS|Chrome OS/i.test(source)) return 'ChromeOS';
  if (/Android/i.test(source)) return 'Android';
  if (/iPhone|iPad|iPod|iOS/i.test(source)) return 'iOS';
  if (/Mac/i.test(source)) return 'macOS';
  if (/Linux/i.test(source)) return 'Linux';
  return 'Unknown platform';
}

export function inferDeviceIdentity(nav = globalThis.navigator) {
  const browser = browserFamily(nav);
  const platform = platformFamily(nav);
  const mobile =
    typeof nav?.userAgentData?.mobile === 'boolean'
      ? nav.userAgentData.mobile
      : /Android|iPhone|iPad|iPod/i.test(String(nav?.userAgent || ''));
  return {
    suggestedName: `${browser} on ${platform}`,
    metadata: {
      surface: 'web',
      browser,
      platform,
      deviceClass: mobile ? 'mobile' : 'desktop',
    },
    capabilities: [
      'aboutYou',
      'wakeWord',
      'persona',
      'voiceEngine',
      'turnDetection',
      'appearance',
      'microphone',
      'privacy',
    ],
  };
}

export const DEVICE_ID_STORAGE_KEY = STORAGE_KEY;
