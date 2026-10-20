import test from 'node:test';
import assert from 'node:assert/strict';
import { initPhoneBudget } from '../../../web/static/js/phonebudget.mjs';

function fixture(apiJSON) {
  let loseAuth = () => {};
  const nodes = Object.fromEntries(['status', 'refresh'].map(key => [key, {
    disabled: false, textContent: '', handlers: {},
    addEventListener(event, handler) { this.handlers[event] = handler; },
  }]));
  const root = { querySelector: selector => nodes[selector.match(/budget-(\w+)/)[1]] };
  initPhoneBudget(root, { apiJSON, onAuthLost: handler => { loseAuth = handler; } });
  return { root, nodes, loseAuth: () => loseAuth() };
}
const budget = {
  month: '2026-10', day: '2026-10-19', timezone: 'America/New_York', allocatedUSD: '12.60915',
  monthlyCapUSD: '100', monthlyRemainingUSD: '87.39085', dailyAllowanceUSD: '10', dailyReservedUSD: '1.40',
  dailyRemainingUSD: '8.60', unsettledCalls: 1, requiredReservationUSD: '10', minimumReservationUSD: '0.30',
  perCallCapUSD: '10', perCallMaxSeconds: 600, telephoneReserveUSD: '0.4590',
  expiresAt: 1792468800, pausedReason: '',
};

test('standing allowance shows day and month use without any approval control', async () => {
  const calls = [];
  const { nodes } = fixture(async path => {
    calls.push(path);
    if (path === '/api/v1/me') return { userId: 'owner', role: 'owner' };
    return budget;
  });
  await nodes.refresh.handlers.click();
  const text = nodes.status.textContent;
  assert.match(text, /\$1\.40 of the \$10 outgoing allowance used, \$8\.60 remaining/);
  assert.match(text, /\$12\.60915 of the \$100 cap used, \$87\.39085 remaining, including incoming calls and the fixed \$8 base/);
  assert.match(text, /1 recent call still count at the reserved amount until the measured cost arrives/);
  assert.match(text, /Each phone conversation lasts at most 10 minutes and reserves up to \$10 \(its per-call cost cap/);
  assert.doesNotMatch(text, /180|three minutes|reserves \$1\.40/);
  assert.match(text, /No approval is needed/);
  assert.doesNotMatch(text, /\$5|authoriz|approve/i);
  assert.ok(text.includes(new Date(budget.expiresAt * 1000).toLocaleString(undefined, {
    timeZone: budget.timezone, dateStyle: 'medium', timeStyle: 'short',
  })));
  assert.deepEqual(calls, ['/api/v1/me', '/api/v1/phone/budget']);
});

test('a backend without the per-call fields still reads 10 minutes and $10', async () => {
  const { perCallCapUSD, perCallMaxSeconds, telephoneReserveUSD, ...older } = budget;
  const { nodes } = fixture(async path => path === '/api/v1/me' ? { userId: 'owner', role: 'owner' } : older);
  await nodes.refresh.handlers.click();
  const text = nodes.status.textContent;
  assert.match(text, /Each phone conversation lasts at most 10 minutes and reserves up to \$10 \(its per-call cost cap/);
  assert.doesNotMatch(text, /NaN|undefined|null/);
});

test('cap and allowance holds are explained in words', async () => {
  for (const [reason, pattern] of [
    ['monthly_cap_reached', /This month's \$100 cap is reached\. Calls resume next month\./],
    ['daily_allowance_used', /Today's \$10 outgoing allowance is used\. Outgoing calls resume after the reset\./],
    ['voice_stop_or_active_call', /Calls are stopped, or another call is still active/],
  ]) {
    const { nodes } = fixture(async path => path === '/api/v1/me' ? { userId: 'owner', role: 'owner' } : { ...budget, pausedReason: reason });
    await nodes.refresh.handlers.click();
    assert.match(nodes.status.textContent, pattern);
    assert.doesNotMatch(nodes.status.textContent, new RegExp(reason));
  }
});

test('incoming daily allowance is reported separately', async () => {
  const { nodes } = fixture(async path => path === '/api/v1/me' ? { userId: 'owner', role: 'owner' } : {
    ...budget, incomingDailyBudget: { enabled: true, limitUSD: '10', reservedUSD: '9.80', remainingUSD: '0.20', day: budget.day, timezone: budget.timezone },
  });
  await nodes.refresh.handlers.click();
  assert.match(nodes.status.textContent, /own \$10 daily allowance: \$9\.80 used, \$0\.20 remaining/);
  assert.match(nodes.status.textContent, /no PIN or keypad response is needed/);
});

test('nonowner cannot load the phone budget', async () => {
  let requests = 0;
  const { root, nodes } = fixture(async () => { requests++; return { role: 'member' }; });
  await nodes.refresh.handlers.click();
  assert.equal(requests, 1);
  assert.equal(root.hidden, true);
});

test('auth loss hides the panel and suppresses an in-flight response', async () => {
  let finish;
  const apiJSON = async path => {
    if (path === '/api/v1/me') return { userId: 'owner', role: 'owner' };
    return new Promise(resolve => { finish = resolve; });
  };
  const { root, nodes, loseAuth } = fixture(apiJSON);
  const loading = nodes.refresh.handlers.click();
  await new Promise(resolve => setImmediate(resolve));
  loseAuth(); finish(budget); await loading;
  assert.equal(root.hidden, true);
  assert.equal(nodes.refresh.disabled, true);
  assert.match(nodes.status.textContent, /Sign-in changed/);
});
