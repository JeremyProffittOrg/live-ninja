import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

import { test, expect } from '@playwright/test';

const ROOT = new URL('../../../', import.meta.url);
const tick = () => new Promise((resolve) => setImmediate(resolve));

function desktopOnly(testInfo) {
  test.skip(testInfo.project.name !== 'desktop-chrome', 'local runtime contract only needs one project');
}

async function withBrowserSupport(fn) {
  const windowDesc = Object.getOwnPropertyDescriptor(globalThis, 'window');
  const navigatorDesc = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  try {
    Object.defineProperty(globalThis, 'window', {
      configurable: true,
      value: { AudioWorkletNode: class AudioWorkletNode {} },
    });
    Object.defineProperty(globalThis, 'navigator', {
      configurable: true,
      value: {
        hardwareConcurrency: 1,
        mediaDevices: { getUserMedia: async () => null },
      },
    });
    await fn();
  } finally {
    if (windowDesc) Object.defineProperty(globalThis, 'window', windowDesc);
    else delete globalThis.window;
    if (navigatorDesc) Object.defineProperty(globalThis, 'navigator', navigatorDesc);
    else delete globalThis.navigator;
  }
}

async function loadServiceWorkerHarness() {
  const source = await readFile(new URL('web/sw.js', ROOT), 'utf8');
  const listeners = new Map();
  const cacheMaps = new Map();
  let fetchImpl = async () => {
    throw new Error('network not configured');
  };
  let putHook = async () => {};
  let openHook = async () => {};

  const keyFor = (request, ignoreSearch = false) => {
    const raw = typeof request === 'string' ? request : request.url;
    const url = new URL(raw, 'https://live.test/');
    if (ignoreSearch) url.search = '';
    return url.href;
  };

  const cachesApi = {
    async keys() {
      return [...cacheMaps.keys()];
    },
    async delete(name) {
      return cacheMaps.delete(name);
    },
    async open(name) {
      await openHook(name);
      if (!cacheMaps.has(name)) cacheMaps.set(name, new Map());
      const entries = cacheMaps.get(name);
      return {
        async match(request, opts = {}) {
          const stored = entries.get(keyFor(request, !!opts.ignoreSearch));
          return stored ? stored.clone() : undefined;
        },
        async put(request, response) {
          await putHook(name, request, response);
          entries.set(keyFor(request), response.clone());
        },
      };
    },
  };

  const context = {
    URL,
    Request,
    Response,
    console,
    caches: cachesApi,
    fetch: (...args) => fetchImpl(...args),
    self: {
      location: new URL('https://live.test/'),
      clients: { claim: async () => {} },
      skipWaiting: async () => {},
      addEventListener(type, handler) {
        listeners.set(type, handler);
      },
    },
  };
  vm.runInNewContext(source, context, { filename: 'web/sw.js' });

  return {
    setFetch(fn) {
      fetchImpl = fn;
    },
    setPutHook(fn) {
      putHook = fn;
    },
    setOpenHook(fn) {
      openHook = fn;
    },
    async seed(cacheName, url, response) {
      if (!cacheMaps.has(cacheName)) cacheMaps.set(cacheName, new Map());
      cacheMaps.get(cacheName).set(keyFor(url), response.clone());
    },
    async cachedText(cacheName, url) {
      const response = cacheMaps.get(cacheName)?.get(keyFor(url));
      return response ? response.clone().text() : null;
    },
    dispatchFetch(request) {
      let responsePromise;
      const lifetimePromises = [];
      listeners.get('fetch')({
        request,
        respondWith(value) {
          responsePromise = Promise.resolve(value);
        },
        waitUntil(value) {
          lifetimePromises.push(Promise.resolve(value));
        },
      });
      return { responsePromise, lifetimePromises };
    },
  };
}

test('transcript turns start after the broker session marker', async ({}, testInfo) => {
  desktopOnly(testInfo);

  const originalDescriptors = new Map();
  const installGlobal = (name, value) => {
    originalDescriptors.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
  };
  const requests = [];
  installGlobal('document', {
    cookie: '',
    visibilityState: 'visible',
    addEventListener() {},
  });
  installGlobal('window', {
    addEventListener() {},
    location: { assign() {} },
  });
  installGlobal('fetch', async (path, options = {}) => {
    if (path === '/api/v1/auth/refresh') {
      return Response.json({
        accessToken: 'transcript-test-token',
        expiresAt: Math.floor(Date.now() / 1000) + 3600,
      });
    }
    if (path === '/test/transcript') {
      requests.push(JSON.parse(options.body));
      return Response.json({ ok: true, written: 1 });
    }
    throw new Error(`Unexpected fetch: ${path}`);
  });

  try {
    const moduleUrl = new URL('web/static/js/transcriptsink.mjs', ROOT);
    moduleUrl.searchParams.set('sequence-marker-test', String(Date.now()));
    const { createTranscriptSink } = await import(moduleUrl);
    const sink = createTranscriptSink({
      endpoint: '/test/transcript',
      flushIntervalMs: 60_000,
    });
    sink.beginSession('broker-session', 'gemini-flash-live');
    sink.addTurn('user', 'Keep the first spoken turn.');
    await sink.flush();

    expect(requests).toHaveLength(1);
    expect(requests[0].turns).toEqual([{
      seq: 1,
      role: 'user',
      text: 'Keep the first spoken turn.',
      engine: 'gemini-flash-live',
    }]);
  } finally {
    for (const [name, descriptor] of originalDescriptors) {
      if (descriptor) Object.defineProperty(globalThis, name, descriptor);
      else delete globalThis[name];
    }
  }
});

test('wake-word settings hot-swap the active detector and release old ONNX sessions', async ({}, testInfo) => {
  desktopOnly(testInfo);
  await withBrowserSupport(async () => {
    const { WakeWordEngine, applyWakeWordSettings } = await import(
      new URL('web/static/js/wakeword.mjs', ROOT)
    );
    const released = [];
    const fakeSession = (name) => ({
      release: async () => {
        released.push(name);
      },
    });
    const engine = new WakeWordEngine({ wakeWordId: 'old-word', sensitivity: 0.2 });
    engine._state = 'listening';
    engine._phrase = 'Old word';
    engine._sessions = {
      mel: fakeSession('mel'),
      emb: fakeSession('emb'),
      det: fakeSession('det'),
    };
    const oldSessions = engine._sessions;
    const replacement = {
      mel: fakeSession('replacement-mel'),
      emb: fakeSession('replacement-emb'),
      det: fakeSession('replacement-det'),
    };
    let loadedFor = '';
    let oldStayedActiveWhileLoading = false;
    engine._loadRuntime = async () => {};
    engine._createModelBundle = async (id) => {
      loadedFor = id;
      oldStayedActiveWhileLoading =
        engine.wakeWordId === 'old-word' && engine._sessions === oldSessions;
      return {
        sessions: replacement,
        names: {
          melIn: 'mel-in',
          melOut: 'mel-out',
          embIn: 'emb-in',
          embOut: 'emb-out',
          detIn: 'det-in',
          detOut: 'det-out',
        },
        detWindow: 16,
        phrase: 'New word',
      };
    };

    const delta = await applyWakeWordSettings(
      engine,
      { wakeWord: 'old-word', sensitivity: 0.2 },
      { wakeWord: 'new-word', sensitivity: 0.8 },
    );

    expect(delta).toEqual({ wakeWordChanged: true, sensitivityChanged: true });
    expect(loadedFor).toBe('new-word');
    expect(oldStayedActiveWhileLoading).toBeTruthy();
    expect(engine.wakeWordId).toBe('new-word');
    expect(engine.phrase).toBe('New word');
    expect(engine._threshold).toBeCloseTo(0.26, 8);
    expect(engine._sessions).toBe(replacement);
    expect(released.sort()).toEqual(['det', 'emb', 'mel']);
  });
});

test('model buffers require a matching SHA-256 before session creation', async ({}, testInfo) => {
  desktopOnly(testInfo);
  const { fetchVerified } = await import(new URL('web/static/js/wakeword.mjs', ROOT));
  const payload = Buffer.from('detector-model-v2');
  const sha = createHash('sha256').update(payload).digest('hex');
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response(payload, { status: 200 });
  try {
    expect(Buffer.from(await fetchVerified('/model.onnx', sha))).toEqual(payload);
    await expect(fetchVerified('/model.onnx', '0'.repeat(64))).rejects.toThrow(
      /SHA-256 mismatch/,
    );
    await expect(fetchVerified('/model.onnx', '')).rejects.toThrow(
      /Missing or invalid SHA-256/,
    );
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('a rejected replacement leaves the proven wake-word model active', async ({}, testInfo) => {
  desktopOnly(testInfo);
  await withBrowserSupport(async () => {
    const { WakeWordEngine } = await import(new URL('web/static/js/wakeword.mjs', ROOT));
    const oldSessions = {
      mel: { release: async () => {} },
      emb: { release: async () => {} },
      det: { release: async () => {} },
    };
    const engine = new WakeWordEngine({ wakeWordId: 'proven-word' });
    engine._state = 'listening';
    engine._phrase = 'Proven word';
    engine._sessions = oldSessions;
    engine._loadRuntime = async () => {};
    engine._createModelBundle = async () => {
      throw new Error('SHA-256 mismatch for replacement');
    };

    let failure;
    try {
      await engine.setWakeWord('unverified-word');
    } catch (err) {
      failure = err;
    }
    expect(failure?.wakeWordPreserved).toBeTruthy();
    expect(engine.state).toBe('listening');
    expect(engine.wakeWordId).toBe('proven-word');
    expect(engine.phrase).toBe('Proven word');
    expect(engine._sessions).toBe(oldSessions);
  });
});

test('the inline settings writer notifies the conversation delta path', async ({}, testInfo) => {
  desktopOnly(testInfo);
  const [settingsSource, conversationSource] = await Promise.all([
    readFile(new URL('web/static/js/settings.mjs', ROOT), 'utf8'),
    readFile(new URL('web/static/js/conversation.mjs', ROOT), 'utf8'),
  ]);
  expect(settingsSource).toContain("new CustomEvent('ln:settings-changed'");
  expect(conversationSource).toContain("const SETTINGS_LOCAL_EVENT = 'ln:settings-changed'");
  expect(conversationSource).toContain('await applyWakeWordSettings(wakeEngine, prev, fresh)');
  expect(conversationSource).toContain(
    'await applySettingsDelta(adoption.prev, adoption.fresh)',
  );
});

test('online navigation waits for its cache write and is then available offline', async ({}, testInfo) => {
  desktopOnly(testInfo);
  const harness = await loadServiceWorkerHarness();
  const url = 'https://live.test/conversation';
  let releasePut;
  const putGate = new Promise((resolve) => {
    releasePut = resolve;
  });
  let putStarted = false;
  harness.setPutHook(async (cacheName) => {
    if (cacheName.startsWith('ln-html-')) {
      putStarted = true;
      await putGate;
    }
  });
  harness.setFetch(async () => new Response('<h1>fresh shell</h1>', { status: 200 }));

  const online = harness.dispatchFetch(
    new Request(url, { headers: { accept: 'text/html' } }),
  );
  let responseSettled = false;
  online.responsePromise.then(() => {
    responseSettled = true;
  });
  for (let i = 0; i < 5 && !putStarted; i++) await tick();
  expect(putStarted).toBeTruthy();
  expect(responseSettled, 'navigation resolved before cache.put completed').toBeFalsy();

  releasePut();
  expect(await (await online.responsePromise).text()).toContain('fresh shell');

  harness.setPutHook(async () => {});
  harness.setFetch(async () => {
    throw new Error('offline');
  });
  const offline = harness.dispatchFetch(
    new Request(url, { headers: { accept: 'text/html' } }),
  );
  expect(await (await offline.responsePromise).text()).toContain('fresh shell');

  harness.setPutHook(async () => {
    throw new Error('cache quota exceeded');
  });
  harness.setFetch(async () => new Response('<h1>network still wins</h1>', { status: 200 }));
  const noStorage = harness.dispatchFetch(
    new Request('https://live.test/history', { headers: { accept: 'text/html' } }),
  );
  expect(await (await noStorage.responsePromise).text()).toContain('network still wins');

  harness.setOpenHook(async () => {
    throw new Error('Cache Storage unavailable');
  });
  const noCacheAPI = harness.dispatchFetch(
    new Request('https://live.test/settings', { headers: { accept: 'text/html' } }),
  );
  expect(await (await noCacheAPI.responsePromise).text()).toContain('network still wins');
});

test('static revalidation keeps the worker alive through cache.put', async ({}, testInfo) => {
  desktopOnly(testInfo);
  const harness = await loadServiceWorkerHarness();
  const url = 'https://live.test/static/js/app.123.js';
  await harness.seed('ln-static-v3', url, new Response('old asset', { status: 200 }));

  let releasePut;
  const putGate = new Promise((resolve) => {
    releasePut = resolve;
  });
  let putStarted = false;
  harness.setPutHook(async (cacheName) => {
    if (cacheName === 'ln-static-v3') {
      putStarted = true;
      await putGate;
    }
  });
  harness.setFetch(async () => ({
    status: 200,
    type: 'basic',
    clone: () => new Response('new asset', { status: 200 }),
  }));

  const result = harness.dispatchFetch(new Request(url));
  expect(result.lifetimePromises).toHaveLength(1);
  expect(await (await result.responsePromise).text()).toBe('old asset');

  let lifetimeSettled = false;
  result.lifetimePromises[0].then(() => {
    lifetimeSettled = true;
  });
  for (let i = 0; i < 5 && !putStarted; i++) await tick();
  expect(putStarted).toBeTruthy();
  expect(lifetimeSettled, 'waitUntil resolved before cache.put completed').toBeFalsy();

  releasePut();
  await result.lifetimePromises[0];
  expect(await harness.cachedText('ln-static-v3', url)).toBe('new asset');
});

test('static assets remain network-available when Cache Storage cannot open', async ({}, testInfo) => {
  desktopOnly(testInfo);
  const harness = await loadServiceWorkerHarness();
  harness.setOpenHook(async () => {
    throw new Error('Cache Storage unavailable');
  });
  harness.setFetch(async () => new Response('network asset', { status: 200 }));

  const result = harness.dispatchFetch(
    new Request('https://live.test/static/js/network-only.js'),
  );
  expect(await (await result.responsePromise).text()).toBe('network asset');
  await Promise.all(result.lifetimePromises);
});

test('API and auth traffic still bypass the service worker', async ({}, testInfo) => {
  desktopOnly(testInfo);
  const harness = await loadServiceWorkerHarness();
  for (const path of ['/api/v1/settings', '/v1/app/android/latest', '/auth/callback']) {
    const result = harness.dispatchFetch(
      new Request(`https://live.test${path}`, { headers: { Accept: 'text/html' } }),
    );
    expect(result.responsePromise, path).toBeUndefined();
    expect(result.lifetimePromises, path).toEqual([]);
  }
});

test('Nova sends the minted config first and stays pending until the bridge ACK', async ({}, testInfo) => {
  desktopOnly(testInfo);

  const originalDescriptors = new Map();
  const installGlobal = (name, value) => {
    originalDescriptors.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
  };

  const makeNode = () => ({
    connect() {},
    disconnect() {},
  });
  let stoppedNovaSources = 0;
  class FakeAudioContext {
    constructor(options = {}) {
      this.sampleRate = options.sampleRate || 48_000;
      this.destination = {};
      this.currentTime = 0;
    }
    resume() {
      return Promise.resolve();
    }
    close() {
      return Promise.resolve();
    }
    createMediaStreamSource() {
      return makeNode();
    }
    createScriptProcessor() {
      return { ...makeNode(), onaudioprocess: null };
    }
    createGain() {
      return { ...makeNode(), gain: { value: 1 } };
    }
    createBuffer(_channels, length, sampleRate) {
      return {
        duration: length / sampleRate,
        copyToChannel() {},
      };
    }
    createBufferSource() {
      return {
        ...makeNode(),
        buffer: null,
        onended: null,
        start() {},
        stop() {
          stoppedNovaSources += 1;
        },
      };
    }
  }
  class FakeDataChannel {
    close() {}
  }
  class FakePeerConnection {
    constructor() {
      this.iceGatheringState = 'complete';
      this.localDescription = null;
      this.dataChannel = new FakeDataChannel();
    }
    createDataChannel() {
      return this.dataChannel;
    }
    addTrack() {}
    async createOffer() {
      return { type: 'offer', sdp: 'unused-nova-offer' };
    }
    async setLocalDescription(description) {
      this.localDescription = description;
    }
    addEventListener() {}
    removeEventListener() {}
    close() {}
  }
  class FakeWebSocket {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSED = 3;
    static instances = [];

    constructor(url) {
      this.url = url;
      this.readyState = FakeWebSocket.CONNECTING;
      this.sent = [];
      FakeWebSocket.instances.push(this);
    }
    open() {
      this.readyState = FakeWebSocket.OPEN;
      this.onopen?.();
    }
    receive(value) {
      this.onmessage?.({ data: JSON.stringify(value) });
    }
    receiveBinary(value) {
      this.onmessage?.({ data: value });
    }
    send(value) {
      this.sent.push(value);
    }
    close() {
      this.readyState = FakeWebSocket.CLOSED;
      this.onclose?.({ code: 1000 });
    }
  }
  class TestCustomEvent extends Event {
    constructor(type, init = {}) {
      super(type);
      this.detail = init.detail;
    }
  }

  const sessionConfig = {
    systemPrompt: 'Server-authored Nova instructions',
    voice: 'tiffany',
    toolManifest: [{ name: 'get_weather' }],
  };
  const stream = () => {
    const track = { enabled: true, stop() {} };
    return {
      getAudioTracks: () => [track],
      getTracks: () => [track],
    };
  };

  installGlobal('document', { cookie: '' });
  installGlobal('window', { AudioContext: FakeAudioContext });
  installGlobal('RTCPeerConnection', FakePeerConnection);
  installGlobal('WebSocket', FakeWebSocket);
  installGlobal('CustomEvent', TestCustomEvent);
  installGlobal('fetch', async (path) => {
    if (path === '/api/v1/auth/refresh') {
      return Response.json({
        accessToken: 'test-access-token',
        expiresAt: Math.floor(Date.now() / 1000) + 3600,
      });
    }
    if (path === '/test/nova-session') {
      return Response.json({
        mode: 'nova-bridge',
        wsUrl: 'wss://live.test/nova/session?token=opaque',
        sessionId: 'minted-session',
        model: 'nova-sonic-v1',
        sessionConfig,
      });
    }
    if (path === '/test/nova-session-without-config') {
      return Response.json({
        mode: 'nova-bridge',
        wsUrl: 'wss://live.test/nova/session?token=opaque',
      });
    }
    throw new Error(`Unexpected fetch: ${path}`);
  });

  let session;
  try {
    const moduleUrl = new URL('web/static/js/realtime.mjs', ROOT);
    moduleUrl.searchParams.set('nova-handshake-test', String(Date.now()));
    const { RealtimeSession } = await import(moduleUrl);
    session = new RealtimeSession({ sessionPath: '/test/nova-session' });

    const connecting = session.connect({ stream: stream() });
    for (let i = 0; i < 10 && FakeWebSocket.instances.length === 0; i++) await tick();
    expect(FakeWebSocket.instances).toHaveLength(1);
    const ws = FakeWebSocket.instances[0];
    expect(ws.sent).toEqual([]);

    let connectState = 'pending';
    connecting.then(
      () => {
        connectState = 'resolved';
      },
      () => {
        connectState = 'rejected';
      },
    );
    ws.open();
    expect(ws.sent).toHaveLength(1);
    expect(JSON.parse(ws.sent[0])).toEqual({
      type: 'session.start',
      config: sessionConfig,
    });

    await tick();
    expect(connectState, 'connect resolved before inbound session.start ACK').toBe('pending');
    expect(session.isConnected).toBeFalsy();

    ws.receive({
      type: 'session.start',
      sessionId: 'bridge-session',
      input: { sampleRate: 16_000 },
      output: { sampleRate: 24_000 },
    });
    await connecting;
    expect(connectState).toBe('resolved');
    expect(session.isConnected).toBeTruthy();
    expect(session.sessionId).toBe('bridge-session');

    let transcriptDelta;
    let bridgeError;
    let speakingEnded = 0;
    session.addEventListener('assistantdelta', (event) => {
      transcriptDelta = event.detail;
    });
    session.addEventListener('servererror', (event) => {
      bridgeError = event.detail.error;
    });
    session.addEventListener('speakingended', () => {
      speakingEnded += 1;
    });
    ws.receiveBinary(new Int16Array([100, -100]).buffer);
    expect(stoppedNovaSources).toBe(0);
    ws.receive({ type: 'turn.end', role: 'assistant', interrupted: true });
    ws.receive({ type: 'transcript', role: 'assistant', text: 'Hello', final: false });
    ws.receive({ type: 'error', code: 'nova_test', message: 'bridge failed' });
    await tick();
    expect(stoppedNovaSources).toBe(1);
    expect(speakingEnded).toBe(1);
    expect(transcriptDelta).toMatchObject({ delta: 'Hello', text: 'Hello' });
    expect(bridgeError).toMatchObject({ code: 'nova_test', message: 'bridge failed' });

    const missingConfig = new RealtimeSession({
      sessionPath: '/test/nova-session-without-config',
    });
    await expect(missingConfig.connect({ stream: stream() })).rejects.toMatchObject({
      code: 'mint_failed',
    });
    expect(FakeWebSocket.instances).toHaveLength(1);
  } finally {
    session?.close();
    for (const [name, descriptor] of originalDescriptors) {
      if (descriptor) Object.defineProperty(globalThis, name, descriptor);
      else delete globalThis[name];
    }
  }
});

test('Gemini resumes safely within one broker token and never leaks a fresh session', async ({}, testInfo) => {
  desktopOnly(testInfo);

  const originalDescriptors = new Map();
  const installGlobal = (name, value) => {
    originalDescriptors.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
  };
  const makeNode = () => ({
    connect() {},
    disconnect() {},
  });
  class FakeAudioContext {
    constructor(options = {}) {
      this.sampleRate = options.sampleRate || 48_000;
      this.destination = {};
      this.currentTime = 0;
    }
    resume() {
      return Promise.resolve();
    }
    close() {
      return Promise.resolve();
    }
    createMediaStreamSource() {
      return makeNode();
    }
    createScriptProcessor() {
      return { ...makeNode(), onaudioprocess: null };
    }
    createGain() {
      return { ...makeNode(), gain: { value: 1 } };
    }
    createBuffer(_channels, length, sampleRate) {
      return {
        duration: length / sampleRate,
        copyToChannel() {},
      };
    }
    createBufferSource() {
      return {
        ...makeNode(),
        buffer: null,
        onended: null,
        start() {},
        stop() {},
      };
    }
  }
  class FakeDataChannel {
    close() {}
  }
  class FakePeerConnection {
    constructor() {
      this.iceGatheringState = 'complete';
      this.localDescription = null;
      this.dataChannel = new FakeDataChannel();
    }
    createDataChannel() {
      return this.dataChannel;
    }
    addTrack() {}
    async createOffer() {
      return { type: 'offer', sdp: 'unused-gemini-offer' };
    }
    async setLocalDescription(description) {
      this.localDescription = description;
    }
    addEventListener() {}
    removeEventListener() {}
    close() {}
  }
  class FakeWebSocket {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSED = 3;
    static instances = [];

    constructor(url) {
      this.url = url;
      this.readyState = FakeWebSocket.CONNECTING;
      this.sent = [];
      FakeWebSocket.instances.push(this);
    }
    open() {
      this.readyState = FakeWebSocket.OPEN;
      this.onopen?.();
    }
    receive(value) {
      this.onmessage?.({ data: JSON.stringify(value) });
    }
    send(value) {
      this.sent.push(value);
    }
    drop(code = 1006) {
      this.readyState = FakeWebSocket.CLOSED;
      this.onclose?.({ code });
    }
    close() {
      this.readyState = FakeWebSocket.CLOSED;
      this.onclose?.({ code: 1000 });
    }
  }
  class TestCustomEvent extends Event {
    constructor(type, init = {}) {
      super(type);
      this.detail = init.detail;
    }
  }

  const sessionConfig = {
    model: 'models/gemini-test-live',
    generationConfig: { responseModalities: ['AUDIO'] },
    sessionResumption: {},
    contextWindowCompression: { slidingWindow: {} },
    inputAudioTranscription: {},
    outputAudioTranscription: {},
  };
  const stream = () => {
    const track = { enabled: true, stop() {} };
    return {
      getAudioTracks: () => [track],
      getTracks: () => [track],
    };
  };
  let sessionMints = 0;
  let toolInvokes = 0;
  let markToolInvokeStarted;
  const toolInvokeStarted = new Promise((resolve) => {
    markToolInvokeStarted = resolve;
  });
  let releaseToolInvoke;
  const toolInvokeReleased = new Promise((resolve) => {
    releaseToolInvoke = resolve;
  });
  const originalDateNow = Date.now;

  installGlobal('document', { cookie: '' });
  installGlobal('window', { AudioContext: FakeAudioContext });
  installGlobal('RTCPeerConnection', FakePeerConnection);
  installGlobal('WebSocket', FakeWebSocket);
  installGlobal('CustomEvent', TestCustomEvent);
  installGlobal('fetch', async (path) => {
    if (path === '/api/v1/auth/refresh') {
      return Response.json({
        accessToken: 'test-access-token',
        expiresAt: Math.floor(Date.now() / 1000) + 3600,
      });
    }
    if (path === '/test/gemini-session') {
      sessionMints += 1;
      return Response.json({
        mode: 'gemini-direct',
        geminiEndpoint: 'wss://generativelanguage.test/BidiGenerateContentConstrained',
        accessToken: {
          value: `auth_tokens/test-ephemeral-${sessionMints}`,
          expiresAt: new Date(Date.now() + 30 * 60_000).toISOString(),
        },
        sessionId: 'gemini-session',
        model: 'gemini-test-live',
        voice: 'Kore',
        engine: 'gemini-flash-live',
        sessionConfig,
      });
    }
    if (path === '/api/v1/tools/invoke') {
      toolInvokes += 1;
      markToolInvokeStarted();
      await toolInvokeReleased;
      return Response.json({
        tool: 'get_weather',
        ok: true,
        output: { temperatureC: 21 },
      });
    }
    throw new Error(`Unexpected fetch: ${path}`);
  });

  let session;
  let unsafeSession;
  try {
    const moduleUrl = new URL('web/static/js/realtime.mjs', ROOT);
    moduleUrl.searchParams.set('gemini-resumption-test', String(Date.now()));
    const { RealtimeSession } = await import(moduleUrl);
    session = new RealtimeSession({ sessionPath: '/test/gemini-session' });

    const connecting = session.connect({ stream: stream() });
    for (let i = 0; i < 10 && FakeWebSocket.instances.length === 0; i++) await tick();
    expect(FakeWebSocket.instances).toHaveLength(1);
    const first = FakeWebSocket.instances[0];
    first.open();
    expect(JSON.parse(first.sent[0])).toEqual({ setup: sessionConfig });
    first.receive({ setupComplete: {} });
    await connecting;
    expect(session.isConnected).toBeTruthy();

    let connectionLost = 0;
    session.addEventListener('connectionlost', () => {
      connectionLost += 1;
    });
    first.receive({
      sessionResumptionUpdate: { resumable: true, newHandle: 'resume-handle-1' },
    });
    const toolResultObserved = new Promise((resolve) => {
      session.addEventListener('toolresult', resolve, { once: true });
    });
    first.receive({
      toolCall: {
        functionCalls: [{
          id: 'weather-call-1',
          name: 'get_weather',
          args: { location: 'home' },
        }],
      },
    });
    await toolInvokeStarted;
    // The tool call advanced model state beyond handle-1. A goAway cannot
    // reuse that stale checkpoint while the tool is still in flight.
    first.receive({ goAway: { timeLeft: '30s' } });
    await tick();
    expect(FakeWebSocket.instances).toHaveLength(1);
    // A fresh post-tool-call checkpoint permits the pending replacement and
    // is consumed exactly once.
    first.receive({
      sessionResumptionUpdate: { resumable: true, newHandle: 'resume-handle-2' },
    });
    for (let i = 0; i < 10 && FakeWebSocket.instances.length < 2; i++) await tick();
    expect(FakeWebSocket.instances).toHaveLength(2);
    const second = FakeWebSocket.instances[1];
    second.open();
    expect(JSON.parse(second.sent[0]).setup.sessionResumption).toEqual({
      handle: 'resume-handle-2',
    });
    // Complete the tool while break-before-make is awaiting setupComplete.
    // Its control frame must be retained, never sent on the not-yet-ready
    // replacement and never lost with the predecessor.
    releaseToolInvoke();
    await toolResultObserved;
    expect(second.sent).toHaveLength(1);
    second.receive({ setupComplete: {} });
    await tick();
    const secondControlFrames = second.sent.slice(1).map((frame) => JSON.parse(frame));
    expect(secondControlFrames).toEqual([{
      toolResponse: {
        functionResponses: [{
          id: 'weather-call-1',
          name: 'get_weather',
          response: {
            result: {
              tool: 'get_weather',
              ok: true,
              output: { temperatureC: 21 },
            },
          },
        }],
      },
    }]);
    expect(toolInvokes).toBe(1);
    expect(session.isConnected).toBeTruthy();
    expect(connectionLost).toBe(0);
    expect(sessionMints).toBe(1);
    second.receive({ serverContent: { turnComplete: true } });
    await tick();
    expect(FakeWebSocket.instances).toHaveLength(2);
    expect(second.sent.slice(1).map((frame) => JSON.parse(frame))).toEqual(secondControlFrames);

    // The consumed handle is not reusable until the replacement socket
    // declares a fresh safe checkpoint.
    second.receive({
      sessionResumptionUpdate: { resumable: true, newHandle: 'resume-handle-1b' },
    });
    // A close without a preceding goAway can then use that fresh handle.
    second.drop();
    for (let i = 0; i < 10 && FakeWebSocket.instances.length < 3; i++) await tick();
    expect(FakeWebSocket.instances).toHaveLength(3);
    const third = FakeWebSocket.instances[2];
    third.open();
    expect(JSON.parse(third.sent[0]).setup.sessionResumption).toEqual({
      handle: 'resume-handle-1b',
    });
    third.receive({ setupComplete: {} });
    await tick();
    expect(session.isConnected).toBeTruthy();
    expect(connectionLost).toBe(0);
    expect(sessionMints).toBe(1);

    // A replacement starts non-resumable even though it used a prior
    // handle. An explicit false update remains authoritative.
    // A false update is authoritative: wait for a new safe checkpoint
    // instead of silently rolling back generation/tool state to handle-1.
    third.receive({ sessionResumptionUpdate: { resumable: false } });
    third.receive({ goAway: { timeLeft: '30s' } });
    await tick();
    expect(FakeWebSocket.instances).toHaveLength(3);
    third.receive({
      sessionResumptionUpdate: { resumable: true, newHandle: 'resume-handle-2' },
    });
    for (let i = 0; i < 10 && FakeWebSocket.instances.length < 4; i++) await tick();
    expect(FakeWebSocket.instances).toHaveLength(4);
    const fourth = FakeWebSocket.instances[3];
    fourth.open();
    expect(JSON.parse(fourth.sent[0]).setup.sessionResumption).toEqual({
      handle: 'resume-handle-2',
    });
    fourth.receive({ setupComplete: {} });
    await tick();
    expect(session.isConnected).toBeTruthy();
    expect(connectionLost).toBe(0);
    expect(sessionMints).toBe(1);

    // Past expiresAt, the current broker endpoint could only allocate a new
    // session id/slot. End this logical conversation rather than hiding that
    // identity and quota leak behind Google's otherwise-valid handle.
    Date.now = () => originalDateNow() + 31 * 60_000;
    fourth.receive({
      sessionResumptionUpdate: { resumable: true, newHandle: 'resume-handle-expired' },
    });
    fourth.receive({ goAway: { timeLeft: '30s' } });
    await tick();
    expect(FakeWebSocket.instances).toHaveLength(4);
    expect(sessionMints).toBe(1);
    expect(connectionLost).toBe(1);

    // A separate, user-initiated conversation may mint normally. After a
    // successful resume, that checkpoint is consumed: a close before the
    // replacement issues a fresh safe update must not replay it again.
    Date.now = originalDateNow;
    unsafeSession = new RealtimeSession({ sessionPath: '/test/gemini-session' });
    const unsafeConnecting = unsafeSession.connect({ stream: stream() });
    for (let i = 0; i < 10 && FakeWebSocket.instances.length < 5; i++) await tick();
    const fifth = FakeWebSocket.instances[4];
    fifth.open();
    expect(JSON.parse(fifth.sent[0])).toEqual({ setup: sessionConfig });
    fifth.receive({ setupComplete: {} });
    await unsafeConnecting;
    expect(sessionMints).toBe(2);

    let unsafeConnectionLost = 0;
    unsafeSession.addEventListener('connectionlost', () => {
      unsafeConnectionLost += 1;
    });
    fifth.receive({
      sessionResumptionUpdate: { resumable: true, newHandle: 'unsafe-handle' },
    });
    fifth.receive({ goAway: { timeLeft: '30s' } });
    for (let i = 0; i < 10 && FakeWebSocket.instances.length < 6; i++) await tick();
    const sixth = FakeWebSocket.instances[5];
    sixth.open();
    expect(JSON.parse(sixth.sent[0]).setup.sessionResumption).toEqual({
      handle: 'unsafe-handle',
    });
    sixth.receive({ setupComplete: {} });
    await tick();
    // Explicit false is authoritative too; no historical handle survives.
    sixth.receive({ sessionResumptionUpdate: { resumable: false } });
    sixth.drop();
    await tick();
    expect(FakeWebSocket.instances).toHaveLength(6);
    expect(unsafeConnectionLost).toBe(1);
  } finally {
    Date.now = originalDateNow;
    session?.close();
    unsafeSession?.close();
    for (const [name, descriptor] of originalDescriptors) {
      if (descriptor) Object.defineProperty(globalThis, name, descriptor);
      else delete globalThis[name];
    }
  }
});
