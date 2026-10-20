// Read-only phone budget view. The standing allowance (owner decision
// 2026-10-19) needs no approval: outgoing calls may reserve up to $10 per
// local day, and every call this month shares the $100 cap. Each conversation
// is capped at $10 of cost and 10 minutes (owner decision 2026-10-19).
export function initPhoneBudget(root, { apiJSON, onAuthLost = () => {} }) {
  if (!root) return;
  const status = root.querySelector('[data-budget-status]');
  const refresh = root.querySelector('[data-budget-refresh]');
  let busy = false, invalidated = false;
  onAuthLost(() => {
    invalidated = true;
    status.textContent = 'Sign-in changed. Reload this page before reviewing the phone budget.';
    refresh.disabled = true; root.hidden = true;
  });
  function render(data) {
    const resets = new Date(Number(data.expiresAt) * 1000).toLocaleString(undefined, {
      timeZone: data.timezone, dateStyle: 'medium', timeStyle: 'short',
    });
    const hold = {
      disabled: 'Calling is disabled.',
      voice_stop_or_active_call: 'Calls are stopped, or another call is still active.',
      monthly_cap_reached: `This month's $${data.monthlyCapUSD} cap is reached. Calls resume next month.`,
      daily_allowance_used: `Today's $${data.dailyAllowanceUSD} outgoing allowance is used. Outgoing calls resume after the reset.`,
      '': 'No call hold was reported.',
    }[data.pausedReason || ''] || 'Calling is on hold. Check the call status before another attempt.';
    const incoming = data.incomingDailyBudget;
    const incomingText = incoming?.enabled ?
      ` Incoming calls from your designated number have their own $${incoming.limitUSD} daily allowance: $${incoming.reservedUSD} used, $${incoming.remainingUSD} remaining for ${incoming.day}. Recognized owner calls connect and record automatically; no PIN or keypad response is needed.` : '';
    const pending = Number(data.unsettledCalls) > 0 ?
      ` ${data.unsettledCalls} recent call${Number(data.unsettledCalls) === 1 ? '' : 's'} still count at the reserved amount until the measured cost arrives.` : '';
    // A backend that predates these fields (staggered deploy) means the owner
    // decision's fixed limits: 600 s and $10 per call.
    const maxMinutes = Math.round((Number(data.perCallMaxSeconds) || 600) / 60);
    const perCallCap = data.perCallCapUSD || '10';
    status.textContent =
      `Today ${data.day} (${data.timezone}): $${data.dailyReservedUSD} of the $${data.dailyAllowanceUSD} outgoing allowance used, $${data.dailyRemainingUSD} remaining. Resets ${resets}. ` +
      `Month ${data.month}: $${data.allocatedUSD} of the $${data.monthlyCapUSD} cap used, $${data.monthlyRemainingUSD} remaining, including incoming calls and the fixed $8 base.` +
      incomingText + pending +
      ` Each phone conversation lasts at most ${maxMinutes} minutes and reserves up to $${perCallCap} (its per-call cost cap, never more than the remaining day and month allowance) until its measured cost arrives.` +
      ` ${hold} No approval is needed.`;
  }
  async function load() {
    if (busy || invalidated) return;
    busy = true; refresh.disabled = true;
    try {
      const me = await apiJSON('/api/v1/me');
      if (invalidated) return;
      if (me.role !== 'owner') { root.hidden = true; return; }
      const data = await apiJSON('/api/v1/phone/budget');
      if (invalidated) return;
      render(data);
    } catch {
      if (invalidated) return;
      status.textContent = 'Phone budget could not be loaded. Refresh to check it. Calls remain under their current hold.';
    } finally { busy = false; refresh.disabled = invalidated; }
  }
  refresh.addEventListener('click', load);
}
