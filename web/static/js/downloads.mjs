// downloads.mjs — /downloads Download Center controller (M9 FR-DLV-05,
// owned by the M9 web-UI workstream).
//
// Data plane (contracts/api.md "Deliverables Store" + M9 locked
// decisions; concrete shapes from internal/webapp/deliverables_routes.go
// and internal/tools/deliverable.go):
//   - GET    /api/v1/deliverables            — Query-backed paginated list
//     ({deliverables:[{deliverableId, name, kind, status, contentType,
//     sizeBytes, createdAt}], nextCursor?}; ?limit=&cursor=).
//   - DELETE /api/v1/deliverables/{id}       — remove one deliverable.
//   - deliverable_deliver / deliverable_zip  — invoked through the shared
//     tool router (POST /api/v1/tools/invoke, tools registry) rather than
//     bespoke REST calls: deliver ({deliverableId, method:"link"}) mints
//     the 15-minute presigned GET we use for BOTH the Download action and
//     the Share/copy-link action; zip takes {deliverableIds:[...]} (max
//     50 sources, internal/deliv.MaxZipSources).
//
// Why downloads navigate instead of fetch()ing: the page CSP's
// connect-src is 'self' + api.openai.com (pages_routes.go), so the S3
// presigned URL can never be fetch()ed from page JS. We mint the URL over
// a same-origin API call, then trigger a plain anchor navigation to it —
// navigations aren't governed by connect-src, and S3's
// Content-Disposition turns it into a file save. The same minted URL is
// what "Copy link" places on the clipboard (valid ~15 minutes, per
// FR-DLV-03).
//
// All row content is built with textContent (never innerHTML) — file
// names are user/model-authored data.

import { apiJSON, authFetch, ApiError } from './toolclient.mjs';

const $ = (id) => document.getElementById(id);

// ---- state ------------------------------------------------------------

let items = []; // normalized deliverables currently loaded
let nextCursor = null; // opaque pagination cursor from the server
let sortKey = 'createdAt';
let sortDir = 'desc';
const selected = new Set(); // deliverable ids checked for bulk zip
let loadSeq = 0; // stale-response guard for refresh vs load-more races

// ---- toast (same pattern as settings.mjs) -----------------------------

const toastEl = $('toast');
const toastMsgEl = $('toastMsg');
const toastActionBtn = $('toastActionBtn');
let toastTimer = 0;
let toastAction = null;

function showToast(msg, { label, onClick, error = false } = {}) {
  clearTimeout(toastTimer);
  toastMsgEl.textContent = msg;
  toastEl.classList.toggle('is-error', !!error);
  if (label && onClick) {
    toastAction = onClick;
    toastActionBtn.textContent = label;
    toastActionBtn.hidden = false;
  } else {
    toastAction = null;
    toastActionBtn.hidden = true;
  }
  toastEl.hidden = false;
  requestAnimationFrame(() => toastEl.classList.add('is-visible'));
  toastTimer = setTimeout(hideToast, label ? 10000 : 6000);
}

function hideToast() {
  clearTimeout(toastTimer);
  toastEl.classList.remove('is-visible');
  toastEl.hidden = true;
}

toastActionBtn.addEventListener('click', () => {
  const fn = toastAction;
  hideToast();
  if (fn) fn();
});

// ---- API helpers ------------------------------------------------------

function apiErrorMessage(err, fallback) {
  if (err instanceof ApiError && err.body) {
    const b = err.body;
    if (typeof b.message === 'string' && b.message) return b.message;
    if (b.error && typeof b.error === 'object' && b.error.message) return b.error.message;
  }
  return fallback;
}

function randomId() {
  if (globalThis.crypto && typeof globalThis.crypto.randomUUID === 'function') {
    return globalThis.crypto.randomUUID();
  }
  return 'dl-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10);
}

/** Invoke a deliverables tool through the shared tool router. Resolves the
 * tool Result.output; throws Error(message) on a tool-level failure. */
async function toolInvoke(tool, args) {
  let res;
  try {
    res = await apiJSON('/api/v1/tools/invoke', {
      method: 'POST',
      json: { tool, args, idempotencyKey: randomId() },
    });
  } catch (err) {
    // Non-2xx tool results still carry the Result envelope in the body.
    if (err instanceof ApiError && err.body && err.body.error && err.body.error.message) {
      throw new Error(err.body.error.message);
    }
    throw err;
  }
  if (res && res.ok === false) {
    throw new Error((res.error && res.error.message) || 'The request failed.');
  }
  return (res && res.output) || {};
}

/** Mint the short-lived presigned GET URL for one deliverable via the
 * deliverable_deliver tool (method "link" = hand the URL back rather
 * than emailing it — internal/tools/deliverable.go). */
async function mintUrl(id) {
  const out = await toolInvoke('deliverable_deliver', { deliverableId: id, method: 'link' });
  if (!out.url) throw new Error('The server did not return a download link.');
  return { url: out.url, expiresAt: out.expiresAt || null };
}

// ---- normalization / formatting ---------------------------------------

function normalizeItem(d) {
  if (!d || typeof d !== 'object') return null;
  const id = d.id || d.deliverableId;
  if (!id) return null;
  let createdAt = d.createdAt ?? d.created ?? null;
  if (typeof createdAt === 'number' && createdAt < 1e12) createdAt *= 1000; // epoch s → ms
  return {
    id: String(id),
    name: String(d.name || d.filename || d.title || id),
    kind: String(d.kind || ''),
    contentType: String(d.contentType || d.type || ''),
    sizeBytes: Number.isFinite(Number(d.sizeBytes ?? d.size)) ? Number(d.sizeBytes ?? d.size) : null,
    createdAt: createdAt ? new Date(createdAt).getTime() || 0 : 0,
    createdAtISO: createdAt ? new Date(createdAt).toISOString() : '',
    // Absent status means the object is fully written (create is
    // synchronous); zip bundles may briefly surface pending/zipping.
    status: String(d.status || 'ready'),
  };
}

const TYPE_LABELS = [
  [/zip/i, 'ZIP'],
  [/pdf/i, 'PDF'],
  [/markdown|\.md$/i, 'Markdown'],
  [/csv/i, 'CSV'],
  [/html/i, 'HTML'],
  [/json/i, 'JSON'],
  [/calendar|\.ics$/i, 'Calendar'],
  [/^image\//i, 'Image'],
  [/^text\//i, 'Text'],
];

function typeLabel(it) {
  const probe = `${it.kind} ${it.contentType}`.trim() || it.name;
  for (const [re, label] of TYPE_LABELS) {
    if (re.test(probe)) return label;
  }
  const ext = /\.([a-z0-9]{1,5})$/i.exec(it.name);
  return ext ? ext[1].toUpperCase() : 'File';
}

function fmtSize(n) {
  if (n === null || !Number.isFinite(n)) return '—';
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

const dateFmt = new Intl.DateTimeFormat([], {
  year: 'numeric', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit',
});
const fmtDate = (ms) => (ms ? dateFmt.format(new Date(ms)) : '—');

// ---- loading ----------------------------------------------------------

const loadingEl = $('dlLoading');
const emptyEl = $('dlEmpty');
const errorEl = $('dlError');
const errorMsgEl = $('dlErrorMsg');
const tableWrapEl = $('dlTableWrap');
const bodyEl = $('dlBody');
const countEl = $('dlCount');
const moreWrapEl = $('dlMoreWrap');
const moreBtn = $('dlMore');

function setViewState(state) {
  loadingEl.hidden = state !== 'loading';
  emptyEl.hidden = state !== 'empty';
  errorEl.hidden = state !== 'error';
  tableWrapEl.hidden = state !== 'table';
}

async function fetchPage(cursor) {
  const qs = cursor ? `?cursor=${encodeURIComponent(cursor)}` : '';
  const resp = await apiJSON(`/api/v1/deliverables${qs}`);
  const raw = Array.isArray(resp.deliverables) ? resp.deliverables
    : Array.isArray(resp.items) ? resp.items : [];
  return {
    items: raw.map(normalizeItem).filter(Boolean),
    nextCursor: resp.nextCursor || resp.cursor || null,
  };
}

async function loadList({ append = false } = {}) {
  const seq = ++loadSeq;
  if (!append) {
    setViewState('loading');
    countEl.textContent = '';
  } else {
    moreBtn.disabled = true;
    moreBtn.textContent = 'Loading…';
  }
  try {
    const page = await fetchPage(append ? nextCursor : null);
    if (seq !== loadSeq) return; // superseded by a newer refresh
    if (append) {
      const known = new Set(items.map((i) => i.id));
      items = items.concat(page.items.filter((i) => !known.has(i.id)));
    } else {
      items = page.items;
      selected.clear();
    }
    nextCursor = page.nextCursor;
    render();
  } catch (err) {
    if (seq !== loadSeq) return;
    if (append) {
      showToast(apiErrorMessage(err, "Couldn't load more files — try again."), { error: true });
    } else {
      errorMsgEl.textContent = apiErrorMessage(err, 'Check your connection and try again.');
      setViewState('error');
    }
  } finally {
    moreBtn.disabled = false;
    moreBtn.textContent = 'Load more';
  }
}

// ---- rendering --------------------------------------------------------

function sortedItems() {
  const dir = sortDir === 'asc' ? 1 : -1;
  return [...items].sort((a, b) => {
    let cmp;
    if (sortKey === 'name') cmp = a.name.localeCompare(b.name, undefined, { sensitivity: 'base' });
    else if (sortKey === 'sizeBytes') cmp = (a.sizeBytes ?? -1) - (b.sizeBytes ?? -1);
    else cmp = a.createdAt - b.createdAt;
    if (cmp === 0) cmp = a.id < b.id ? -1 : 1; // stable tiebreak
    return cmp * dir;
  });
}

function render() {
  // Drop selections for rows that no longer exist.
  const ids = new Set(items.map((i) => i.id));
  for (const id of [...selected]) if (!ids.has(id)) selected.delete(id);

  if (items.length === 0) {
    setViewState('empty');
    countEl.textContent = '';
    moreWrapEl.hidden = true;
    syncBulkBar();
    return;
  }

  bodyEl.textContent = '';
  for (const it of sortedItems()) bodyEl.appendChild(buildRow(it));
  setViewState('table');

  countEl.textContent = nextCursor
    ? `Showing ${items.length} file${items.length === 1 ? '' : 's'} — more available`
    : `Showing ${items.length} file${items.length === 1 ? '' : 's'}`;
  moreWrapEl.hidden = !nextCursor;
  syncBulkBar();
  syncSelectAll();
}

function buildRow(it) {
  const ready = it.status === 'ready';
  const tr = document.createElement('tr');
  tr.dataset.id = it.id;

  // select checkbox
  const tdCheck = document.createElement('td');
  tdCheck.className = 'dl-col-check';
  const cb = document.createElement('input');
  cb.type = 'checkbox';
  cb.checked = selected.has(it.id);
  cb.disabled = !ready;
  cb.setAttribute('aria-label', `Select ${it.name}`);
  cb.addEventListener('change', () => {
    if (cb.checked) selected.add(it.id);
    else selected.delete(it.id);
    syncBulkBar();
    syncSelectAll();
  });
  tdCheck.appendChild(cb);
  tr.appendChild(tdCheck);

  // name
  const tdName = document.createElement('td');
  tdName.className = 'dl-name';
  tdName.textContent = it.name;
  tr.appendChild(tdName);

  // type badge
  const tdType = document.createElement('td');
  const badge = document.createElement('span');
  const label = typeLabel(it);
  badge.className = 'ln-badge ln-badge--dot-none' + (label === 'ZIP' ? ' dl-badge-zip' : ' ln-badge--teal');
  badge.textContent = label;
  tdType.appendChild(badge);
  if (!ready) {
    const st = document.createElement('span');
    st.className = 'ln-badge ln-badge--dot-none';
    st.style.marginLeft = '6px';
    st.textContent = 'Preparing…';
    tdType.appendChild(st);
  }
  tr.appendChild(tdType);

  // size (right-aligned)
  const tdSize = document.createElement('td');
  tdSize.className = 'dl-num';
  tdSize.textContent = fmtSize(it.sizeBytes);
  tr.appendChild(tdSize);

  // created
  const tdDate = document.createElement('td');
  tdDate.className = 'dl-date';
  const time = document.createElement('time');
  if (it.createdAtISO) time.dateTime = it.createdAtISO;
  time.textContent = fmtDate(it.createdAt);
  tdDate.appendChild(time);
  tr.appendChild(tdDate);

  // actions
  const tdActions = document.createElement('td');
  tdActions.className = 'dl-actions-cell';
  tdActions.appendChild(actionButtons(it, ready));
  tr.appendChild(tdActions);

  return tr;
}

function makeBtn(label, ariaLabel, cls = 'ln-btn ln-btn--ghost') {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = cls;
  b.textContent = label;
  b.setAttribute('aria-label', ariaLabel);
  return b;
}

function actionButtons(it, ready) {
  const frag = document.createDocumentFragment();

  const dlBtn = makeBtn('Download', `Download ${it.name}`);
  dlBtn.disabled = !ready;
  dlBtn.addEventListener('click', () => downloadItem(it, dlBtn));
  frag.appendChild(dlBtn);

  const shareBtn = makeBtn('Copy link', `Copy a share link for ${it.name}`);
  shareBtn.disabled = !ready;
  shareBtn.addEventListener('click', () => shareItem(it, shareBtn));
  frag.appendChild(shareBtn);

  const delBtn = makeBtn('Delete', `Delete ${it.name}`, 'ln-btn ln-btn--danger');
  delBtn.addEventListener('click', () => confirmDelete(it, delBtn));
  frag.appendChild(delBtn);

  return frag;
}

// ---- actions ----------------------------------------------------------

async function downloadItem(it, btn) {
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  try {
    const { url } = await mintUrl(it.id);
    // Anchor navigation (see file header: CSP forbids fetch()ing S3).
    const a = document.createElement('a');
    a.href = url;
    a.rel = 'noopener';
    a.download = it.name; // hint only cross-origin; S3 Content-Disposition governs
    document.body.appendChild(a);
    a.click();
    a.remove();
  } catch (err) {
    showToast(apiErrorMessage(err, `Couldn't start the download for ${it.name}.`), { error: true });
  } finally {
    btn.disabled = false;
    btn.removeAttribute('aria-busy');
  }
}

async function copyText(text) {
  if (navigator.clipboard && navigator.clipboard.writeText) {
    await navigator.clipboard.writeText(text);
    return;
  }
  // Legacy fallback (non-secure contexts don't apply here, but keep it).
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  document.execCommand('copy');
  ta.remove();
}

async function shareItem(it, btn) {
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  try {
    const { url } = await mintUrl(it.id);
    await copyText(url);
    showToast('Link copied — it works for about 15 minutes.');
  } catch (err) {
    showToast(apiErrorMessage(err, "Couldn't create a share link — try again."), { error: true });
  } finally {
    btn.disabled = false;
    btn.removeAttribute('aria-busy');
  }
}

/** Two-step inline delete confirmation: the row's action cell swaps to
 * "Delete <name>? [Yes] [Cancel]". Escape or Cancel restores it. */
function confirmDelete(it, trigger) {
  const cell = trigger.closest('td');
  if (!cell) return;
  cell.textContent = '';

  const wrap = document.createElement('span');
  wrap.className = 'dl-confirm';

  const q = document.createElement('span');
  q.textContent = 'Delete this file?';
  wrap.appendChild(q);

  const yes = makeBtn('Yes, delete', `Confirm deleting ${it.name}`, 'ln-btn ln-btn--danger');
  const cancel = makeBtn('Cancel', `Cancel deleting ${it.name}`);

  const restore = () => {
    cell.textContent = '';
    cell.appendChild(actionButtons(it, it.status === 'ready'));
  };

  yes.addEventListener('click', async () => {
    yes.disabled = true;
    cancel.disabled = true;
    try {
      const resp = await authFetch(`/api/v1/deliverables/${encodeURIComponent(it.id)}`, { method: 'DELETE' });
      if (!resp.ok && resp.status !== 404) {
        throw new ApiError(resp.status, await resp.json().catch(() => null));
      }
      items = items.filter((x) => x.id !== it.id);
      selected.delete(it.id);
      render();
      showToast(`Deleted ${it.name}.`);
    } catch (err) {
      restore();
      showToast(apiErrorMessage(err, `Couldn't delete ${it.name} — try again.`), { error: true });
    }
  });
  cancel.addEventListener('click', restore);
  wrap.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      e.stopPropagation();
      restore();
    }
  });

  wrap.appendChild(yes);
  wrap.appendChild(cancel);
  cell.appendChild(wrap);
  yes.focus();
}

// ---- bulk selection + zip ---------------------------------------------

const bulkBar = $('dlBulkBar');
const bulkCount = $('dlBulkCount');
const zipBtn = $('dlZipBtn');
const clearSelBtn = $('dlClearSel');
const selectAll = $('dlSelectAll');

function syncBulkBar() {
  const n = selected.size;
  bulkBar.hidden = n === 0;
  bulkCount.textContent = `${n} selected`;
  zipBtn.disabled = n === 0;
}

function syncSelectAll() {
  const selectable = items.filter((i) => i.status === 'ready');
  const n = selectable.filter((i) => selected.has(i.id)).length;
  selectAll.checked = selectable.length > 0 && n === selectable.length;
  selectAll.indeterminate = n > 0 && n < selectable.length;
}

selectAll.addEventListener('change', () => {
  const selectable = items.filter((i) => i.status === 'ready');
  if (selectAll.checked) for (const i of selectable) selected.add(i.id);
  else selected.clear();
  render();
});

clearSelBtn.addEventListener('click', () => {
  selected.clear();
  render();
});

zipBtn.addEventListener('click', async () => {
  const ids = [...selected];
  if (ids.length === 0) return;
  if (ids.length > 50) {
    showToast('Zips are limited to 50 files at a time — deselect a few and try again.', { error: true });
    return;
  }
  zipBtn.disabled = true;
  zipBtn.setAttribute('aria-busy', 'true');
  try {
    await toolInvoke('deliverable_zip', { deliverableIds: ids });
    selected.clear();
    showToast(`Zipping ${ids.length} file${ids.length === 1 ? '' : 's'} — the bundle will appear in this list shortly.`);
    // The zipper Lambda runs async (M9 locked decision): refresh once
    // soon and once again after it has had time to finish.
    setTimeout(() => loadList(), 4000);
    setTimeout(() => loadList(), 15000);
    render();
  } catch (err) {
    showToast(apiErrorMessage(err, "Couldn't start the zip — try again."), { error: true });
  } finally {
    zipBtn.removeAttribute('aria-busy');
    syncBulkBar();
  }
});

// ---- sorting ----------------------------------------------------------

for (const btn of document.querySelectorAll('.dl-sort')) {
  btn.addEventListener('click', () => {
    const key = btn.dataset.sort;
    if (sortKey === key) {
      sortDir = sortDir === 'asc' ? 'desc' : 'asc';
    } else {
      sortKey = key;
      sortDir = key === 'name' ? 'asc' : 'desc';
    }
    for (const th of document.querySelectorAll('th[data-sort-col]')) {
      const active = th.dataset.sortCol === sortKey;
      th.setAttribute('aria-sort', active ? (sortDir === 'asc' ? 'ascending' : 'descending') : 'none');
      const arrow = th.querySelector('.arrow');
      if (arrow) arrow.textContent = active && sortDir === 'asc' ? '▲' : '▼';
    }
    render();
  });
}

// ---- init -------------------------------------------------------------

$('dlRefresh').addEventListener('click', () => loadList());
$('dlRetry').addEventListener('click', () => loadList());
moreBtn.addEventListener('click', () => loadList({ append: true }));

loadList();
