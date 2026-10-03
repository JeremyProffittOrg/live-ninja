// Jobs uses the same authenticated/CSRF-protected client as Memory.
// User-authored values only enter the DOM through textContent/value.
import { apiJSON } from './toolclient.mjs';

const LABELS = { active: 'Active', paused: 'Paused', cancelled: 'Cancelled', queued: 'Queued', running: 'Running', waiting_approval: 'Needs review', succeeded: 'Completed', failed: 'Failed' };
const DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
export const statusLabel = (status) => LABELS[status] || String(status || 'Unknown');

export function inTimezone(value, timezone) {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return '';
  const parts = new Intl.DateTimeFormat('en-CA', { timeZone: timezone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).formatToParts(date);
  const p = Object.fromEntries(parts.map(({ type, value: v }) => [type, v]));
  return `${p.year}-${p.month}-${p.day}T${p.hour}:${p.minute}`;
}

// Reject nonexistent and ambiguous local times rather than silently moving
// the user's commitment across a DST transition. Recurrence is server-owned.
export function localTimeToISO(local, timezone) {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(local)) throw new Error('Choose a date and time.');
  const nominal = Date.parse(`${local}:00Z`);
  if (!Number.isFinite(nominal) || new Date(nominal).toISOString().slice(0, 16) !== local) throw new Error('Choose a valid date and time.');
  const offsets = new Set();
  for (const hours of [-36, -12, 0, 12, 36]) {
    const instant = nominal + hours * 3600000;
    offsets.add(Date.parse(`${inTimezone(instant, timezone)}:00Z`) - instant);
  }
  const matches = [...offsets].map((offset) => nominal - offset).filter((instant) => inTimezone(instant, timezone) === local);
  if (!matches.length) throw new Error('This time does not exist in that timezone because the clock changes. Choose another time.');
  if (matches.length > 1) throw new Error('This time occurs twice when the clock changes. Choose an unambiguous time, or use UTC.');
  return new Date(matches[0]).toISOString();
}

export function buildSchedule({ kind, timezone, at, time, weekday }, now = Date.now()) {
  const tz = String(timezone || '').trim();
  try { new Intl.DateTimeFormat('en', { timeZone: tz }).format(); } catch { throw new Error('Enter a valid IANA timezone, such as America/New_York.'); }
  if (!tz) throw new Error('Choose a timezone.');
  if (kind === 'manual') return { kind: 'once', timezone: tz };
  if (kind === 'once') {
    const iso = localTimeToISO(at, tz);
    if (Date.parse(iso) <= now) throw new Error('Choose a time in the future, or choose Manual only.');
    return { kind, timezone: tz, at: iso };
  }
  if (!['daily', 'weekdays', 'weekly'].includes(kind)) throw new Error('Choose a supported schedule.');
  if (!/^([01]\d|2[0-3]):[0-5]\d$/.test(time || '')) throw new Error('Choose a valid time.');
  const schedule = { kind, timezone: tz, time };
  if (kind === 'weekly') {
    const day = Number(weekday);
    if (!Number.isInteger(day) || day < 0 || day > 6) throw new Error('Choose a day of the week.');
    schedule.weekday = day;
  }
  return schedule;
}

export function scheduleLabel(schedule = {}) {
  if (schedule.kind === 'once') return schedule.at ? `Once · ${dateLabel(schedule.at, schedule.timezone)} · ${schedule.timezone || 'UTC'}` : 'Manual only';
  const label = { daily: 'Every day', weekdays: 'Weekdays', weekly: `Every ${DAYS[schedule.weekday ?? 0] || 'week'}` }[schedule.kind] || 'Unknown schedule';
  return `${label} at ${schedule.time || '—'} · ${schedule.timezone || 'UTC'}`;
}

export function dateLabel(value, timezone) {
  const date = new Date(value);
  if (!value || !Number.isFinite(date.getTime()) || date.getUTCFullYear() < 2000) return 'Not scheduled';
  try { return new Intl.DateTimeFormat([], { dateStyle: 'medium', timeStyle: 'short', ...(timezone ? { timeZone: timezone } : {}) }).format(date); }
  catch { return date.toISOString(); }
}

export function errorMessage(error) {
  if (error?.status === 409) return 'This job changed, or this action is no longer available. Refresh and review the latest version before trying again.';
  if (error?.status === 401 || error?.name === 'AuthLostError') return 'Your session expired. Sign in again to continue.';
  if (error?.status === 503) return 'Jobs are temporarily unavailable. Your input is kept here; try again shortly.';
  return error?.message || 'The request could not be confirmed. Refresh to check its outcome before trying again.';
}

// Retain an idempotency key while an outcome is unknown. The same action
// and exact payload must retry with the same key; an edited payload gets a new one.
export function createRequestKeys(randomUUID = () => globalThis.crypto.randomUUID()) {
  const keys = new Map();
  return {
    get(action, payload) { const key = `${action}:${JSON.stringify(payload)}`; if (!keys.has(key)) keys.set(key, randomUUID()); return keys.get(key); },
    clear(action, payload) { keys.delete(`${action}:${JSON.stringify(payload)}`); },
  };
}

if (typeof document !== 'undefined' && document.getElementById('jobsList')) initJobs();

function initJobs() {
  const $ = (id) => document.getElementById(id);
  const root = '/api/v1/jobs';
  const state = { jobs: [], selected: '', runs: [], runsLoaded: false, runPagesExpanded: false, jobPagesExpanded: false, runError: '', capabilities: null, cursor: '', runCursor: '', listSeq: 0, runSeq: 0, busy: false, loading: false, reloadNeeded: false, lastRefresh: 0, editorJob: null, editorVersion: 0, editing: false, saving: false, confirm: null };
  const keys = createRequestKeys();
  let detailSignature = ''; let runsSignature = '';
  const expandedRuns = new Set();
  const el = (tag, cls, content) => { const node = document.createElement(tag); if (cls) node.className = cls; if (content !== undefined) node.textContent = String(content); return node; };
  const badge = (status) => el('span', `jobs-badge jobs-badge--${Object.hasOwn(LABELS, status) ? status : 'unknown'}`, statusLabel(status));
  const actionButton = (label, action, cls = 'ln-btn--ghost') => { const b = el('button', `ln-btn ${cls}`, label); b.type = 'button'; b.disabled = state.busy; b.addEventListener('click', action); return b; };
  const current = () => state.jobs.find((job) => job.id === state.selected);
  const jobPath = (id) => `${root}/${encodeURIComponent(id)}`;
  const runPath = (job, run) => `${jobPath(job.id)}/runs/${encodeURIComponent(run.id)}`;
  const more = actionButton('Load more jobs', () => loadJobs(true));
  more.id = 'jobsMore'; more.hidden = true; $('jobsList').after(more);
  function notice(message, isError = false) { $('jobsNotice').textContent = message; $('jobsNotice').hidden = !message; $('jobsNotice').dataset.error = String(isError); }

  function setCapabilities(caps) {
    state.capabilities = caps;
    const available = Boolean(caps?.reminder || caps?.review);
    $('jobNew').disabled = !available; $('jobsEmptyNew').disabled = !available;
    $('jobsCapability').textContent = !available ? 'Jobs unavailable' : caps.scheduling ? 'Reminders & human review · Scheduling available' : 'Manual jobs available · Automatic scheduling unavailable';
    const providers = Array.isArray(caps?.providers) ? caps.providers : [];
    $('jobsCapabilityDetails').textContent = [caps?.scheduling ? 'Scheduled runs are enabled. Times follow the timezone you choose.' : 'Automatic scheduling is not enabled on this server. Use Run now for a manual job.', ...providers.map((p) => `${p.label || p.id}: ${p.available ? 'available' : 'unavailable'}${p.reason ? ` — ${p.reason}` : ''}.`)].join(' ');
    for (const option of $('jobKind').options) option.disabled = !caps?.[option.value];
    for (const option of $('jobSchedule').options) option.disabled = option.value !== 'manual' && !caps?.scheduling;
  }

  async function loadJobs(append = false, quiet = false) {
    if (state.loading) { state.reloadNeeded = true; return; }
    state.loading = true;
    const seq = ++state.listSeq;
    $('jobsRefresh').disabled = true; more.disabled = true;
    if (!quiet) { $('jobsLoading').hidden = false; $('jobsError').hidden = true; }
    try {
      const preservePages = quiet && state.jobPagesExpanded;
      const data = preservePages ? null : await apiJSON(`${root}${append && state.cursor ? `?cursor=${encodeURIComponent(state.cursor)}` : ''}`);
      if (seq !== state.listSeq) return;
      const incoming = Array.isArray(data?.jobs) ? data.jobs : [];
      if (!preservePages) {
        state.jobs = append ? [...new Map([...state.jobs, ...incoming].map((j) => [j.id, j])).values()] : incoming;
        state.cursor = data?.nextCursor || ''; state.jobPagesExpanded = append;
      }
      // A selected/just-created job may not be in the first query page.
      // Keep it addressable across refreshes rather than losing its detail.
      if (state.selected && (!current() || preservePages)) {
        try { const selected = await apiJSON(jobPath(state.selected)); if (selected?.job) state.jobs = [...new Map([...state.jobs, selected.job].map((j) => [j.id, j])).values()]; }
        catch (error) { if (error?.status === 404) state.selected = ''; else throw error; }
      }
      if (data) setCapabilities(data.capabilities || {});
      $('jobsError').hidden = true;
      renderList();
      if (state.selected && current()) { renderDetail(); if (!quiet || !state.runPagesExpanded) await loadRuns(); }
      else if (state.selected) { state.selected = ''; renderDetail(); }
    } catch (error) {
      if (!quiet || !state.jobs.length) { $('jobsError').hidden = false; $('jobsErrorText').textContent = errorMessage(error); }
      else notice(`Could not refresh. Displayed information may be out of date. ${errorMessage(error)}`, true);
    } finally {
      if (seq === state.listSeq) {
        state.loading = false; state.lastRefresh = Date.now(); $('jobsLoading').hidden = true; $('jobsRefresh').disabled = false; more.disabled = false;
        if (state.reloadNeeded) { state.reloadNeeded = false; queueMicrotask(() => loadJobs(false, true)); }
      }
    }
  }

  function renderList() {
    const focusedJob = document.activeElement?.closest('.jobs-card')?.dataset.jobId;
    const filter = $('jobsFilter').value;
    const jobs = state.jobs.filter((job) => filter === 'all' || job.status === filter);
    $('jobsActive').textContent = state.jobs.filter((job) => job.status === 'active').length;
    $('jobsPaused').textContent = state.jobs.filter((job) => job.status === 'paused').length;
    $('jobsTotal').textContent = `${state.jobs.length}${state.cursor ? '+' : ''}`;
    $('jobsActiveLabel').textContent = state.cursor ? 'Active shown' : 'Active';
    $('jobsPausedLabel').textContent = state.cursor ? 'Paused shown' : 'Paused';
    $('jobsList').replaceChildren();
    for (const job of jobs) {
      const item = el('li'); const card = el('button', 'jobs-card'); card.type = 'button'; card.dataset.jobId = job.id;
      card.setAttribute('aria-current', String(job.id === state.selected));
      const top = el('span', 'jobs-card-top'); top.append(el('span', 'jobs-card-title', job.title), badge(job.status)); card.append(top);
      card.append(el('span', 'jobs-card-meta', job.kind === 'review' ? 'Human review' : 'Reminder'));
      card.append(el('span', 'jobs-card-meta', scheduleLabel(job.schedule)));
      card.append(el('span', 'jobs-card-meta', job.status !== 'active' ? 'No future runs while inactive' : job.nextRunAt ? `Next: ${dateLabel(job.nextRunAt, job.schedule?.timezone)}` : 'Run when you are ready'));
      if (job.lastRun) card.append(el('span', 'jobs-card-meta', `Latest run: ${statusLabel(job.lastRun.status)}`));
      if (job.error) card.append(el('span', 'jobs-card-meta jobs-error-text', job.error));
      card.addEventListener('click', () => selectJob(job.id)); item.append(card); $('jobsList').append(item);
    }
    $('jobsEmpty').hidden = jobs.length > 0;
    const filtered = state.jobs.length > 0;
    $('jobsEmptyTitle').textContent = filtered ? 'No jobs in this view' : 'Make room for what matters';
    $('jobsEmptyText').textContent = filtered ? (state.cursor ? 'No matching jobs in the loaded pages. Load more jobs, or choose another status.' : 'Choose another status to see your other jobs.') : 'Create a reminder or a review checkpoint. Each run keeps a receipt here.';
    $('jobsEmptyNew').hidden = filtered;
    more.hidden = !state.cursor;
    if (focusedJob) [...$('jobsList').querySelectorAll('.jobs-card')].find((card) => card.dataset.jobId === focusedJob)?.focus({ preventScroll: true });
  }

  async function selectJob(id) {
    state.selected = id; state.runs = []; state.runCursor = ''; state.runsLoaded = false; state.runPagesExpanded = false; state.runError = ''; expandedRuns.clear();
    renderList(); renderDetail();
    $('jobDetail').focus({ preventScroll: true });
    if (globalThis.matchMedia('(max-width:700px)').matches) $('jobDetail').scrollIntoView({ behavior: 'instant', block: 'start' });
    await loadRuns();
  }

  function fact(list, label, value) { const wrapper = el('div'); wrapper.append(el('dt', '', label), el('dd', '', value)); list.append(wrapper); }
  function renderDetail() {
    const job = current(); const content = $('jobContent');
    const signature = JSON.stringify([job, state.busy, state.capabilities?.historyLimit]);
    if (signature === detailSignature) return;
    detailSignature = signature;
    const focusedAction = content.contains(document.activeElement) ? document.activeElement?.textContent : null;
    const focusedRun = document.activeElement?.closest('.jobs-run')?.dataset.runId;
    $('jobPlaceholder').hidden = Boolean(job); content.hidden = !job;
    content.replaceChildren();
    if (!job) { $('jobDetail').setAttribute('aria-labelledby', 'jobDetailHeading'); return; }
    $('jobDetail').setAttribute('aria-labelledby', 'jobSelectedTitle');
    const head = el('div', 'jobs-detail-head'); const title = el('div', 'jobs-detail-title');
    title.append(badge(job.status)); const heading = el('h2', '', job.title); heading.id = 'jobSelectedTitle'; title.append(heading); head.append(title);
    if (job.status !== 'cancelled') head.append(actionButton('Edit job', () => openEditor(job)));
    content.append(head, el('p', 'jobs-detail-notes', job.instructions));
    if (job.error) content.append(el('p', 'jobs-form-error', job.error));
    const facts = el('dl', 'jobs-facts');
    fact(facts, 'Type', job.kind === 'review' ? 'Human review checkpoint' : 'In-app reminder');
    fact(facts, 'Schedule', scheduleLabel(job.schedule));
    fact(facts, 'Next run', job.status === 'active' ? (job.nextRunAt ? `${dateLabel(job.nextRunAt, job.schedule?.timezone)} · ${job.schedule?.timezone || 'UTC'}` : 'Manual only / no future occurrence') : 'None while inactive');
    fact(facts, 'Last updated', dateLabel(job.updatedAt)); content.append(facts);
    const actions = el('div', 'jobs-actions');
    if (job.status === 'active') {
      actions.append(actionButton('Run now', () => mutate(job, 'run'), 'ln-btn--primary'));
      actions.append(actionButton('Pause', () => mutate(job, 'pause')));
    } else if (job.status === 'paused') actions.append(actionButton('Resume', () => mutate(job, 'resume'), 'ln-btn--primary'));
    if (job.status !== 'cancelled') actions.append(actionButton('Cancel job', () => confirmAction('Cancel this job?', 'Stop future runs and cancel pending checkpoints for this job. Its run history remains available.', () => mutate(job, 'cancel', null, true)), 'ln-btn--danger'));
    content.append(actions);
    content.append(el('h3', 'jobs-runs-heading', 'Run history'));
    content.append(el('p', 'jobs-runs-intro', `Each run records the job as it was at that time. Up to ${state.capabilities?.historyLimit || 50} recent runs are retained; long receipts can reduce that number. Review approval only completes a human checkpoint.`));
    const runState = el('p', 'ln-muted', 'Loading run history…'); runState.id = 'jobsRunState'; content.append(runState);
    const runs = el('ol', 'jobs-runs'); runs.id = 'jobsRuns'; content.append(runs);
    renderRuns(state.runsLoaded, true);
    if (focusedAction) {
      const replacement = [...content.querySelectorAll('button, summary')].find((button) => button.textContent === focusedAction && (!focusedRun || button.closest('.jobs-run')?.dataset.runId === focusedRun));
      if (replacement && !replacement.disabled) replacement.focus({ preventScroll: true });
      else $('jobDetail').focus({ preventScroll: true });
    }
  }

  async function loadRuns(append = false) {
    const job = current(); if (!job) return;
    const seq = ++state.runSeq;
    try {
      const data = await apiJSON(`${jobPath(job.id)}/runs${append && state.runCursor ? `?cursor=${encodeURIComponent(state.runCursor)}` : ''}`);
      if (seq !== state.runSeq || state.selected !== job.id) return;
      const runs = Array.isArray(data?.runs) ? data.runs : [];
      state.runs = append ? [...new Map([...state.runs, ...runs].map((run) => [run.id, run])).values()] : runs;
      state.runCursor = data?.nextCursor || '';
      state.runsLoaded = true; state.runPagesExpanded = append; state.runError = '';
      renderRuns(true);
    } catch (error) {
      if (seq !== state.runSeq || state.selected !== job.id) return;
      state.runError = `Could not load run history. ${errorMessage(error)}`;
      renderRuns(state.runsLoaded);
    }
  }

  function renderRuns(loaded = false, force = false) {
    const list = $('jobsRuns'); if (!list) return;
    const signature = JSON.stringify([state.selected, state.runs, state.runCursor, state.busy, loaded, state.runError]);
    if (!force && signature === runsSignature) return;
    runsSignature = signature;
    const focusedRun = document.activeElement?.closest('.jobs-run')?.dataset.runId;
    const focusedLabel = list.contains(document.activeElement) ? document.activeElement.textContent : null;
    list.replaceChildren(); const job = current();
    const status = $('jobsRunState'); status.hidden = state.runs.length > 0;
    if (loaded) status.textContent = 'No runs yet. Run this job now, or wait for its next scheduled time.';
    if (state.runPagesExpanded) { status.hidden = false; status.textContent = 'Older runs are loaded. Use Refresh to include new runs.'; }
    if (state.runError) { status.hidden = false; status.textContent = state.runError; }
    for (const run of state.runs) {
      const row = el('li', 'jobs-run'); row.dataset.runId = run.id;
      const head = el('div', 'jobs-run-head'); head.append(badge(run.status));
      const date = el('time', '', dateLabel(run.createdAt || run.scheduledFor)); if (run.createdAt) date.dateTime = run.createdAt; head.append(date); row.append(head);
      if (run.progress !== undefined && run.progress !== null) {
        if (typeof run.progress === 'number') { const progress = el('progress'); progress.max = 100; progress.value = Math.min(100, Math.max(0, run.progress)); progress.setAttribute('aria-label', `Run progress: ${progress.value}%`); row.append(progress); }
        else row.append(el('p', '', typeof run.progress === 'string' ? run.progress : JSON.stringify(run.progress)));
      }
      if (run.status === 'waiting_approval' && !run.progress) row.append(el('p', '', 'Waiting for your review. No external action will be taken.'));
      if (run.result) row.append(el('pre', 'jobs-receipt', typeof run.result === 'string' ? run.result : JSON.stringify(run.result, null, 2)));
      if (run.error) row.append(el('p', 'jobs-form-error', typeof run.error === 'string' ? run.error : JSON.stringify(run.error)));
      const details = el('details'); details.append(el('summary', '', `Run details · attempt ${run.attempt || 1}`));
      details.open = expandedRuns.has(run.id);
      details.addEventListener('toggle', () => { if (details.isConnected) { if (details.open) expandedRuns.add(run.id); else expandedRuns.delete(run.id); } });
      details.append(el('pre', 'jobs-receipt', [`Job: ${run.title || job.title}`, run.instructions || '', `Run ID: ${run.id}`, `Created: ${dateLabel(run.createdAt)}`, ...(run.finishedAt ? [`Finished: ${dateLabel(run.finishedAt)}`] : []), ...(run.approvalExpiresAt ? [`Review expires: ${dateLabel(run.approvalExpiresAt)}`] : []), ...(run.retryOf ? [`Retry of: ${run.retryOf}`] : [])].join('\n'))); row.append(details);
      const actions = el('div', 'jobs-actions');
      if (run.status === 'waiting_approval' && job.status !== 'cancelled') actions.append(actionButton('Review & approve', () => confirmAction('Complete this review?', 'Approve the exact checkpoint below. This records your review; it does not send messages or execute an external task.', () => mutate(job, 'approve', run, true), `${run.title || job.title}\n\n${run.instructions || ''}\n\nRun: ${run.id}`), 'ln-btn--primary'));
      if (['failed', 'cancelled'].includes(run.status) && job.status === 'active' && (run.attempt || 1) < 3) actions.append(actionButton('Retry run', () => mutate(job, 'retry', run)));
      if (['queued', 'waiting_approval'].includes(run.status)) actions.append(actionButton('Cancel run', () => confirmAction('Cancel this run?', 'Cancel this occurrence. The job’s future schedule stays as it is.', () => mutate(job, 'cancel', run, true)), 'ln-btn--danger'));
      if (actions.childNodes.length) row.append(actions);
      list.append(row);
    }
    if (state.runCursor) { const li = el('li'); li.append(actionButton('Load older runs', () => loadRuns(true))); list.append(li); }
    if (focusedRun && focusedLabel) {
      [...list.querySelectorAll('button, summary')].find((node) => node.closest('.jobs-run')?.dataset.runId === focusedRun && node.textContent === focusedLabel)?.focus({ preventScroll: true });
    }
  }

  function confirmAction(title, description, callback, details = '') {
    if (state.busy) return;
    state.confirm = callback; $('jobConfirmTitle').textContent = title; $('jobConfirmText').textContent = description;
    $('jobConfirmDetails').hidden = !details; $('jobConfirmDetails').textContent = details;
    $('jobConfirmError').hidden = true; $('jobConfirmYes').textContent = title.startsWith('Complete') ? 'Approve this review' : 'Confirm cancellation';
    $('jobConfirm').showModal(); $('jobConfirmBack').focus();
  }

  async function mutate(job, action, run = null, inDialog = false) {
    if (state.busy) return false;
    state.busy = true;
    const path = `${run ? runPath(job, run) : jobPath(job.id)}/${action}`;
    const payload = { expectedVersion: job.version };
    const requestId = keys.get(path, payload);
    $('jobConfirmYes').disabled = true; $('jobConfirmBack').disabled = true;
    renderDetail();
    try {
      const data = await apiJSON(path, { method: 'POST', json: { ...payload, requestId } });
      keys.clear(path, payload);
      if (data?.job) state.jobs = state.jobs.map((j) => j.id === data.job.id ? data.job : j);
      if (inDialog) $('jobConfirm').close();
      const receipt = data?.run;
      notice(receipt ? `Run ${statusLabel(receipt.status).toLowerCase()}. Open its receipt below for the recorded outcome.` : ({ pause: 'Job paused. Pending reviews were cancelled.', resume: 'Job resumed.', cancel: 'Job cancelled. Its history is retained.' }[action] || 'Job updated.'));
      await loadJobs();
      return true;
    } catch (error) {
      const msg = errorMessage(error);
      if (inDialog) { $('jobConfirmError').textContent = msg; $('jobConfirmError').hidden = false; }
      else notice(msg, true);
      if (error?.status === 409) await loadJobs(false, true);
      return false;
    } finally {
      state.busy = false; $('jobConfirmYes').disabled = false; $('jobConfirmBack').disabled = false; renderList(); renderDetail();
    }
  }

  const detectedZone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  const zones = typeof Intl.supportedValuesOf === 'function' ? Intl.supportedValuesOf('timeZone') : ['America/Los_Angeles', 'America/Denver', 'America/Chicago', 'America/New_York', 'Europe/London', 'Europe/Paris', 'Asia/Tokyo', 'Australia/Sydney'];
  for (const zone of new Set(['UTC', detectedZone, ...zones])) { const option = el('option'); option.value = zone; $('jobTimezones').append(option); }

  function updateScheduleFields() {
    const kind = $('jobSchedule').value;
    $('jobOnceField').hidden = kind !== 'once'; $('jobAt').disabled = kind !== 'once'; $('jobAt').required = kind === 'once';
    const recurring = ['daily', 'weekdays', 'weekly'].includes(kind);
    $('jobTimeField').hidden = !recurring; $('jobTime').disabled = !recurring; $('jobTime').required = recurring;
    $('jobWeekdayField').hidden = kind !== 'weekly'; $('jobWeekday').disabled = kind !== 'weekly';
  }
  function updateKindHint() { $('jobKindHint').textContent = $('jobKind').value === 'review' ? 'Waits for your explicit approval of a human checkpoint. Approval does not execute external work.' : 'Creates an in-app reminder receipt when it runs.'; }

  function openEditor(job = null) {
    if (state.busy || state.saving) return;
    state.editorJob = job ? { ...job } : null; state.editorVersion = job?.version || 0; state.editing = Boolean(job);
    $('jobEditorTitle').textContent = job ? 'Edit job' : 'New job'; $('jobSave').textContent = job ? 'Save changes' : 'Create job';
    $('jobTitle').value = job?.title || ''; $('jobInstructions').value = job?.instructions || '';
    $('jobKind').value = job?.kind || (state.capabilities?.reminder ? 'reminder' : 'review');
    const schedule = job?.schedule || {};
    $('jobTimezone').value = schedule.timezone || detectedZone;
    $('jobSchedule').value = schedule.kind ? (schedule.kind === 'once' && !schedule.at ? 'manual' : schedule.kind) : 'manual';
    $('jobAt').value = inTimezone(schedule.at || Date.now() + 3600000, $('jobTimezone').value);
    $('jobTime').value = schedule.time || '09:00'; $('jobWeekday').value = String(schedule.weekday ?? (schedule.kind === 'weekly' ? 0 : 1));
    $('jobFormError').hidden = true; $('jobReloadLatest').hidden = true; updateScheduleFields(); updateKindHint();
    if (!$('jobEditor').open) $('jobEditor').showModal(); $('jobTitle').focus();
  }

  async function saveJob(event) {
    event.preventDefault(); if (state.saving) return;
    const title = $('jobTitle').value.trim(); const instructions = $('jobInstructions').value.trim();
    $('jobFormError').hidden = true;
    let schedule;
    try {
      if (!title || !instructions) throw new Error('Enter a title and notes for this job.');
      if (new TextEncoder().encode(title).length > 160) throw new Error('Shorten the job title to 160 bytes or fewer. Some characters use more than one byte.');
      if (new TextEncoder().encode(instructions).length > 2000) throw new Error('Shorten the notes to 2,000 bytes or fewer. Some characters use more than one byte.');
      const original = state.editorJob?.schedule;
      // An already-fired one-time job can still have its notes edited. Keep
      // its exact schedule when unchanged instead of recreating an occurrence.
      const sameOnce = state.editing && original?.kind === 'once' && original.at && $('jobSchedule').value === 'once' && $('jobTimezone').value.trim() === original.timezone && $('jobAt').value === inTimezone(original.at, original.timezone);
      schedule = sameOnce ? { ...original } : buildSchedule({ kind: $('jobSchedule').value, timezone: $('jobTimezone').value, at: $('jobAt').value, time: $('jobTime').value, weekday: $('jobWeekday').value });
    } catch (error) { $('jobFormError').textContent = errorMessage(error); $('jobFormError').hidden = false; return; }
    const payload = { title, instructions, kind: $('jobKind').value, schedule, ...(state.editing ? { expectedVersion: state.editorVersion } : {}) };
    const path = state.editing ? jobPath(state.editorJob.id) : root;
    const method = state.editing ? 'PATCH' : 'POST'; const key = `${method}:${path}`;
    const requestId = keys.get(key, payload);
    state.saving = true; $('jobSave').disabled = true; $('jobSave').setAttribute('aria-busy', 'true'); $('jobEditorClose').disabled = true; $('jobEditorCancel').disabled = true;
    try {
      const data = await apiJSON(path, { method, json: { ...payload, requestId } });
      keys.clear(key, payload); $('jobEditor').close();
      if (data?.job?.id) state.selected = data.job.id;
      $('jobsFilter').value = 'all';
      notice(state.editing ? 'Changes saved. Pending reviews were cancelled; existing receipts keep their original contents.' : 'Job created. Its next run and history are shown below.');
      await loadJobs();
    } catch (error) {
      $('jobFormError').textContent = `${errorMessage(error)}${error?.status === 409 ? ' Your edits are kept. Load the latest version to replace them, or close and review the job.' : ''}`;
      $('jobFormError').hidden = false; $('jobReloadLatest').hidden = error?.status !== 409;
    } finally {
      state.saving = false; $('jobSave').disabled = false; $('jobSave').removeAttribute('aria-busy'); $('jobEditorClose').disabled = false; $('jobEditorCancel').disabled = false;
    }
  }

  $('jobNew').addEventListener('click', () => openEditor()); $('jobsEmptyNew').addEventListener('click', () => openEditor());
  $('jobsRefresh').addEventListener('click', () => loadJobs()); $('jobsTryAgain').addEventListener('click', () => loadJobs());
  $('jobsFilter').addEventListener('change', renderList); $('jobSchedule').addEventListener('change', updateScheduleFields); $('jobKind').addEventListener('change', updateKindHint);
  $('jobForm').addEventListener('submit', saveJob);
  for (const id of ['jobEditorClose', 'jobEditorCancel']) $(id).addEventListener('click', () => { if (!state.saving) $('jobEditor').close(); });
  $('jobEditor').addEventListener('cancel', (event) => { if (state.saving) event.preventDefault(); });
  $('jobConfirm').addEventListener('cancel', (event) => { if (state.busy) event.preventDefault(); });
  $('jobConfirmBack').addEventListener('click', () => { if (!state.busy) $('jobConfirm').close(); });
  $('jobConfirmYes').addEventListener('click', () => { if (!state.busy) state.confirm?.(); });
  $('jobReloadLatest').addEventListener('click', async () => {
    if (state.saving || !state.editorJob) return;
    $('jobReloadLatest').disabled = true;
    try { const data = await apiJSON(jobPath(state.editorJob.id)); if (!data?.job) throw new Error('The latest version was not returned.'); openEditor(data.job); }
    catch (error) { $('jobFormError').textContent = errorMessage(error); $('jobFormError').hidden = false; }
    finally { $('jobReloadLatest').disabled = false; }
  });
  document.addEventListener('visibilitychange', () => { if (!document.hidden && !state.busy && !state.saving && !$('jobEditor').open && !$('jobConfirm').open) loadJobs(false, true); });
  setInterval(() => {
    const pending = state.runs.some((run) => ['queued', 'running'].includes(run.status)) || state.jobs.some((job) => ['queued', 'running'].includes(job.lastRun?.status));
    if (!document.hidden && !state.busy && !state.saving && !$('jobEditor').open && !$('jobConfirm').open && (pending || Date.now() - state.lastRefresh >= 15000)) loadJobs(false, true);
  }, 3000);
  loadJobs();
}
