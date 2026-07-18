// history.mjs — /history conversation history + topic filters + Topic
// Manager controller (M11 FR-TOP-02/03/04/05, owned by the M10+M11 web-UI
// workstream).
//
// Data plane (contracts/api.md "Conversation Topics & Filterable History",
// served by the M11 API workstream under the shared /api/v1 prefix):
//   - GET  /api/v1/topics                 — the user's topic taxonomy
//     ({topics:[{topicId,name,color,archived,mergedInto?,convCount}]}).
//     Populates the filter chips AND the Topic Manager (never free text).
//   - PATCH /api/v1/topics/{id}           — rename/color/archive/merge
//     ({name?|color?|archived?|mergedInto?}); tags reference the stable
//     topicId so none of these re-tag conversations (FR-TOP-02).
//   - DELETE /api/v1/topics/{id}          — remove a topic and untag every
//     conversation that carried it (conversations themselves are kept —
//     see confirmDeleteTopic below for the confirmation copy). A topic
//     name that comes back later is a brand-new topicId.
//   - GET  /api/v1/conversations          — filterable list
//     (?topic=&device=&from=&to=&cursor=&limit=), Query-only server-side
//     (CONV#/TREF# partitions per the M11 locked item shapes).
//     Multi-topic selection = one request per selected topic, unioned
//     client-side (each topic IS its own TREF# key range server-side, so
//     N chips = N Querys — no Scan anywhere). Cursor pagination applies
//     to the 0/1-topic case; unions load a single larger page instead.
//   - GET  /api/v1/conversations/{id}     — one conversation + transcript
//     turns for the detail view.
//   - GET  /api/v1/devices                — populates the device picker
//     (plus the fixed "Web"/"Android" surface entries — conversations
//     carry the surface as deviceId for browser/phone sessions).
//
// All data-driven markup is built with textContent (never innerHTML).

import { apiJSON, ApiError } from './toolclient.mjs';

const $ = (id) => document.getElementById(id);

// ---- state ------------------------------------------------------------

let topics = []; // full taxonomy (incl. archived — chips hide archived)
let conversations = [];
let nextCursor = null;
let loadSeq = 0;

const selectedTopics = new Set(); // topicIds checked in the filter chips
let deviceFilter = '';
let fromDate = ''; // yyyy-mm-dd from the native date input
let toDate = '';

let detailConvId = null; // non-null while the detail view is open

// True while settings.privacy.storeTranscripts === false: the transcript
// sink is dropping turns client-side, so History must say so instead of
// sitting silently empty (the storage-off banner + empty-state copy).
let transcriptsOff = false;

// "Show tool calls" toggle state, remembered across visits (default off).
const SHOW_TOOLS_KEY = 'ln.history.showToolCalls';

// ---- toast ------------------------------------------------------------

const toastEl = $('toast');
const toastMsgEl = $('toastMsg');
let toastTimer = 0;

function showToast(msg, { error = false } = {}) {
  clearTimeout(toastTimer);
  toastMsgEl.textContent = msg;
  toastEl.classList.toggle('is-error', !!error);
  toastEl.hidden = false;
  requestAnimationFrame(() => toastEl.classList.add('is-visible'));
  toastTimer = setTimeout(() => {
    toastEl.classList.remove('is-visible');
    toastEl.hidden = true;
  }, 6000);
}

function apiErrorMessage(err, fallback) {
  if (err instanceof ApiError && err.body) {
    const b = err.body;
    if (typeof b.message === 'string' && b.message) return b.message;
    if (b.error && typeof b.error === 'object' && b.error.message) return b.error.message;
  }
  return fallback;
}

// ---- normalization / formatting ---------------------------------------

const SAFE_COLOR = /^#[0-9a-fA-F]{3,8}$/;

function normalizeTopic(t) {
  if (!t || typeof t !== 'object') return null;
  const id = t.topicId || t.id;
  if (!id) return null;
  return {
    id: String(id),
    name: String(t.name || id),
    color: SAFE_COLOR.test(String(t.color || '')) ? String(t.color) : '#5eead4',
    archived: t.archived === true,
    mergedInto: t.mergedInto ? String(t.mergedInto) : '',
    convCount: Number.isFinite(Number(t.convCount)) ? Number(t.convCount) : null,
  };
}

/** Follow merge pointers to the canonical topic (bounded hops). */
function canonicalTopic(id) {
  let t = topics.find((x) => x.id === id) || null;
  for (let i = 0; t && t.mergedInto && i < 5; i++) {
    const next = topics.find((x) => x.id === t.mergedInto);
    if (!next) break;
    t = next;
  }
  return t;
}

function toMs(v) {
  if (v === null || v === undefined || v === '') return 0;
  if (typeof v === 'number') return v < 1e12 ? v * 1000 : v; // epoch s → ms
  const ms = Date.parse(v);
  return Number.isFinite(ms) ? ms : 0;
}

function normalizeConv(c) {
  if (!c || typeof c !== 'object') return null;
  const id = c.conversationId || c.sessionId || c.id;
  if (!id) return null;
  const startedAt = toMs(c.startedAt ?? c.ts ?? c.createdAt);
  const rawTopics = Array.isArray(c.topicIds) ? c.topicIds : Array.isArray(c.topics) ? c.topics : [];
  return {
    id: String(id),
    startedAt,
    startedAtISO: startedAt ? new Date(startedAt).toISOString() : '',
    deviceId: String(c.deviceId || c.surface || ''),
    deviceName: String(c.deviceName || ''),
    engine: String(c.engine || ''),
    summary: String(c.summary || c.title || ''),
    topicIds: rawTopics.map((t) => String(typeof t === 'object' && t ? t.topicId || t.id || '' : t)).filter(Boolean),
    turnCount: Number.isFinite(Number(c.turnCount ?? c.turns)) ? Number(c.turnCount ?? c.turns) : null,
  };
}

const dateFmt = new Intl.DateTimeFormat([], {
  year: 'numeric', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit',
});
const fmtDate = (ms) => (ms ? dateFmt.format(new Date(ms)) : '—');

const DEVICE_LABELS = { web: 'Web', android: 'Android', browser: 'Web' };

function deviceLabel(conv) {
  if (conv.deviceName) return conv.deviceName;
  if (!conv.deviceId) return '—';
  const known = knownDevices.get(conv.deviceId);
  if (known) return known;
  return DEVICE_LABELS[conv.deviceId.toLowerCase()] || conv.deviceId;
}

// ---- topics: load + filter chips --------------------------------------

const topicChipsEl = $('topicChips');
const topicChipsNote = $('topicChipsNote');

async function loadTopics() {
  try {
    const resp = await apiJSON('/api/v1/topics');
    const raw = Array.isArray(resp.topics) ? resp.topics : Array.isArray(resp.items) ? resp.items : [];
    topics = raw.map(normalizeTopic).filter(Boolean);
    renderTopicChips();
    renderTopicManager();
  } catch (err) {
    topicChipsNote.textContent = apiErrorMessage(err, "Couldn't load topics — conversations are still listed below.");
    topicChipsNote.hidden = false;
  }
}

function renderTopicChips() {
  // Chips show live (non-archived, non-merged) topics; a selected topic
  // stays visible even if archived so an active filter is never hidden.
  const visible = topics.filter((t) => (!t.archived && !t.mergedInto) || selectedTopics.has(t.id));
  topicChipsEl.textContent = '';
  if (visible.length === 0) {
    topicChipsNote.textContent = 'No topics yet — they’re created automatically as you talk.';
    topicChipsNote.hidden = false;
    topicChipsEl.appendChild(topicChipsNote);
    return;
  }
  topicChipsNote.hidden = true;
  for (const t of visible.sort((a, b) => a.name.localeCompare(b.name))) {
    const label = document.createElement('label');
    label.className = 'hist-chip';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = t.id;
    cb.checked = selectedTopics.has(t.id);
    cb.addEventListener('change', () => {
      if (cb.checked) selectedTopics.add(t.id);
      else selectedTopics.delete(t.id);
      syncClearButtons();
      loadConversations();
    });
    const span = document.createElement('span');
    span.style.setProperty('--chip-color', t.color);
    span.textContent = t.name;
    label.appendChild(cb);
    label.appendChild(span);
    topicChipsEl.appendChild(label);
  }
}

// ---- devices: picker ---------------------------------------------------

const deviceSelect = $('deviceFilter');
const knownDevices = new Map(); // deviceId → display name

async function loadDevices() {
  // Fixed surface entries first (web/Android sessions carry the surface
  // as their device identity), then the user's registered hardware.
  const fixed = [
    ['web', 'Web'],
    ['android', 'Android'],
  ];
  for (const [v, l] of fixed) knownDevices.set(v, l);
  try {
    const resp = await apiJSON('/api/v1/devices');
    for (const d of Array.isArray(resp.devices) ? resp.devices : []) {
      if (d && d.deviceId) knownDevices.set(String(d.deviceId), String(d.name || d.deviceId));
    }
  } catch {
    // Device list failing shouldn't block history — the fixed surface
    // options still render and filtering by them still works.
  }
  const current = deviceSelect.value;
  deviceSelect.textContent = '';
  const all = document.createElement('option');
  all.value = '';
  all.textContent = 'All devices';
  deviceSelect.appendChild(all);
  for (const [value, label] of knownDevices) {
    const opt = document.createElement('option');
    opt.value = value;
    opt.textContent = label;
    deviceSelect.appendChild(opt);
  }
  deviceSelect.value = knownDevices.has(current) ? current : '';
}

// ---- transcript-storage hint ------------------------------------------

const histStorageOffEl = $('histStorageOff');

/** Best-effort settings probe: when privacy.storeTranscripts is false the
 * transcript sink drops every turn client-side, so the page shows the
 * storage-off banner (and the empty state explains itself) instead of
 * looking silently broken. A failed fetch just leaves the hint off —
 * history itself must never depend on settings loading. */
async function loadStorageHint() {
  try {
    const doc = await apiJSON('/api/v1/settings');
    transcriptsOff = !!(doc && doc.privacy && doc.privacy.storeTranscripts === false);
  } catch {
    transcriptsOff = false;
  }
  histStorageOffEl.hidden = !transcriptsOff;
  // The list may already be sitting on the generic empty state — refresh
  // its copy now that we know why it's empty.
  if (transcriptsOff && !histEmptyEl.hidden) renderConversations();
}

// ---- conversations: loading -------------------------------------------

const histLoadingEl = $('histLoading');
const histEmptyEl = $('histEmpty');
const histEmptyTitle = $('histEmptyTitle');
const histEmptyMsg = $('histEmptyMsg');
const histEmptyClear = $('histEmptyClear');
const histErrorEl = $('histError');
const histErrorMsgEl = $('histErrorMsg');
const histTableWrapEl = $('histTableWrap');
const histBodyEl = $('histBody');
const histCountEl = $('histCount');
const histMoreWrapEl = $('histMoreWrap');
const histMoreBtn = $('histMore');
const dateRangeErr = $('dateRangeErr');

function setHistState(state) {
  histLoadingEl.hidden = state !== 'loading';
  histEmptyEl.hidden = state !== 'empty';
  histErrorEl.hidden = state !== 'error';
  histTableWrapEl.hidden = state !== 'table';
}

function hasActiveFilters() {
  return selectedTopics.size > 0 || !!deviceFilter || !!fromDate || !!toDate;
}

function syncClearButtons() {
  $('clearFilters').hidden = !hasActiveFilters();
}

function rangeParams() {
  const params = new URLSearchParams();
  if (deviceFilter) params.set('device', deviceFilter);
  // Local calendar days → inclusive instant range.
  if (fromDate) params.set('from', new Date(fromDate + 'T00:00:00').toISOString());
  if (toDate) params.set('to', new Date(toDate + 'T23:59:59.999').toISOString());
  return params;
}

function extractConvs(resp) {
  const raw = Array.isArray(resp.conversations) ? resp.conversations
    : Array.isArray(resp.items) ? resp.items : [];
  return raw.map(normalizeConv).filter(Boolean);
}

async function fetchConvPage(cursor) {
  const chosen = [...selectedTopics];
  if (chosen.length <= 1) {
    const params = rangeParams();
    if (chosen.length === 1) params.set('topic', chosen[0]);
    if (cursor) params.set('cursor', cursor);
    const qs = params.toString();
    const resp = await apiJSON(`/api/v1/conversations${qs ? '?' + qs : ''}`);
    return { items: extractConvs(resp), nextCursor: resp.nextCursor || resp.cursor || null, paged: true };
  }
  // Multi-topic union: one Query-backed request per topic, merged here.
  const pages = await Promise.all(chosen.map((topicId) => {
    const params = rangeParams();
    params.set('topic', topicId);
    params.set('limit', '100');
    return apiJSON(`/api/v1/conversations?${params.toString()}`);
  }));
  const merged = [];
  const seen = new Set();
  for (const p of pages) {
    for (const c of extractConvs(p)) {
      if (!seen.has(c.id)) { seen.add(c.id); merged.push(c); }
    }
  }
  return { items: merged, nextCursor: null, paged: false };
}

async function loadConversations({ append = false } = {}) {
  if (fromDate && toDate && fromDate > toDate) {
    dateRangeErr.hidden = false;
    return;
  }
  dateRangeErr.hidden = true;

  const seq = ++loadSeq;
  if (!append) {
    setHistState('loading');
    histCountEl.textContent = '';
  } else {
    histMoreBtn.disabled = true;
    histMoreBtn.textContent = 'Loading…';
  }
  try {
    const page = await fetchConvPage(append ? nextCursor : null);
    if (seq !== loadSeq) return;
    if (append) {
      const known = new Set(conversations.map((c) => c.id));
      conversations = conversations.concat(page.items.filter((c) => !known.has(c.id)));
    } else {
      conversations = page.items;
    }
    nextCursor = page.paged ? page.nextCursor : null;
    renderConversations();
  } catch (err) {
    if (seq !== loadSeq) return;
    if (append) {
      showToast(apiErrorMessage(err, "Couldn't load more conversations — try again."), { error: true });
    } else {
      histErrorMsgEl.textContent = apiErrorMessage(err, 'Check your connection and try again.');
      setHistState('error');
    }
  } finally {
    histMoreBtn.disabled = false;
    histMoreBtn.textContent = 'Load more';
  }
}

// ---- conversations: rendering -----------------------------------------

function renderConversations() {
  if (conversations.length === 0) {
    if (hasActiveFilters()) {
      histEmptyTitle.textContent = 'No conversations match';
      histEmptyMsg.textContent = 'Nothing matches these filters — try widening the date range or clearing a topic.';
      histEmptyClear.hidden = false;
    } else if (transcriptsOff) {
      histEmptyTitle.textContent = 'Transcript storage is off';
      histEmptyMsg.textContent = 'Conversations aren’t being saved because transcript storage is turned off in Settings. Turn it on to see new conversations here.';
      histEmptyClear.hidden = true;
    } else {
      histEmptyTitle.textContent = 'No conversations yet';
      histEmptyMsg.textContent = 'Once you talk with Live Ninja, your conversations will appear here, tagged by topic.';
      histEmptyClear.hidden = true;
    }
    setHistState('empty');
    histCountEl.textContent = '';
    histMoreWrapEl.hidden = true;
    return;
  }
  const sorted = [...conversations].sort((a, b) => b.startedAt - a.startedAt || (a.id < b.id ? -1 : 1));
  histBodyEl.textContent = '';
  for (const c of sorted) histBodyEl.appendChild(buildConvRow(c));
  setHistState('table');
  const n = conversations.length;
  histCountEl.textContent = nextCursor
    ? `Showing ${n} conversation${n === 1 ? '' : 's'} — more available`
    : `Showing ${n} conversation${n === 1 ? '' : 's'}`;
  histMoreWrapEl.hidden = !nextCursor;
}

function topicBadge(topicId) {
  const t = canonicalTopic(topicId);
  const badge = document.createElement('span');
  badge.className = 'hist-topic-dot';
  badge.style.setProperty('--chip-color', t ? t.color : '#94a3b8');
  badge.textContent = t ? t.name : topicId;
  return badge;
}

function buildConvRow(c) {
  const tr = document.createElement('tr');
  tr.dataset.id = c.id;

  const tdWhen = document.createElement('td');
  tdWhen.className = 'hist-when';
  const time = document.createElement('time');
  if (c.startedAtISO) time.dateTime = c.startedAtISO;
  time.textContent = fmtDate(c.startedAt);
  tdWhen.appendChild(time);
  tr.appendChild(tdWhen);

  const tdSummary = document.createElement('td');
  tdSummary.className = 'hist-summary';
  tdSummary.textContent = c.summary || '—';
  if (c.summary) tdSummary.title = c.summary;
  tr.appendChild(tdSummary);

  const tdTopics = document.createElement('td');
  tdTopics.className = 'hist-topics-cell';
  if (c.topicIds.length === 0) {
    tdTopics.textContent = '—';
  } else {
    const wrap = document.createElement('span');
    wrap.className = 'hist-topic-badges';
    // De-duplicate after canonicalization (two merged topics can point
    // at the same canonical topic); a topic id with no live match (the
    // topic was deleted — DeleteTopic never rewrites topicIds, see
    // internal/store/topics.go) is filtered out here on read rather than
    // rendered as a bare, unnamed id.
    const seen = new Set();
    for (const id of c.topicIds) {
      const t = canonicalTopic(id);
      if (!t) continue;
      if (seen.has(t.id)) continue;
      seen.add(t.id);
      wrap.appendChild(topicBadge(id));
    }
    // Every tag on this conversation pointed at a deleted topic — same
    // empty-state as never having been tagged.
    if (wrap.children.length === 0) tdTopics.textContent = '—';
    else tdTopics.appendChild(wrap);
  }
  tr.appendChild(tdTopics);

  const tdDevice = document.createElement('td');
  tdDevice.className = 'hist-device';
  tdDevice.textContent = deviceLabel(c);
  tr.appendChild(tdDevice);

  const tdTurns = document.createElement('td');
  tdTurns.className = 'hist-num';
  tdTurns.textContent = c.turnCount === null ? '—' : String(c.turnCount);
  tr.appendChild(tdTurns);

  const tdActions = document.createElement('td');
  tdActions.className = 'hist-actions-cell';
  const viewBtn = document.createElement('button');
  viewBtn.type = 'button';
  viewBtn.className = 'ln-btn ln-btn--ghost';
  viewBtn.textContent = 'View';
  viewBtn.setAttribute('aria-label', `View the conversation from ${fmtDate(c.startedAt)}`);
  viewBtn.addEventListener('click', () => openDetail(c));
  tdActions.appendChild(viewBtn);
  tr.appendChild(tdActions);

  return tr;
}

// ---- detail view ------------------------------------------------------

const listView = $('histListView');
const detailView = $('histDetailView');
const detailMeta = $('detailMeta');
const detailLoading = $('detailLoading');
const detailError = $('detailError');
const detailErrorMsg = $('detailErrorMsg');
const detailEmpty = $('detailEmpty');
const detailTranscript = $('detailTranscript');
const detailHeading = $('detailHeading');

function setDetailState(state) {
  detailLoading.hidden = state !== 'loading';
  detailError.hidden = state !== 'error';
  detailEmpty.hidden = state !== 'empty';
  detailTranscript.hidden = state !== 'transcript';
}

function metaBadge(text) {
  const b = document.createElement('span');
  b.className = 'ln-badge ln-badge--dot-none';
  b.textContent = text;
  return b;
}

function openDetail(c) {
  detailConvId = c.id;
  listView.hidden = true;
  detailView.hidden = false;
  detailHeading.textContent = c.summary || `Conversation — ${fmtDate(c.startedAt)}`;

  detailMeta.textContent = '';
  detailMeta.appendChild(metaBadge(fmtDate(c.startedAt)));
  detailMeta.appendChild(metaBadge(deviceLabel(c)));
  if (c.engine) detailMeta.appendChild(metaBadge(c.engine));
  const seen = new Set();
  for (const id of c.topicIds) {
    const t = canonicalTopic(id);
    if (!t) continue; // deleted topic — filtered on read, see buildConvRow
    if (seen.has(t.id)) continue;
    seen.add(t.id);
    detailMeta.appendChild(topicBadge(id));
  }

  $('detailBack').focus();
  loadDetail();
  window.scrollTo({ top: 0 });
}

function extractTurns(resp) {
  const raw = Array.isArray(resp.turns) ? resp.turns
    : Array.isArray(resp.transcript) ? resp.transcript
    : resp.conversation && Array.isArray(resp.conversation.turns) ? resp.conversation.turns : [];
  return raw
    .map((t) => {
      if (!t || typeof t !== 'object') return null;
      const role = String(t.role || t.speaker || '').toLowerCase();
      if (role === 'tool') {
        // Tool-call audit entry (server merges them into `turns` by
        // timestamp): parsed fields when the server could parse the audit
        // line, raw text as the fallback.
        return {
          role: 'tool',
          tool: String(t.tool || ''),
          outcome: String(t.outcome || ''),
          callId: String(t.callId || ''),
          args: typeof t.args === 'string' ? t.args : '',
          error: String(t.error || ''),
          output: typeof t.output === 'string' ? t.output : '',
          text: String(t.text || ''),
          ts: toMs(t.ts ?? t.at ?? t.createdAt),
        };
      }
      const text = String(t.text || t.content || '');
      if (!text) return null;
      return { role: role === 'user' ? 'user' : 'assistant', text, ts: toMs(t.ts ?? t.at ?? t.createdAt) };
    })
    .filter(Boolean);
}

// ---- tool-call cards (same ln-toolcard style the live transcript's
// appendToolResultCard renders — title/badge head + a <dl class="kv">,
// never a raw object dump) ----

const OUTCOME_BADGES = {
  ok: ['OK', 'teal'],
  error: ['Error', 'error'],
  duplicate: ['Duplicate', 'muted'],
};

/** Compact one-line preview of a JSON string (args/output snippets are
 * already capped server-side; this only keeps the card readable). */
function jsonPreview(raw, max = 200) {
  const s = String(raw || '').trim();
  if (!s || s === '{}' || s === 'null') return '';
  return s.length > max ? s.slice(0, max) + '…' : s;
}

let toolCardSeq = 0;

function buildToolCard(turn) {
  const outer = document.createElement('div');
  outer.className = 'ln-toolcard hist-toolcard';

  const card = document.createElement('div');
  card.className = 'ln-card';

  const head = document.createElement('div');
  head.className = 'ln-toolcard__head';

  const icon = document.createElement('span');
  icon.className = 'ln-toolcard__icon';
  icon.setAttribute('aria-hidden', 'true');
  icon.textContent = '🔧';
  head.appendChild(icon);

  const titleWrap = document.createElement('div');
  const title = document.createElement('div');
  title.className = 'ln-toolcard__title';
  title.textContent = turn.tool ? `Tool: ${turn.tool}` : 'Tool call';
  titleWrap.appendChild(title);
  if (turn.ts) {
    const sub = document.createElement('div');
    sub.className = 'ln-toolcard__sub';
    sub.textContent = fmtDate(turn.ts);
    titleWrap.appendChild(sub);
  }
  head.appendChild(titleWrap);

  const [badgeText, badgeVariant] = OUTCOME_BADGES[turn.outcome] || ['Tool', 'muted'];
  const badge = document.createElement('span');
  badge.className = `ln-badge ln-badge--${badgeVariant} ln-badge--dot-none`;
  badge.style.marginLeft = 'auto';
  badge.textContent = badgeText;
  head.appendChild(badge);

  // Full args + output (the audit rows store an output snippet) behind an
  // accessible disclosure: the Details button pins the panel open
  // (aria-expanded), while mouse hover or keyboard focus anywhere in the
  // card reveals it transiently (CSS :hover / :focus-within). A plain
  // `title` tooltip on the card is the last-resort fallback.
  const fullArgs = String(turn.args || '').trim();
  const fullOutput = String(turn.output || '').trim();
  let fullPanel = null;
  if (fullArgs || fullOutput || turn.error) {
    const tipParts = [];
    if (fullArgs) tipParts.push(`Arguments: ${fullArgs}`);
    if (turn.error) tipParts.push(`Error: ${turn.error}`);
    if (fullOutput) tipParts.push(`Output: ${fullOutput}`);
    const tip = tipParts.join('\n');
    outer.title = tip.length > 1500 ? tip.slice(0, 1500) + '…' : tip;

    const panelId = `histToolFull-${++toolCardSeq}`;
    const detailsBtn = document.createElement('button');
    detailsBtn.type = 'button';
    detailsBtn.className = 'ln-btn ln-btn--ghost hist-toolcard__details-btn';
    detailsBtn.textContent = 'Details';
    detailsBtn.setAttribute('aria-expanded', 'false');
    detailsBtn.setAttribute('aria-controls', panelId);
    detailsBtn.setAttribute('aria-label', `Show full details for the ${turn.tool || 'tool'} call`);
    head.appendChild(detailsBtn);

    fullPanel = document.createElement('div');
    fullPanel.className = 'hist-toolcard__full';
    fullPanel.id = panelId;
    fullPanel.hidden = true;
    const panel = fullPanel;
    const addBlock = (label, value) => {
      const lab = document.createElement('div');
      lab.className = 'hist-toolcard__full-label';
      lab.textContent = label;
      const pre = document.createElement('pre');
      pre.textContent = value;
      panel.appendChild(lab);
      panel.appendChild(pre);
    };
    if (fullArgs) addBlock('Arguments', fullArgs);
    if (turn.error) addBlock('Error', turn.error);
    if (fullOutput) addBlock('Output', fullOutput);

    detailsBtn.addEventListener('click', () => {
      panel.hidden = !panel.hidden;
      detailsBtn.setAttribute('aria-expanded', panel.hidden ? 'false' : 'true');
    });
  }

  card.appendChild(head);

  const fields = [];
  const args = jsonPreview(turn.args);
  if (args) fields.push(['Arguments', args]);
  if (turn.error) fields.push(['Error', turn.error]);
  const output = jsonPreview(turn.output);
  if (output) fields.push(['Result', output]);
  if (fields.length === 0 && !turn.tool && turn.text) {
    // Unparseable/legacy audit line — show it verbatim rather than nothing.
    fields.push(['Details', turn.text]);
  }
  if (fields.length > 0) {
    const dl = document.createElement('dl');
    dl.className = 'kv';
    for (const [label, value] of fields) {
      const dt = document.createElement('dt');
      dt.textContent = label;
      const dd = document.createElement('dd');
      dd.textContent = value;
      dl.appendChild(dt);
      dl.appendChild(dd);
    }
    card.appendChild(dl);
  }

  // Full detail sits below the compact preview.
  if (fullPanel) card.appendChild(fullPanel);

  outer.appendChild(card);
  return outer;
}

// "Show tool calls" toggle — ln-toggle slider at the TOP of the page
// (list level: one control governs every detail view), default off,
// remembered in localStorage across visits.
const showToolCalls = $('showToolCalls');
showToolCalls.checked = localStorage.getItem(SHOW_TOOLS_KEY) === '1';
detailTranscript.classList.toggle('show-tools', showToolCalls.checked);
showToolCalls.addEventListener('change', () => {
  localStorage.setItem(SHOW_TOOLS_KEY, showToolCalls.checked ? '1' : '0');
  detailTranscript.classList.toggle('show-tools', showToolCalls.checked);
});

async function loadDetail() {
  const id = detailConvId;
  setDetailState('loading');
  try {
    const resp = await apiJSON(`/api/v1/conversations/${encodeURIComponent(id)}`);
    if (detailConvId !== id) return; // navigated away meanwhile
    const turns = extractTurns(resp);
    if (turns.length === 0) {
      setDetailState('empty');
      return;
    }
    detailTranscript.textContent = '';
    for (const turn of turns) {
      if (turn.role === 'tool') {
        detailTranscript.appendChild(buildToolCard(turn));
        continue;
      }
      const bubble = document.createElement('div');
      bubble.className = `ln-bubble ln-bubble--${turn.role}`;
      const role = document.createElement('div');
      role.className = 'ln-bubble__role';
      role.textContent = turn.role === 'user' ? 'You' : 'Live Ninja';
      const body = document.createElement('div');
      body.textContent = turn.text;
      bubble.appendChild(role);
      bubble.appendChild(body);
      detailTranscript.appendChild(bubble);
    }
    setDetailState('transcript');
  } catch (err) {
    if (detailConvId !== id) return;
    detailErrorMsg.textContent = apiErrorMessage(err, 'Check your connection and try again.');
    setDetailState('error');
  }
}

$('detailBack').addEventListener('click', () => {
  detailConvId = null;
  detailView.hidden = true;
  listView.hidden = false;
});
$('detailRetry').addEventListener('click', () => loadDetail());

// ---- topic manager ----------------------------------------------------

const topicMgrList = $('topicMgrList');
const topicMgrEmpty = $('topicMgrEmpty');

async function patchTopic(t, patch, successMsg) {
  await apiJSON(`/api/v1/topics/${encodeURIComponent(t.id)}`, { method: 'PATCH', json: patch });
  Object.assign(t, patch);
  if (successMsg) showToast(successMsg);
  renderTopicChips();
  renderTopicManager();
  renderConversations(); // badges may have changed name/color/merge target
}

function renderTopicManager() {
  const live = topics.filter((t) => !t.mergedInto);
  topicMgrList.textContent = '';
  topicMgrEmpty.hidden = live.length > 0;
  for (const t of live.sort((a, b) => Number(a.archived) - Number(b.archived) || a.name.localeCompare(b.name))) {
    topicMgrList.appendChild(buildTopicRow(t));
  }
}

/** Small action-button factory shared by buildTopicRow and its delete
 * confirmation (confirmDeleteTopic) — module scope so both can use it. */
function mkBtn(label, aria, cls = 'ln-btn ln-btn--ghost') {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = cls;
  b.textContent = label;
  b.setAttribute('aria-label', aria);
  return b;
}

function buildTopicRow(t) {
  const row = document.createElement('div');
  row.className = 'hist-topicrow';
  row.dataset.id = t.id;

  // Color — native color picker, saved on change.
  const color = document.createElement('input');
  color.type = 'color';
  color.value = t.color;
  color.setAttribute('aria-label', `Color for the topic ${t.name}`);
  color.addEventListener('change', () => {
    patchTopic(t, { color: color.value }, null).catch((err) => {
      color.value = t.color;
      showToast(apiErrorMessage(err, "Couldn't change the color — try again."), { error: true });
    });
  });
  row.appendChild(color);

  const name = document.createElement('span');
  name.className = 'hist-topicname';
  name.textContent = t.name;
  row.appendChild(name);

  const count = document.createElement('span');
  count.className = 'hist-topiccount';
  count.textContent = t.convCount === null ? '' : `${t.convCount} conversation${t.convCount === 1 ? '' : 's'}`;
  row.appendChild(count);

  if (t.archived) {
    const badge = document.createElement('span');
    badge.className = 'ln-badge ln-badge--dot-none';
    badge.textContent = 'Archived';
    row.appendChild(badge);
  }

  // Rename — swaps the name span for an input with Save/Cancel.
  const renameBtn = mkBtn('Rename', `Rename the topic ${t.name}`);
  renameBtn.addEventListener('click', () => {
    const input = document.createElement('input');
    input.type = 'text';
    input.className = 'ln-input';
    input.maxLength = 80;
    input.value = t.name;
    input.setAttribute('aria-label', `New name for the topic ${t.name}`);
    name.textContent = '';
    name.appendChild(input);
    renameBtn.hidden = true;

    const save = mkBtn('Save', `Save the new name for ${t.name}`, 'ln-btn ln-btn--primary');
    const cancel = mkBtn('Cancel', `Cancel renaming ${t.name}`);
    const restore = () => {
      name.textContent = t.name;
      save.remove();
      cancel.remove();
      renameBtn.hidden = false;
    };
    save.addEventListener('click', async () => {
      const newName = input.value.trim();
      if (!newName || newName === t.name) {
        restore();
        return;
      }
      save.disabled = true;
      try {
        await patchTopic(t, { name: newName }, `Renamed to “${newName}”.`);
      } catch (err) {
        restore();
        showToast(apiErrorMessage(err, "Couldn't rename the topic — try again."), { error: true });
      }
    });
    cancel.addEventListener('click', restore);
    input.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter') { ev.preventDefault(); save.click(); }
      else if (ev.key === 'Escape') { ev.stopPropagation(); restore(); }
    });
    renameBtn.after(save, cancel);
    input.focus();
    input.select();
  });
  row.appendChild(renameBtn);

  // Archive / restore — reversible, so a single click is fine.
  const archBtn = mkBtn(t.archived ? 'Restore' : 'Archive',
    t.archived ? `Restore the topic ${t.name}` : `Archive the topic ${t.name}`);
  archBtn.addEventListener('click', async () => {
    archBtn.disabled = true;
    try {
      await patchTopic(t, { archived: !t.archived },
        t.archived ? `Restored “${t.name}”.` : `Archived “${t.name}” — its tags are kept.`);
    } catch (err) {
      showToast(apiErrorMessage(err, "Couldn't update the topic — try again."), { error: true });
    } finally {
      archBtn.disabled = false;
    }
  });
  row.appendChild(archBtn);

  // Merge — target picked from the populated taxonomy (never typed),
  // guarded by an explicit confirm step since it repoints tags.
  const mergeBtn = mkBtn('Merge…', `Merge the topic ${t.name} into another topic`);
  mergeBtn.addEventListener('click', () => {
    const others = topics.filter((x) => x.id !== t.id && !x.mergedInto && !x.archived);
    if (others.length === 0) {
      showToast('No other topics to merge into yet.');
      return;
    }
    mergeBtn.hidden = true;
    const form = document.createElement('span');
    form.className = 'hist-mergeform';
    const sel = document.createElement('select');
    sel.className = 'ln-select';
    sel.setAttribute('aria-label', `Topic to merge ${t.name} into`);
    const ph = document.createElement('option');
    ph.value = '';
    ph.textContent = 'Merge into…';
    sel.appendChild(ph);
    for (const o of others.sort((a, b) => a.name.localeCompare(b.name))) {
      const opt = document.createElement('option');
      opt.value = o.id;
      opt.textContent = o.name;
      sel.appendChild(opt);
    }
    const go = mkBtn('Merge', `Confirm merging ${t.name}`, 'ln-btn ln-btn--danger');
    go.disabled = true;
    const cancel = mkBtn('Cancel', `Cancel merging ${t.name}`);
    sel.addEventListener('change', () => { go.disabled = !sel.value; });
    const restore = () => {
      form.remove();
      mergeBtn.hidden = false;
    };
    go.addEventListener('click', async () => {
      const target = topics.find((x) => x.id === sel.value);
      if (!target) return;
      go.disabled = true;
      try {
        await patchTopic(t, { mergedInto: target.id },
          `Merged “${t.name}” into “${target.name}” — existing tags now show as ${target.name}.`);
      } catch (err) {
        restore();
        showToast(apiErrorMessage(err, "Couldn't merge the topics — try again."), { error: true });
      }
    });
    cancel.addEventListener('click', restore);
    form.appendChild(sel);
    form.appendChild(go);
    form.appendChild(cancel);
    mergeBtn.after(form);
    sel.focus();
  });
  row.appendChild(mergeBtn);

  // Delete — irreversible taxonomy removal, guarded by an explicit inline
  // confirm (house destructive-action rule) that states exactly what
  // happens: the topic and its TREF refs go away, but conversations are
  // kept, only untagged (DeleteTopic never touches CONV rows). convCount
  // came straight from the topic list response, no extra round trip.
  const deleteBtn = mkBtn('Delete', `Delete the topic ${t.name}`, 'ln-btn ln-btn--danger');
  deleteBtn.addEventListener('click', () => confirmDeleteTopic(t, deleteBtn));
  row.appendChild(deleteBtn);

  return row;
}

/** Two-step inline delete confirmation for one topic (mirrors
 * confirmForget in memory.mjs): the trigger button hides and an inline
 * confirm — message + Confirm/Cancel — takes its place in the row. */
function confirmDeleteTopic(t, trigger) {
  trigger.hidden = true;
  const n = t.convCount || 0;
  const wrap = document.createElement('span');
  wrap.className = 'hist-delconfirm';
  const msg = document.createElement('span');
  msg.textContent = `Delete “${t.name}”? Removes the topic and untags ${n} conversation${n === 1 ? '' : 's'} — conversations themselves are kept.`;
  wrap.appendChild(msg);

  const restore = () => {
    wrap.remove();
    trigger.hidden = false;
  };

  const go = mkBtn('Yes, delete', `Confirm deleting ${t.name}`, 'ln-btn ln-btn--danger');
  const cancel = mkBtn('Cancel', `Cancel deleting ${t.name}`);
  go.addEventListener('click', async () => {
    go.disabled = true;
    cancel.disabled = true;
    try {
      await apiJSON(`/api/v1/topics/${encodeURIComponent(t.id)}`, { method: 'DELETE' });
      topics = topics.filter((x) => x.id !== t.id);
      selectedTopics.delete(t.id);
      showToast(`Deleted “${t.name}”.`);
      renderTopicChips();
      renderTopicManager();
      renderConversations(); // badges for its (now-untagged) conversations drop
    } catch (err) {
      restore();
      showToast(apiErrorMessage(err, `Couldn't delete “${t.name}” — try again.`), { error: true });
    }
  });
  cancel.addEventListener('click', restore);
  wrap.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape') {
      ev.stopPropagation();
      restore();
    }
  });

  wrap.appendChild(go);
  wrap.appendChild(cancel);
  trigger.after(wrap);
  go.focus();
}

// ---- filters wiring ---------------------------------------------------

deviceSelect.addEventListener('change', () => {
  deviceFilter = deviceSelect.value;
  syncClearButtons();
  loadConversations();
});

$('fromDate').addEventListener('change', (ev) => {
  fromDate = ev.target.value;
  syncClearButtons();
  loadConversations();
});
$('toDate').addEventListener('change', (ev) => {
  toDate = ev.target.value;
  syncClearButtons();
  loadConversations();
});

function clearFilters() {
  selectedTopics.clear();
  deviceFilter = '';
  fromDate = '';
  toDate = '';
  deviceSelect.value = '';
  $('fromDate').value = '';
  $('toDate').value = '';
  dateRangeErr.hidden = true;
  syncClearButtons();
  renderTopicChips();
  loadConversations();
}
$('clearFilters').addEventListener('click', clearFilters);
histEmptyClear.addEventListener('click', clearFilters);

$('histRefresh').addEventListener('click', () => loadConversations());
$('histRetry').addEventListener('click', () => loadConversations());
histMoreBtn.addEventListener('click', () => loadConversations({ append: true }));

// ---- init -------------------------------------------------------------

loadTopics();
loadDevices();
loadConversations();
loadStorageHint();
syncClearButtons();
