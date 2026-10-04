// Bounded, Windows-only local harness for the Jobs/Approvals/Ghost Playwright
// suites. Builds cmd/jobs-preview offline into a private temp directory, runs
// it on fixed loopback 127.0.0.1:18974, runs the local suites headless, then
// terminates ONLY the process trees it spawned (by their own PIDs). Synthetic
// fixtures and local auth only; no network downloads, no remote targets.
//
// Timing model (all relative to harness start):
//   0 .. WORK_MS            build / ready / tests; deadline abort at WORK_MS
//   WORK_MS .. CLEANUP_END  single reserved cleanup budget (stop + final kill)
//   HARD_MS (< 300 s)       last-resort watchdog; exits non-zero
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HOST = '127.0.0.1';
const PORT = 18974;
const LISTEN = `${HOST}:${PORT}`;
const BASE_URL = `http://${LISTEN}`;
const MODE = 'local-jobs-preview';
const GO_EXE = 'C:\\Program Files\\Go\\bin\\go.exe';
const TASKKILL = 'C:\\Windows\\System32\\taskkill.exe';

const WORK_MS = 240_000;        // all work stages must end (or be aborted) by here
const CLEANUP_END_MS = 285_000; // all termination/cleanup must end by here
const HARD_MS = 295_000;        // last-resort watchdog, strictly < 300 s
const BUILD_MS = 120_000;
const READY_MS = 30_000;
const MIN_TEST_MS = 30_000;
const KILL_MS = 15_000;         // max per killTree call
const TASKKILL_MS = 10_000;     // max lifetime of one taskkill helper
const CLOSE_GRACE_MS = 1_000;   // wait for 'close' after confirmed exit

const EXIT = { precondition: 2, port: 3, build: 4, ready: 5, deadline: 124, signal: 130, cleanup: 1 };

const startedAt = Date.now();
const remaining = () => WORK_MS - (Date.now() - startedAt);
const cleanupLeft = () => CLEANUP_END_MS - (Date.now() - startedAt);
const log = msg => process.stdout.write(`[run-ghost-local] ${msg}\n`);

// Exact repository root: tests/web/run-ghost-local.mjs -> two levels up.
const ROOT = path.resolve(fileURLToPath(new URL('../../', import.meta.url)));
const WEB_DIR = path.join(ROOT, 'tests', 'web');
const CONFIG = path.join(WEB_DIR, 'playwright.local.config.mjs');
const PW_CLI = path.join(WEB_DIR, 'node_modules', '@playwright', 'test', 'cli.js');
const AXE_PKG = path.join(WEB_DIR, 'node_modules', '@axe-core', 'playwright', 'package.json');
const PREVIEW_MAIN = path.join(ROOT, 'cmd', 'jobs-preview', 'main.go');
const GO_MOD = path.join(ROOT, 'go.mod');
const TEST_RESULTS = path.join(WEB_DIR, 'test-results');

// owned: every child this harness spawned that has not yet emitted 'exit'.
const state = { aborted: null, active: null, preview: null, previewExited: false, owned: new Set(), code: undefined, finalized: false };

function pick(keys) {
  const out = {};
  for (const k of keys) {
    const v = process.env[k];
    if (typeof v === 'string' && v !== '') out[k] = v;
  }
  return out;
}

// Minimal, explicit environments. Nothing credential-like is forwarded.
const BASE_ENV_KEYS = ['SystemRoot', 'windir', 'TEMP', 'TMP', 'USERPROFILE', 'LOCALAPPDATA', 'APPDATA',
  'HOMEDRIVE', 'HOMEPATH', 'ProgramData', 'ProgramFiles', 'ProgramFiles(x86)', 'ProgramW6432',
  'NUMBER_OF_PROCESSORS', 'PROCESSOR_ARCHITECTURE', 'OS', 'PATHEXT', 'ComSpec'];
const SYS_PATH = 'C:\\Windows\\System32;C:\\Windows';

function goEnv() {
  return {
    ...pick(BASE_ENV_KEYS),
    ...pick(['GOPATH', 'GOMODCACHE', 'GOCACHE']),
    PATH: `C:\\Program Files\\Go\\bin;${SYS_PATH}`,
    GOTOOLCHAIN: 'local',
    GOPROXY: 'off',
    GOSUMDB: 'off',
    GOFLAGS: '-mod=readonly',
    CGO_ENABLED: '0',
  };
}

function previewEnv() {
  return { ...pick(['SystemRoot', 'windir', 'TEMP', 'TMP']), PATH: SYS_PATH };
}

function taskkillEnv() {
  return { ...pick(['SystemRoot', 'windir']), PATH: SYS_PATH };
}

function playwrightEnv() {
  return {
    ...pick(BASE_ENV_KEYS),
    ...pick(['PLAYWRIGHT_BROWSERS_PATH']),
    PATH: `${path.dirname(process.execPath)};${SYS_PATH}`,
    // All suites target the single owned loopback preview; never a default port.
    LN_JOBS_BASE_URL: BASE_URL,
    LN_APPROVALS_BASE_URL: BASE_URL,
    LN_GHOST_BASE_URL: BASE_URL,
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD: '1',
    FORCE_COLOR: '0',
  };
}

function isRunning(child) {
  return !!child && child.exitCode === null && child.signalCode === null;
}

function own(child) {
  state.owned.add(child);
  child.once('exit', () => state.owned.delete(child));
  child.once('error', () => { if (child.pid === undefined) state.owned.delete(child); });
}

// Resolve true once the child has emitted 'exit'; false if ms elapses first.
function waitExit(child, ms) {
  return new Promise(resolve => {
    if (!isRunning(child)) return resolve(true);
    if (ms <= 0) return resolve(false);
    const onExit = () => { clearTimeout(t); resolve(true); };
    const t = setTimeout(() => { child.removeListener('exit', onExit); resolve(!isRunning(child)); }, ms);
    child.once('exit', onExit);
  });
}

function withDeadline(promise, ms, fallback) {
  return new Promise(resolve => {
    let done = false;
    const t = setTimeout(() => { if (!done) { done = true; resolve(fallback()); } }, Math.max(0, ms));
    promise.then(
      v => { if (!done) { done = true; clearTimeout(t); resolve(v); } },
      () => { if (!done) { done = true; clearTimeout(t); resolve(fallback()); } });
  });
}

// Run one bounded taskkill helper against exactly one PID. If the helper
// itself exceeds its deadline, terminate ONLY the helper and release it.
// Resolves with taskkill's exit code, or null on spawn error / timeout.
function runTaskkill(pid, ms) {
  return new Promise(resolve => {
    if (ms <= 0) return resolve(null);
    let tk;
    try {
      tk = spawn(TASKKILL, ['/PID', String(pid), '/T', '/F'],
        { env: taskkillEnv(), shell: false, windowsHide: true, stdio: 'ignore' });
    } catch {
      return resolve(null);
    }
    let done = false;
    const fin = v => { if (done) return; done = true; clearTimeout(timer); resolve(v); };
    const timer = setTimeout(() => {
      log(`taskkill helper (PID ${tk.pid}) for PID ${pid} exceeded ${ms} ms; terminating the helper only`);
      try { tk.kill(); } catch { /* ignore */ }
      try { tk.unref(); } catch { /* ignore */ }
      fin(null);
    }, ms);
    tk.once('error', () => fin(null));
    tk.once('close', code => fin(code));
  });
}

async function killTreeOnce(child, label, ms) {
  const end = Date.now() + ms;
  const pid = child.pid;
  if (!Number.isInteger(pid) || pid <= 0) { log(`${label}: no valid PID to terminate`); return false; }
  const tkMs = ms >= 2_000 ? Math.min(TASKKILL_MS, ms - 1_000) : ms;
  const code = await runTaskkill(pid, tkMs);
  // Success requires an observed 'exit' of our own child, not just taskkill's code.
  const ok = await waitExit(child, end - Date.now());
  log(`${label}: taskkill PID ${pid} ${code === null ? 'did not complete' : `exit ${code}`}; termination ${ok ? 'verified' : 'NOT verified'}`);
  return ok;
}

const inflight = new WeakMap();

// Terminate exactly one spawned child's tree, by its own numeric PID, within
// min(KILL_MS, budgetMs). Always resolves within that bound.
function killTree(child, label, budgetMs) {
  if (!child || !isRunning(child)) return Promise.resolve(true);
  if (child.pid === undefined) return Promise.resolve(true); // never started
  const ms = Math.max(0, Math.min(KILL_MS, budgetMs));
  if (ms === 0) return Promise.resolve(!isRunning(child));
  let p = inflight.get(child);
  if (!p) {
    p = killTreeOnce(child, label, ms).catch(() => !isRunning(child)).finally(() => inflight.delete(child));
    inflight.set(child, p);
  }
  return withDeadline(p, ms, () => !isRunning(child));
}

// Always settles: normally on 'close'; on timeout/abort within the stop
// budget (+ grace), even if termination fails. An unterminated child stays in
// state.owned for the final cleanup.
function run(label, file, args, { cwd, env, timeoutMs }) {
  return new Promise(resolve => {
    if (state.aborted) return resolve({ code: null, aborted: true, terminated: true });
    if (timeoutMs <= 0) return resolve({ code: null, timedOut: true, terminated: true });
    let child;
    try {
      child = spawn(file, args, { cwd, env, shell: false, windowsHide: true, stdio: ['ignore', 'inherit', 'inherit'] });
    } catch (error) {
      return resolve({ code: null, error, terminated: true });
    }
    own(child);
    let settled = false;
    let timedOut = false;
    let stopping = false;
    let timer = null;
    let graceTimer = null;
    let backstop = null;
    const finish = result => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      clearTimeout(graceTimer);
      clearTimeout(backstop);
      if (state.active && state.active.child === child) state.active = null;
      resolve({ ...result, timedOut });
    };
    const stop = why => {
      if (settled || stopping) return;
      stopping = true;
      clearTimeout(timer);
      const budget = Math.max(0, Math.min(KILL_MS, cleanupLeft()));
      log(`${label}: stopping (${why}); termination budget ${budget} ms`);
      backstop = setTimeout(() => {
        const t = !isRunning(child);
        if (!t) log(`${label}: termination unconfirmed; settling, PID ${child.pid} retained for final cleanup`);
        finish({ code: child.exitCode, signal: child.signalCode, terminated: t });
      }, budget + CLOSE_GRACE_MS + 500);
      killTree(child, label, budget).then(ok => {
        if (settled) return;
        if (!ok) {
          log(`${label}: termination not verified; settling, PID ${child.pid} retained for final cleanup`);
          finish({ code: null, terminated: false });
          return;
        }
        graceTimer = setTimeout(() => finish({ code: child.exitCode, signal: child.signalCode, terminated: true }), CLOSE_GRACE_MS);
      }, () => finish({ code: null, terminated: !isRunning(child) }));
    };
    timer = setTimeout(() => {
      timedOut = true;
      log(`${label}: timed out after ${timeoutMs} ms`);
      stop('timeout');
    }, timeoutMs);
    state.active = { child, stop };
    child.once('error', error => {
      if (child.pid === undefined || !isRunning(child)) finish({ code: null, error, terminated: true });
    });
    child.once('close', (code, signal) => finish({ code, signal, terminated: true }));
    if (state.aborted) stop(state.aborted);
  });
}

function abort(reason) {
  if (state.aborted) return;
  state.aborted = reason;
  log(`aborting: ${reason}`);
  if (state.active) state.active.stop(reason);
}

function abortCode() {
  return state.aborted === 'deadline' ? EXIT.deadline : EXIT.signal;
}

async function exists(p) {
  try { await fsp.access(p, fs.constants.R_OK); return true; } catch { return false; }
}

async function mustBeAbsent(p) {
  try {
    await fsp.lstat(p);
  } catch (e) {
    if (e && e.code === 'ENOENT') return;
    throw e;
  }
  throw new Error(`path collision: ${p}`);
}

function probeConnect() {
  return new Promise(resolve => {
    const s = net.connect({ host: HOST, port: PORT });
    const t = setTimeout(() => { s.destroy(); resolve('unknown'); }, 2000);
    s.once('connect', () => { clearTimeout(t); s.destroy(); resolve('occupied'); });
    s.once('error', e => { clearTimeout(t); resolve(e && e.code === 'ECONNREFUSED' ? 'free' : 'unknown'); });
  });
}

function probeBind() {
  return new Promise(resolve => {
    const srv = net.createServer();
    srv.once('error', () => resolve(false));
    srv.listen({ host: HOST, port: PORT, exclusive: true }, () => srv.close(() => resolve(true)));
  });
}

async function portFree() {
  if ((await probeConnect()) !== 'free') return false;
  return probeBind();
}

const sleep = ms => new Promise(r => setTimeout(r, ms));

async function waitReady() {
  const until = Date.now() + Math.min(READY_MS, remaining());
  while (Date.now() < until) {
    if (state.aborted) return false;
    if (state.previewExited || !isRunning(state.preview)) { log('preview exited before becoming ready'); return false; }
    try {
      const res = await fetch(`${BASE_URL}/healthz`, { redirect: 'error', signal: AbortSignal.timeout(2000) });
      if (res.status === 200 && res.headers.get('x-live-ninja-mode') === MODE) {
        const body = await res.json();
        if (body && body.status === 'ok' && body.mode === MODE && isRunning(state.preview)) return true;
      } else {
        await res.arrayBuffer().catch(() => {});
      }
    } catch {
      // not ready yet
    }
    await sleep(250);
  }
  return false;
}

async function main() {
  if (process.platform !== 'win32') { log('native Windows only'); return EXIT.precondition; }

  for (const [p, what] of [[GO_EXE, 'Go toolchain'], [TASKKILL, 'taskkill'], [GO_MOD, 'go.mod'], [PREVIEW_MAIN, 'cmd/jobs-preview'],
    [CONFIG, 'local Playwright config'], [PW_CLI, '@playwright/test CLI'], [AXE_PKG, '@axe-core/playwright']]) {
    if (!(await exists(p))) { log(`missing dependency: ${what} (${p})`); return EXIT.precondition; }
  }
  log(`repository root: ${ROOT}`);

  if (!(await portFree())) { log(`${LISTEN} is in use or unverifiable; refusing to continue (no process was touched)`); return EXIT.port; }

  const tmpBase = path.resolve(os.tmpdir());
  const workDir = path.resolve(await fsp.mkdtemp(path.join(tmpBase, 'ln-ghost-local-')));
  if (path.dirname(workDir) !== tmpBase) { log('temporary directory outside os.tmpdir; refusing'); return EXIT.precondition; }
  log(`private work directory (owned by this run, retained as evidence): ${workDir}`);
  const binPath = path.join(workDir, 'jobs-preview.exe');
  const dataFile = path.join(workDir, 'jobs-data.json');
  const logFile = path.join(workDir, 'jobs-preview.log');
  for (const p of [binPath, dataFile, `${dataFile}.approvals`, logFile]) await mustBeAbsent(p);

  log('building cmd/jobs-preview (offline)');
  const build = await run('go build', GO_EXE, ['build', '-buildvcs=false', '-trimpath', '-o', binPath, './cmd/jobs-preview'],
    { cwd: ROOT, env: goEnv(), timeoutMs: Math.min(BUILD_MS, remaining()) });
  if (build.terminated === false) log('go build termination unverified; deferred to final cleanup');
  if (state.aborted) return abortCode();
  if (build.error || build.timedOut || build.code !== 0) {
    log(`build failed (${build.timedOut ? 'timeout' : build.error ? 'spawn error' : `exit ${build.code}`})`);
    return EXIT.build;
  }
  if (!(await exists(binPath))) { log('build produced no binary'); return EXIT.build; }

  if (!(await portFree())) { log(`${LISTEN} became occupied; refusing to start preview`); return EXIT.port; }
  if (state.aborted) return abortCode();

  const logFd = fs.openSync(logFile, 'wx');
  try {
    try {
      state.preview = spawn(binPath, ['-listen', LISTEN, '-data', dataFile],
        { cwd: workDir, env: previewEnv(), shell: false, windowsHide: true, stdio: ['ignore', logFd, logFd] });
    } catch {
      log('failed to spawn preview');
      return EXIT.ready;
    }
    own(state.preview);
    state.preview.once('error', () => { state.previewExited = true; });
    state.preview.once('exit', () => { state.previewExited = true; });
    log(`preview started, PID ${state.preview.pid}; log: ${logFile}`);

    if (!(await waitReady())) {
      if (state.aborted) return abortCode();
      log(`preview not ready with mode ${MODE}`);
      return EXIT.ready;
    }
    log(`preview healthy at ${BASE_URL}`);

    const testMs = remaining();
    if (testMs < MIN_TEST_MS) { log('insufficient time left for tests'); return EXIT.deadline; }
    log(`running local Jobs/Approvals/Ghost suites (headless, 2 workers, timeout ${testMs} ms)`);
    const tests = await run('playwright', process.execPath,
      [PW_CLI, 'test', '--config', CONFIG, '--workers=2', '--forbid-only'],
      { cwd: WEB_DIR, env: playwrightEnv(), timeoutMs: testMs });
    log(`artifacts: ${TEST_RESULTS}`);
    if (tests.terminated === false) log('playwright termination unverified; deferred to final cleanup');
    if (state.aborted) return abortCode();
    if (tests.timedOut) { log('tests timed out'); return EXIT.deadline; }
    if (tests.error) { log('failed to spawn Playwright'); return EXIT.precondition; }
    if (tests.code !== 0) { log(`tests failed (exit ${tests.code ?? tests.signal})`); return typeof tests.code === 'number' && tests.code !== 0 ? tests.code : 1; }
    if (!isRunning(state.preview)) { log('preview exited during tests'); return EXIT.ready; }
    log('tests passed');
    return 0;
  } finally {
    try { fs.closeSync(logFd); } catch { /* ignore */ }
  }
}

const deadline = setTimeout(() => abort('deadline'), WORK_MS);
const hard = setTimeout(() => {
  const c = state.finalized ? state.code : (typeof state.code === 'number' && state.code !== 0 ? state.code : EXIT.cleanup);
  log(`hard bound ${HARD_MS} ms reached; exiting ${c}`);
  process.exit(c);
}, HARD_MS);
hard.unref();
for (const sig of ['SIGINT', 'SIGTERM', 'SIGBREAK']) {
  try { process.on(sig, () => abort(`signal ${sig}`)); } catch { /* unsupported */ }
}

let code = 1;
try {
  code = await main();
} catch (e) {
  log(`harness error: ${e && e.message ? e.message : 'unknown'}`);
  code = 1;
} finally {
  state.code = code;
  clearTimeout(deadline);
  let cleanupOk = true;
  const survivors = [...state.owned].filter(isRunning);
  if (survivors.length) {
    const results = await Promise.all(survivors.map(c =>
      killTree(c, c === state.preview ? 'preview' : `owned child PID ${c.pid}`, cleanupLeft())));
    cleanupOk = results.every(Boolean);
  }
  const left = [...state.owned].filter(isRunning);
  if (left.length) cleanupOk = false;
  if (!cleanupOk) {
    log(`cleanup could not verify termination of owned PID(s): ${left.map(c => c.pid).join(', ') || 'unknown'}; failing closed`);
    if (code === 0) code = EXIT.cleanup;
    for (const c of left) { try { c.unref(); } catch { /* ignore */ } }
  }
  state.code = code;
  state.finalized = true;
  log(`exit ${code}`);
  process.exitCode = code;
}
