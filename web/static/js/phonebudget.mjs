// Human-only grant control. No call or model tool is invoked here.
export function initPhoneBudget(root, { apiJSON, onAuthLost = () => {}, storage = sessionStorage, newId = () => crypto.randomUUID() }) {
  if (!root) return;
  const status = root.querySelector('[data-budget-status]');
  const refresh = root.querySelector('[data-budget-refresh]');
  const grant = root.querySelector('[data-budget-grant]');
  let day = '', key = '', pending = null, busy = false, invalidated = false;
  onAuthLost(() => {
    invalidated = true; day = ''; key = ''; pending = null;
    // Keep the owner-scoped stored request for an exact retry after fresh sign-in.
    status.textContent = 'Sign-in changed. Reload this page before reviewing the phone budget.';
    grant.disabled = true; refresh.disabled = true; root.hidden = true;
  });
  function render(data) {
    day = data.day;
    const expiry = new Date(Number(data.expiresAt) * 1000).toLocaleString(undefined, {
      timeZone: data.timezone, dateStyle: 'medium', timeStyle: 'short',
    });
    const hold = {
      disabled: 'Calling is disabled.',
      voice_stop_or_active_call: 'Calls are stopped, or another call is still active.',
      waiting_approval: 'More budget authorization is required before another call.',
      '': 'No call hold was reported.',
    }[data.pausedReason || ''] || 'Calling is on hold. Check the call status before another attempt.';
    status.textContent = `Month ${data.month}: $${data.allocatedUSD} allocated; monthly limit $${data.monthlyLimitUSD}. ` +
      `Today ${data.day} (${data.timezone}): $${data.dailyGrantedUSD} authorized, $${data.dailySpentUSD} used, $${data.dailyRemainingUSD} remaining. ` +
      `Expires ${expiry} (${data.timezone}). ${hold} A grant does not resume calls.` +
      (data.requiredReservationUSD ? ` A full phone conversation reserves $${data.requiredReservationUSD}; you can approve another $5 when the remaining allowance cannot cover it.` : '');
    grant.textContent = pending ? 'Retry the same $5 authorization' : 'Authorize $5 for this day';
  }
  async function load() {
    if (busy || invalidated) return;
    busy = true; grant.disabled = true; refresh.disabled = true;
    try {
      const me = await apiJSON('/api/v1/me');
      if (invalidated) return;
      if (me.role !== 'owner') { root.hidden = true; return; }
      key = `phone-budget-grant:${me.userId}`;
      const data = await apiJSON('/api/v1/phone/budget');
      if (invalidated) return;
      pending = JSON.parse(storage.getItem(key) || 'null');
      // A previous day's unknown result cannot authorize today's spending.
      if (pending && pending.expectedDay !== data.day) { storage.removeItem(key); pending = null; }
      render(data); grant.disabled = false;
    } catch {
      if (invalidated) return;
      status.textContent = 'Phone budget could not be loaded. Refresh to check it. Calls remain under their current hold.';
    } finally { busy = false; refresh.disabled = invalidated; }
  }
  grant.addEventListener('click', async () => {
    if (busy || invalidated || !day || !key) return;
    busy = true; grant.disabled = true; refresh.disabled = true;
    try {
      if (!pending) {
        pending = { requestId: newId(), expectedDay: day };
      }
      // Persist before every submission, including retry after a storage error.
      // A lost response or reload must retain the same ledger identity.
      storage.setItem(key, JSON.stringify(pending));
      const data = await apiJSON('/api/v1/phone/budget/grant', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(pending),
      });
      if (invalidated) return;
      storage.removeItem(key); pending = null;
      render(data);
      status.textContent += ' The $5 authorization was confirmed. Refresh before authorizing another $5.';
      // Keep disabled after success; a double click cannot create a second grant.
    } catch {
      if (invalidated) return;
      status.textContent = 'Authorization was not confirmed. Retry the same $5 authorization to check its result. Calls are not resumed.';
      grant.textContent = 'Retry the same $5 authorization'; grant.disabled = false;
    } finally { busy = false; refresh.disabled = invalidated; }
  });
  refresh.addEventListener('click', load);
}
