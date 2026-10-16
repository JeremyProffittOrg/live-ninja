import test from 'node:test';
import assert from 'node:assert/strict';
import { initPhoneBudget } from '../../../web/static/js/phonebudget.mjs';

function fixture(apiJSON, storage = new Map()) {
  let loseAuth = () => {};
  const nodes = Object.fromEntries(['status', 'refresh', 'grant'].map(key => [key, {
    disabled: key === 'grant', textContent: '', handlers: {},
    addEventListener(event, handler) { this.handlers[event] = handler; },
  }]));
  const root = { querySelector: selector => nodes[selector.match(/budget-(\w+)/)[1]] };
  initPhoneBudget(root, { apiJSON, onAuthLost: handler => { loseAuth = handler; }, newId: () => 'grant-12345678', storage: {
    getItem: key => storage.get(key), setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key),
  } });
  return { root, nodes, storage, loseAuth: () => loseAuth() };
}
const budget = { month: '2026-10', day: '2026-10-16', timezone: 'America/New_York', allocatedUSD: '20', monthlyLimitUSD: '20', dailyGrantedUSD: '5', dailySpentUSD: '0', dailyRemainingUSD: '5', expiresAt: 1792209600, pausedReason: 'voice_stop_or_active_call' };

test('unknown grant retries same persisted ID across reload and never resumes calls', async () => {
  const requests = [];
  let fail = true;
  const apiJSON = async (path, options) => {
    if (path === '/api/v1/me') return { userId: 'owner', role: 'owner' };
    if (path.endsWith('/grant')) { requests.push(JSON.parse(options.body)); if (fail) throw Error('lost response'); }
    else assert.equal(path, '/api/v1/phone/budget');
    return budget;
  };
  const first = fixture(apiJSON);
  await first.nodes.refresh.handlers.click();
  await first.nodes.grant.handlers.click();
  assert.equal(first.storage.size, 1);
  fail = false;
  const second = fixture(apiJSON, first.storage);
  await second.nodes.refresh.handlers.click();
  assert.match(second.nodes.grant.textContent, /Retry the same/);
  await second.nodes.grant.handlers.click();
  assert.deepEqual(requests, [ { requestId: 'grant-12345678', expectedDay: '2026-10-16' }, { requestId: 'grant-12345678', expectedDay: '2026-10-16' } ]);
  assert.equal(second.nodes.grant.disabled, true);
  assert.equal(second.storage.size, 0);
  assert.match(second.nodes.status.textContent, /Calls are stopped, or another call is still active/);
  assert.doesNotMatch(second.nodes.status.textContent, /1792209600|voice_stop_or_active_call/);
  assert.ok(second.nodes.status.textContent.includes(new Date(budget.expiresAt * 1000).toLocaleString(undefined, {
    timeZone: budget.timezone, dateStyle: 'medium', timeStyle: 'short',
  })));
  assert.match(second.nodes.status.textContent, /does not resume calls/);
});

test('nonowner cannot load or grant phone budget', async () => {
  let requests = 0;
  const { root, nodes } = fixture(async () => { requests++; return { role: 'member' }; });
  await nodes.refresh.handlers.click();
  await nodes.grant.handlers.click();
  assert.equal(requests, 1);
  assert.equal(root.hidden, true);
  assert.equal(nodes.grant.disabled, true);
});

test('auth loss suppresses in-flight grant response and retains exact retry identity', async () => {
  let finish, sent;
  const apiJSON = async (path, options) => {
    if (path === '/api/v1/me') return { userId: 'owner', role: 'owner' };
    if (path.endsWith('/grant')) { sent = JSON.parse(options.body); return new Promise(resolve => { finish = resolve; }); }
    return budget;
  };
  const { root, nodes, storage, loseAuth } = fixture(apiJSON);
  await nodes.refresh.handlers.click();
  const sending = nodes.grant.handlers.click();
  loseAuth(); finish(budget); await sending;
  assert.equal(root.hidden, true);
  assert.equal(nodes.grant.disabled, true);
  assert.equal(nodes.refresh.disabled, true);
  assert.match(nodes.status.textContent, /Sign-in changed/);
  assert.deepEqual(JSON.parse(storage.get('phone-budget-grant:owner')), sent);
  await nodes.grant.handlers.click();
  assert.equal(storage.size, 1);
});
