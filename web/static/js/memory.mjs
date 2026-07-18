// memory.mjs — /memory Memory browser + Guides manager controller
// (M10 FR-MEM-05/07/09, owned by the M10+M11 web-UI workstream).
//
// Data plane (contracts/api.md "Memory Layer & Guide Entities", served by
// the M10 API workstream under the same /api/v1 prefix every other
// authenticated route uses — see internal/webapp/api_routes.go):
//   - GET    /api/v1/entities?type={t}   — list entities of one type
//     (Query-only). With no ?type the handler lists every type; if a
//     stricter handler rejects the bare call (400/404/405) we fan out one
//     request per known type and merge — both shapes stay Query-only
//     server-side.
//   - POST   /api/v1/memory              — upsert a typed memory item
//     ({entityId?, type, name, attrs, relations?}); entityId present =
//     edit, absent = create (memory.write semantics, FR-MEM-04).
//   - DELETE /api/v1/memory/{id}         — "forget": removes the entity
//     from DynamoDB AND the embedding store (both tiers, FR-MEM-05).
//   - GET    /api/v1/guides              — list Guide Entities; the server
//     seeds the default "AI is an emerging technology" guide on first
//     list (FR-MEM-09), so this page never has to special-case it.
//   - PUT    /api/v1/guides/{id}         — create/edit a guide
//     ({title, text, enabled, priority, version}); create uses a fresh
//     client UUID as {id} per the contract's "PUT creates or edits".
//
// Entity types are the fixed enum person|place|info|project|task|plan
// (M10 locked item shapes) — every type control on this page is populated
// from that enum, never free text.
//
// All data-driven markup is built with textContent (never innerHTML) —
// names/attrs/guide text are user- and model-authored data.

import { apiJSON, authFetch, ApiError } from './toolclient.mjs';

const $ = (id) => document.getElementById(id);

const ENTITY_TYPES = ['person', 'place', 'info', 'project', 'task', 'plan'];
const TYPE_LABELS = {
  person: 'Person',
  place: 'Place',
  info: 'Info',
  project: 'Project',
  task: 'Task',
  plan: 'Plan',
};

// ---- state ------------------------------------------------------------

let entities = []; // normalized entities currently loaded
let nextCursor = null;
let typeFilter = '';
let loadSeq = 0; // stale-response guard

let guides = [];
let guideLoadSeq = 0;

// ---- toast (same pattern as downloads.mjs/settings.mjs) ---------------

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

function randomId() {
  if (globalThis.crypto && typeof globalThis.crypto.randomUUID === 'function') {
    return globalThis.crypto.randomUUID();
  }
  return 'id-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10);
}

// ---- normalization ----------------------------------------------------

function normalizeEntity(e) {
  if (!e || typeof e !== 'object') return null;
  const id = e.entityId || e.id;
  if (!id) return null;
  let updatedAt = e.updatedAt ?? e.createdAt ?? null;
  if (typeof updatedAt === 'number' && updatedAt < 1e12) updatedAt *= 1000; // epoch s → ms
  const attrs = e.attrs && typeof e.attrs === 'object' && !Array.isArray(e.attrs) ? e.attrs : {};
  return {
    id: String(id),
    type: ENTITY_TYPES.includes(e.type) ? e.type : 'info',
    name: String(e.name || id),
    attrs,
    relations: Array.isArray(e.relations) ? e.relations.filter((r) => r && r.targetId) : [],
    updatedAt: updatedAt ? new Date(updatedAt).getTime() || 0 : 0,
    updatedAtISO: updatedAt ? new Date(updatedAt).toISOString() : '',
  };
}

function normalizeGuide(g) {
  if (!g || typeof g !== 'object') return null;
  const id = g.guideId || g.id;
  if (!id) return null;
  return {
    id: String(id),
    title: String(g.title || 'Untitled guide'),
    text: String(g.text || g.body || ''),
    enabled: g.enabled !== false,
    priority: Number.isFinite(Number(g.priority)) ? Math.min(99, Math.max(1, Number(g.priority))) : 10,
    version: Number.isFinite(Number(g.version)) ? Number(g.version) : 1,
  };
}

const dateFmt = new Intl.DateTimeFormat([], {
  year: 'numeric', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit',
});
const fmtDate = (ms) => (ms ? dateFmt.format(new Date(ms)) : '—');

function attrsSummary(attrs) {
  const parts = [];
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === '') continue;
    parts.push(`${k}: ${typeof v === 'object' ? JSON.stringify(v) : v}`);
    if (parts.length >= 4) break;
  }
  return parts.join(' · ');
}

// ---- entities: loading ------------------------------------------------

const memLoadingEl = $('memLoading');
const memEmptyEl = $('memEmpty');
const memErrorEl = $('memError');
const memErrorMsgEl = $('memErrorMsg');
const memTableWrapEl = $('memTableWrap');
const memBodyEl = $('memBody');
const memCountEl = $('memCount');
const memMoreWrapEl = $('memMoreWrap');
const memMoreBtn = $('memMore');

function setMemState(state) {
  memLoadingEl.hidden = state !== 'loading';
  memEmptyEl.hidden = state !== 'empty';
  memErrorEl.hidden = state !== 'error';
  memTableWrapEl.hidden = state !== 'table';
}

function extractEntities(resp) {
  const raw = Array.isArray(resp.entities) ? resp.entities : Array.isArray(resp.items) ? resp.items : [];
  return raw.map(normalizeEntity).filter(Boolean);
}

async function fetchEntityPage(cursor) {
  const params = new URLSearchParams();
  if (typeFilter) params.set('type', typeFilter);
  if (cursor) params.set('cursor', cursor);
  const qs = params.toString();
  try {
    const resp = await apiJSON(`/api/v1/entities${qs ? '?' + qs : ''}`);
    return { items: extractEntities(resp), nextCursor: resp.nextCursor || resp.cursor || null };
  } catch (err) {
    // "All types" fallback: if the handler requires ?type, fan out one
    // request per enum value and merge (still Query-only server-side).
    const fannable = !typeFilter && !cursor && err instanceof ApiError &&
      (err.status === 400 || err.status === 404 || err.status === 405);
    if (!fannable) throw err;
    const pages = await Promise.all(
      ENTITY_TYPES.map((t) => apiJSON(`/api/v1/entities?type=${t}`).catch(() => null)),
    );
    if (pages.every((p) => p === null)) throw err;
    const merged = [];
    const seen = new Set();
    for (const p of pages) {
      if (!p) continue;
      for (const it of extractEntities(p)) {
        if (!seen.has(it.id)) { seen.add(it.id); merged.push(it); }
      }
    }
    return { items: merged, nextCursor: null };
  }
}

async function loadEntities({ append = false } = {}) {
  const seq = ++loadSeq;
  if (!append) {
    setMemState('loading');
    memCountEl.textContent = '';
  } else {
    memMoreBtn.disabled = true;
    memMoreBtn.textContent = 'Loading…';
  }
  try {
    const page = await fetchEntityPage(append ? nextCursor : null);
    if (seq !== loadSeq) return;
    if (append) {
      const known = new Set(entities.map((e) => e.id));
      entities = entities.concat(page.items.filter((e) => !known.has(e.id)));
    } else {
      entities = page.items;
    }
    nextCursor = page.nextCursor;
    renderEntities();
  } catch (err) {
    if (seq !== loadSeq) return;
    if (append) {
      showToast(apiErrorMessage(err, "Couldn't load more memories — try again."), { error: true });
    } else {
      memErrorMsgEl.textContent = apiErrorMessage(err, 'Check your connection and try again.');
      setMemState('error');
    }
  } finally {
    memMoreBtn.disabled = false;
    memMoreBtn.textContent = 'Load more';
  }
}

// ---- entities: rendering ----------------------------------------------

function renderEntities() {
  if (entities.length === 0) {
    setMemState('empty');
    memCountEl.textContent = '';
    memMoreWrapEl.hidden = true;
    return;
  }
  const sorted = [...entities].sort((a, b) => b.updatedAt - a.updatedAt || (a.id < b.id ? -1 : 1));
  memBodyEl.textContent = '';
  for (const e of sorted) memBodyEl.appendChild(buildEntityRow(e));
  setMemState('table');
  const label = typeFilter ? (TYPE_LABELS[typeFilter] || typeFilter).toLowerCase() : 'memor';
  const noun = typeFilter
    ? `${label} memor${entities.length === 1 ? 'y' : 'ies'}`
    : `memor${entities.length === 1 ? 'y' : 'ies'}`;
  memCountEl.textContent = nextCursor
    ? `Showing ${entities.length} ${noun} — more available`
    : `Showing ${entities.length} ${noun}`;
  memMoreWrapEl.hidden = !nextCursor;
}

function makeBtn(label, ariaLabel, cls = 'ln-btn ln-btn--ghost') {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = cls;
  b.textContent = label;
  b.setAttribute('aria-label', ariaLabel);
  return b;
}

function buildEntityRow(e) {
  const tr = document.createElement('tr');
  tr.dataset.id = e.id;

  const tdName = document.createElement('td');
  tdName.className = 'mem-name';
  tdName.textContent = e.name;
  tr.appendChild(tdName);

  const tdType = document.createElement('td');
  const badge = document.createElement('span');
  badge.className = 'ln-badge ln-badge--dot-none ln-badge--teal';
  badge.textContent = TYPE_LABELS[e.type] || e.type;
  tdType.appendChild(badge);
  tr.appendChild(tdType);

  const tdDetails = document.createElement('td');
  tdDetails.className = 'mem-details';
  const summary = attrsSummary(e.attrs);
  tdDetails.textContent = summary || '—';
  if (summary) tdDetails.title = summary;
  tr.appendChild(tdDetails);

  const tdDate = document.createElement('td');
  tdDate.className = 'mem-date';
  const time = document.createElement('time');
  if (e.updatedAtISO) time.dateTime = e.updatedAtISO;
  time.textContent = fmtDate(e.updatedAt);
  tdDate.appendChild(time);
  tr.appendChild(tdDate);

  const tdActions = document.createElement('td');
  tdActions.className = 'mem-actions-cell';
  tdActions.appendChild(entityActionButtons(e));
  tr.appendChild(tdActions);

  return tr;
}

function entityActionButtons(e) {
  const frag = document.createDocumentFragment();
  const editBtn = makeBtn('View / edit', `View or edit ${e.name}`);
  editBtn.addEventListener('click', () => openEntityDialog(e));
  frag.appendChild(editBtn);
  const forgetBtn = makeBtn('Forget', `Forget ${e.name}`, 'ln-btn ln-btn--danger');
  forgetBtn.addEventListener('click', () => confirmForget(e, forgetBtn));
  frag.appendChild(forgetBtn);
  return frag;
}

/** Two-step inline forget confirmation (destructive + irreversible: the
 * entity AND its embedding are removed — never a one-click delete). */
function confirmForget(e, trigger) {
  const cell = trigger.closest('td');
  if (!cell) return;
  cell.textContent = '';

  const wrap = document.createElement('span');
  wrap.className = 'mem-confirm';
  const q = document.createElement('span');
  q.textContent = 'Forget this memory?';
  wrap.appendChild(q);

  const yes = makeBtn('Yes, forget', `Confirm forgetting ${e.name}`, 'ln-btn ln-btn--danger');
  const cancel = makeBtn('Cancel', `Cancel forgetting ${e.name}`);
  const restore = () => {
    cell.textContent = '';
    cell.appendChild(entityActionButtons(e));
  };

  yes.addEventListener('click', async () => {
    yes.disabled = true;
    cancel.disabled = true;
    try {
      const resp = await authFetch(`/api/v1/memory/${encodeURIComponent(e.id)}`, { method: 'DELETE' });
      if (!resp.ok && resp.status !== 404) {
        throw new ApiError(resp.status, await resp.json().catch(() => null));
      }
      entities = entities.filter((x) => x.id !== e.id);
      renderEntities();
      showToast(`Forgot ${e.name}.`);
    } catch (err) {
      restore();
      showToast(apiErrorMessage(err, `Couldn't forget ${e.name} — try again.`), { error: true });
    }
  });
  cancel.addEventListener('click', restore);
  wrap.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape') {
      ev.stopPropagation();
      restore();
    }
  });

  wrap.appendChild(yes);
  wrap.appendChild(cancel);
  cell.appendChild(wrap);
  yes.focus();
}

// ---- entity dialog (view / edit / create) -----------------------------

const entityDialog = $('entityDialog');
const entityForm = $('entityForm');
const entityDialogTitle = $('entityDialogTitle');
const entityNameInput = $('entityName');
const entityNameErr = $('entityNameErr');
const entityTypeSelect = $('entityType');
const entityTypeHint = $('entityTypeHint');
const entityAttrRows = $('entityAttrRows');
const entityRelField = $('entityRelField');
const entityRelList = $('entityRelList');
const entitySaveBtn = $('entitySave');

let dialogEntity = null; // null = creating a new memory

function addAttrRow(key = '', value = '') {
  const row = document.createElement('div');
  row.className = 'mem-attr-row';

  const k = document.createElement('input');
  k.type = 'text';
  k.className = 'ln-input mem-attr-key';
  k.placeholder = 'Detail (e.g. birthday)';
  k.maxLength = 80;
  k.value = key;
  k.setAttribute('aria-label', 'Detail name');

  const v = document.createElement('input');
  v.type = 'text';
  v.className = 'ln-input mem-attr-val';
  v.placeholder = 'Value (e.g. March 14)';
  v.maxLength = 500;
  v.value = value;
  v.setAttribute('aria-label', 'Detail value');

  const rm = makeBtn('Remove', `Remove the ${key || 'new'} detail`);
  rm.addEventListener('click', () => row.remove());

  row.appendChild(k);
  row.appendChild(v);
  row.appendChild(rm);
  entityAttrRows.appendChild(row);
  return row;
}

function relationLabel(rel) {
  const target = entities.find((x) => x.id === rel.targetId);
  const name = target ? target.name : rel.targetId;
  return rel.type ? `${rel.type} → ${name}` : `→ ${name}`;
}

function openEntityDialog(e) {
  dialogEntity = e || null;
  entityDialogTitle.textContent = e ? `Edit “${e.name}”` : 'New memory';
  entityNameInput.value = e ? e.name : '';
  entityNameInput.classList.remove('is-invalid');
  entityNameErr.hidden = true;
  entityTypeSelect.value = e ? e.type : 'info';
  entityTypeSelect.disabled = !!e; // stable id encodes the type (ENT#<type>#<id>)
  entityTypeHint.hidden = !e;

  entityAttrRows.textContent = '';
  const attrs = e ? Object.entries(e.attrs) : [];
  for (const [k, v] of attrs) {
    addAttrRow(k, typeof v === 'object' ? JSON.stringify(v) : String(v));
  }
  if (attrs.length === 0) addAttrRow();

  const rels = e ? e.relations : [];
  entityRelField.hidden = rels.length === 0;
  entityRelList.textContent = '';
  for (const rel of rels) {
    const chip = document.createElement('span');
    chip.className = 'ln-badge ln-badge--dot-none';
    chip.textContent = relationLabel(rel);
    entityRelList.appendChild(chip);
  }

  entityDialog.showModal();
  entityNameInput.focus();
}

$('entityAttrAdd').addEventListener('click', () => {
  const row = addAttrRow();
  row.querySelector('input').focus();
});
$('entityCancel').addEventListener('click', () => entityDialog.close());
$('memNew').addEventListener('click', () => openEntityDialog(null));

entityNameInput.addEventListener('input', () => {
  if (entityNameInput.value.trim()) {
    entityNameInput.classList.remove('is-invalid');
    entityNameErr.hidden = true;
  }
});

entityForm.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const name = entityNameInput.value.trim();
  if (!name) {
    entityNameInput.classList.add('is-invalid');
    entityNameErr.hidden = false;
    entityNameInput.focus();
    return;
  }
  const attrs = {};
  for (const row of entityAttrRows.querySelectorAll('.mem-attr-row')) {
    const [k, v] = row.querySelectorAll('input');
    const key = k.value.trim();
    if (key) attrs[key] = v.value.trim();
  }
  const body = {
    type: dialogEntity ? dialogEntity.type : entityTypeSelect.value,
    name,
    attrs,
  };
  if (dialogEntity) body.entityId = dialogEntity.id;

  entitySaveBtn.disabled = true;
  entitySaveBtn.setAttribute('aria-busy', 'true');
  try {
    await apiJSON('/api/v1/memory', { method: 'POST', json: body });
    entityDialog.close();
    showToast(dialogEntity ? `Updated ${name}.` : `Remembered ${name}.`);
    loadEntities();
  } catch (err) {
    showToast(apiErrorMessage(err, "Couldn't save the memory — try again."), { error: true });
  } finally {
    entitySaveBtn.disabled = false;
    entitySaveBtn.removeAttribute('aria-busy');
  }
});

// ---- guides -----------------------------------------------------------

const guideLoadingEl = $('guideLoading');
const guideEmptyEl = $('guideEmpty');
const guideErrorEl = $('guideError');
const guideErrorMsgEl = $('guideErrorMsg');
const guideListEl = $('guideList');

function setGuideState(state) {
  guideLoadingEl.hidden = state !== 'loading';
  guideEmptyEl.hidden = state !== 'empty';
  guideErrorEl.hidden = state !== 'error';
  guideListEl.hidden = state !== 'list';
}

async function loadGuides() {
  const seq = ++guideLoadSeq;
  setGuideState('loading');
  try {
    const resp = await apiJSON('/api/v1/guides');
    if (seq !== guideLoadSeq) return;
    const raw = Array.isArray(resp.guides) ? resp.guides : Array.isArray(resp.items) ? resp.items : [];
    guides = raw.map(normalizeGuide).filter(Boolean);
    renderGuides();
  } catch (err) {
    if (seq !== guideLoadSeq) return;
    guideErrorMsgEl.textContent = apiErrorMessage(err, 'Check your connection and try again.');
    setGuideState('error');
  }
}

function renderGuides() {
  if (guides.length === 0) {
    setGuideState('empty');
    return;
  }
  const sorted = [...guides].sort((a, b) => a.priority - b.priority || a.title.localeCompare(b.title));
  guideListEl.textContent = '';
  for (const g of sorted) guideListEl.appendChild(buildGuideRow(g));
  setGuideState('list');
}

/** PUT the full guide document (the contract's create-or-edit route). */
async function putGuide(g, patch) {
  const body = {
    title: g.title,
    text: g.text,
    enabled: g.enabled,
    priority: g.priority,
    version: g.version,
    ...patch,
  };
  const resp = await apiJSON(`/api/v1/guides/${encodeURIComponent(g.id)}`, { method: 'PUT', json: body });
  const updated = normalizeGuide({ ...body, ...(resp && typeof resp === 'object' ? resp : {}), id: g.id });
  const idx = guides.findIndex((x) => x.id === g.id);
  if (idx >= 0) guides[idx] = updated;
  else guides.push(updated);
  return updated;
}

function buildGuideRow(g) {
  const row = document.createElement('div');
  row.className = 'mem-guide-row';
  row.dataset.id = g.id;

  // Enable toggle — live effect (applies to the next session).
  const toggle = document.createElement('label');
  toggle.className = 'ln-toggle';
  const cb = document.createElement('input');
  cb.type = 'checkbox';
  cb.checked = g.enabled;
  cb.setAttribute('aria-label', `Enable the guide ${g.title}`);
  const track = document.createElement('span');
  track.className = 'ln-toggle-track';
  track.setAttribute('aria-hidden', 'true');
  const thumb = document.createElement('span');
  thumb.className = 'ln-toggle-thumb';
  track.appendChild(thumb);
  toggle.appendChild(cb);
  toggle.appendChild(track);
  cb.addEventListener('change', async () => {
    const want = cb.checked;
    cb.disabled = true;
    try {
      await putGuide(g, { enabled: want });
      showToast(want ? `Enabled “${g.title}” — applies to your next session.` : `Disabled “${g.title}”.`);
      renderGuides();
    } catch (err) {
      cb.checked = !want; // revert on failure — never lie about state
      showToast(apiErrorMessage(err, "Couldn't update the guide — try again."), { error: true });
    } finally {
      cb.disabled = false;
    }
  });
  row.appendChild(toggle);

  const body = document.createElement('div');
  body.className = 'mem-guide-body';
  const title = document.createElement('div');
  title.className = 'mem-guide-title';
  title.textContent = g.title;
  const ver = document.createElement('span');
  ver.className = 'ln-badge ln-badge--dot-none';
  ver.textContent = `v${g.version}`;
  title.appendChild(ver);
  if (!g.enabled) {
    const off = document.createElement('span');
    off.className = 'ln-badge ln-badge--dot-none';
    off.textContent = 'Disabled';
    title.appendChild(off);
  }
  body.appendChild(title);
  const text = document.createElement('p');
  text.className = 'mem-guide-text';
  text.textContent = g.text;
  body.appendChild(text);
  row.appendChild(body);

  const side = document.createElement('div');
  side.className = 'mem-guide-side';

  // Priority — bounded number → native stepper, saved on change.
  const prioField = document.createElement('div');
  prioField.className = 'ln-field';
  const prioLabel = document.createElement('label');
  const prioId = `guidePrio-${g.id}`;
  prioLabel.setAttribute('for', prioId);
  prioLabel.textContent = 'Priority';
  const prio = document.createElement('input');
  prio.type = 'number';
  prio.className = 'ln-input mem-guide-priority';
  prio.id = prioId;
  prio.min = '1';
  prio.max = '99';
  prio.step = '1';
  prio.inputMode = 'numeric';
  prio.value = String(g.priority);
  prio.addEventListener('change', async () => {
    const val = Math.min(99, Math.max(1, Number(prio.value) || g.priority));
    prio.value = String(val);
    if (val === g.priority) return;
    prio.disabled = true;
    try {
      await putGuide(g, { priority: val });
      renderGuides();
    } catch (err) {
      prio.value = String(g.priority);
      showToast(apiErrorMessage(err, "Couldn't update the priority — try again."), { error: true });
    } finally {
      prio.disabled = false;
    }
  });
  prioField.appendChild(prioLabel);
  prioField.appendChild(prio);
  side.appendChild(prioField);

  const editBtn = makeBtn('Edit', `Edit the guide ${g.title}`);
  editBtn.addEventListener('click', () => openGuideDialog(g));
  side.appendChild(editBtn);

  row.appendChild(side);
  return row;
}

// ---- guide dialog -----------------------------------------------------

const guideDialog = $('guideDialog');
const guideForm = $('guideForm');
const guideDialogTitle = $('guideDialogTitle');
const guideTitleInput = $('guideTitle');
const guideTitleErr = $('guideTitleErr');
const guideTextInput = $('guideText');
const guideTextErr = $('guideTextErr');
const guidePriorityInput = $('guidePriority');
const guideEnabledInput = $('guideEnabled');
const guideSaveBtn = $('guideSave');

let dialogGuide = null; // null = creating

function openGuideDialog(g) {
  dialogGuide = g || null;
  guideDialogTitle.textContent = g ? `Edit “${g.title}”` : 'New guide';
  guideTitleInput.value = g ? g.title : '';
  guideTextInput.value = g ? g.text : '';
  guidePriorityInput.value = String(g ? g.priority : 10);
  guideEnabledInput.checked = g ? g.enabled : true;
  guideTitleInput.classList.remove('is-invalid');
  guideTitleErr.hidden = true;
  guideTextInput.classList.remove('is-invalid');
  guideTextErr.hidden = true;
  guideDialog.showModal();
  guideTitleInput.focus();
}

$('guideNew').addEventListener('click', () => openGuideDialog(null));
$('guideCancel').addEventListener('click', () => guideDialog.close());

guideTitleInput.addEventListener('input', () => {
  if (guideTitleInput.value.trim()) {
    guideTitleInput.classList.remove('is-invalid');
    guideTitleErr.hidden = true;
  }
});
guideTextInput.addEventListener('input', () => {
  if (guideTextInput.value.trim()) {
    guideTextInput.classList.remove('is-invalid');
    guideTextErr.hidden = true;
  }
});

guideForm.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const title = guideTitleInput.value.trim();
  const text = guideTextInput.value.trim();
  let invalid = false;
  if (!title) {
    guideTitleInput.classList.add('is-invalid');
    guideTitleErr.hidden = false;
    invalid = true;
  }
  if (!text) {
    guideTextInput.classList.add('is-invalid');
    guideTextErr.hidden = false;
    invalid = true;
  }
  if (invalid) {
    (title ? guideTextInput : guideTitleInput).focus();
    return;
  }
  const priority = Math.min(99, Math.max(1, Number(guidePriorityInput.value) || 10));
  const target = dialogGuide || { id: randomId(), title: '', text: '', enabled: true, priority, version: 0 };

  guideSaveBtn.disabled = true;
  guideSaveBtn.setAttribute('aria-busy', 'true');
  try {
    await putGuide(target, { title, text, priority, enabled: guideEnabledInput.checked });
    guideDialog.close();
    showToast(dialogGuide ? `Updated “${title}”.` : `Created “${title}” — it applies to your next session.`);
    loadGuides(); // re-fetch for the server's canonical version number
  } catch (err) {
    showToast(apiErrorMessage(err, "Couldn't save the guide — try again."), { error: true });
  } finally {
    guideSaveBtn.disabled = false;
    guideSaveBtn.removeAttribute('aria-busy');
  }
});

// ---- init -------------------------------------------------------------

$('memTypeFilter').addEventListener('change', (ev) => {
  typeFilter = ev.target.value;
  nextCursor = null;
  loadEntities();
});
$('memRefresh').addEventListener('click', () => loadEntities());
$('memRetry').addEventListener('click', () => loadEntities());
memMoreBtn.addEventListener('click', () => loadEntities({ append: true }));
$('guideRetry').addEventListener('click', () => loadGuides());

loadEntities();
loadGuides();
