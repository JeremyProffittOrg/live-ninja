// personas.mjs — the /personas management page (personas platform
// feature). Data comes from the grouped persona library endpoint:
//
//   GET    /api/v1/personas            {builtin:[], mine:[], shared:[]}
//   POST   /api/v1/personas            create (or duplicate via {copyOf})
//   PUT    /api/v1/personas/{id}       edit own persona
//   DELETE /api/v1/personas/{id}       delete own persona (typed-name confirm)
//   POST   /api/v1/personas/{id}/share {shared: bool}
//
// House rules honored here: the voice control is a <select> populated from
// GET /api/v1/realtime/voices (never a blind text box); instructions are
// genuinely free-form persona style text (the one legitimate free-text
// case); deletes are guarded by a typed-name confirmation; every
// data-driven node is built with textContent (never innerHTML).

import { apiJSON, ApiError } from './toolclient.mjs';

const PERSONAS_PATH = '/api/v1/personas';
const VOICES_PATH = '/api/v1/realtime/voices';

const $ = (id) => document.getElementById(id);

// ---- toast ---------------------------------------------------------------

const toastEl = $('toast');
const toastMsgEl = $('toastMsg');
let toastTimer = 0;

function toast(msg, { error = false } = {}) {
  if (!toastEl || !toastMsgEl) return;
  clearTimeout(toastTimer);
  toastMsgEl.textContent = msg;
  toastEl.classList.toggle('is-error', !!error);
  toastEl.hidden = false;
  toastEl.classList.add('is-visible');
  toastTimer = setTimeout(() => {
    toastEl.classList.remove('is-visible');
    toastEl.hidden = true;
  }, error ? 6000 : 3500);
}

function errText(err, fallback) {
  return err instanceof ApiError && err.message ? err.message : fallback;
}

// ---- state ---------------------------------------------------------------

let library = { builtin: [], mine: [], shared: [] };
let voices = []; // [{id, name, description, default}]

const loadingEl = $('perLoading');
const errorEl = $('perError');
const errorMsgEl = $('perErrorMsg');
const tableWrapEl = $('perTableWrap');
const bodyEl = $('perBody');
const hintEl = $('perHint');

function voiceName(id) {
  if (!id) return '—';
  const v = voices.find((row) => row.id === id);
  return v ? v.name : id;
}

// ---- load ----------------------------------------------------------------

async function load() {
  loadingEl.hidden = false;
  errorEl.hidden = true;
  tableWrapEl.hidden = true;
  hintEl.hidden = true;
  try {
    const [lib, vs] = await Promise.all([
      apiJSON(PERSONAS_PATH),
      voices.length ? Promise.resolve(null) : apiJSON(VOICES_PATH),
    ]);
    library = {
      builtin: Array.isArray(lib.builtin) ? lib.builtin : [],
      mine: Array.isArray(lib.mine) ? lib.mine : [],
      shared: Array.isArray(lib.shared) ? lib.shared : [],
    };
    if (vs && Array.isArray(vs.voices)) {
      voices = vs.voices;
      fillVoiceSelect();
    }
    render();
    loadingEl.hidden = true;
    tableWrapEl.hidden = false;
    hintEl.hidden = false;
  } catch (err) {
    if (err && err.name === 'AuthLostError') return; // redirecting
    loadingEl.hidden = true;
    errorEl.hidden = false;
    errorMsgEl.textContent = errText(err, 'Check your connection and try again.');
  }
}

// ---- table render --------------------------------------------------------

function badge(kind, ownerName) {
  const td = document.createElement('td');
  const b = document.createElement('span');
  b.className = `per-badge per-badge--${kind}`;
  b.textContent = kind === 'builtin' ? 'Built-in' : kind === 'mine' ? 'Mine' : 'Shared';
  td.appendChild(b);
  if (ownerName) {
    const who = document.createElement('span');
    who.className = 'per-owner';
    who.textContent = `by ${ownerName}`;
    td.appendChild(who);
  }
  return td;
}

function actionBtn(label, ghost, onClick) {
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = ghost ? 'ln-btn ln-btn--ghost' : 'ln-btn ln-btn--danger';
  btn.textContent = label;
  btn.addEventListener('click', onClick);
  return btn;
}

function textTd(className, text) {
  const td = document.createElement('td');
  td.className = className;
  td.textContent = text;
  return td;
}

function renderRow(p, kind) {
  const tr = document.createElement('tr');
  tr.appendChild(textTd('per-name', p.name || p.id));
  tr.appendChild(badge(kind, kind === 'shared' ? p.owner : ''));
  tr.appendChild(textTd('per-desc', p.description || ''));
  tr.appendChild(textTd('per-voice', voiceName(p.voice)));

  // Shared column: a real toggle for "mine", a plain marker otherwise.
  const shareTd = document.createElement('td');
  shareTd.className = 'per-share-cell';
  if (kind === 'mine') {
    const label = document.createElement('label');
    label.className = 'ln-toggle';
    const input = document.createElement('input');
    input.type = 'checkbox';
    input.checked = !!p.shared;
    input.setAttribute('aria-label', `Share “${p.name}” with everyone`);
    const track = document.createElement('span');
    track.className = 'ln-toggle-track';
    track.setAttribute('aria-hidden', 'true');
    const thumb = document.createElement('span');
    thumb.className = 'ln-toggle-thumb';
    track.appendChild(thumb);
    label.appendChild(input);
    label.appendChild(track);
    input.addEventListener('change', () => void toggleShare(p, input));
    shareTd.appendChild(label);
  } else {
    shareTd.textContent = kind === 'shared' ? 'Yes' : '—';
  }
  tr.appendChild(shareTd);

  const actions = document.createElement('td');
  actions.className = 'per-actions-cell';
  if (kind === 'builtin') {
    actions.appendChild(actionBtn('Duplicate', true, () => void duplicate(p)));
  } else if (kind === 'mine') {
    actions.appendChild(actionBtn('Edit', true, () => openEditor(p)));
    actions.appendChild(actionBtn('Duplicate', true, () => void duplicate(p)));
    actions.appendChild(actionBtn('Delete', false, () => openDeleteConfirm(p)));
  } else {
    actions.appendChild(actionBtn('Copy to mine', true, () => void duplicate(p)));
  }
  tr.appendChild(actions);
  return tr;
}

function render() {
  bodyEl.replaceChildren();
  for (const p of library.builtin) bodyEl.appendChild(renderRow(p, 'builtin'));
  for (const p of library.mine) bodyEl.appendChild(renderRow(p, 'mine'));
  for (const p of library.shared) bodyEl.appendChild(renderRow(p, 'shared'));
}

// ---- share toggle --------------------------------------------------------

async function toggleShare(p, input) {
  const next = input.checked;
  input.disabled = true;
  try {
    const updated = await apiJSON(`${PERSONAS_PATH}/${encodeURIComponent(p.id)}/share`, {
      method: 'POST',
      json: { shared: next },
    });
    p.shared = !!updated.shared;
    toast(next ? `“${p.name}” is now shared with everyone.` : `“${p.name}” is no longer shared.`);
  } catch (err) {
    input.checked = !next; // revert
    toast(errText(err, "Couldn't update sharing — try again."), { error: true });
  } finally {
    input.disabled = false;
  }
}

// ---- duplicate / copy-to-mine -------------------------------------------

async function duplicate(p) {
  try {
    const created = await apiJSON(PERSONAS_PATH, { method: 'POST', json: { copyOf: p.id } });
    toast(`Created “${created.name}” in your personas.`);
    await load();
  } catch (err) {
    toast(errText(err, "Couldn't copy that persona — try again."), { error: true });
  }
}

// ---- create/edit dialog --------------------------------------------------

const dialogEl = $('perDialog');
const dialogTitleEl = $('perDialogTitle');
const formEl = $('perForm');
const nameEl = $('perName');
const nameErrEl = $('perNameErr');
const descEl = $('perDesc');
const instrEl = $('perInstructions');
const instrErrEl = $('perInstrErr');
const instrCountEl = $('perInstrCount');
const voiceEl = $('perVoice');
const saveBtn = $('perSave');

let editingId = ''; // '' = creating

function fillVoiceSelect() {
  if (!voiceEl) return;
  const keep = voiceEl.value;
  voiceEl.replaceChildren();
  const none = document.createElement('option');
  none.value = '';
  none.textContent = 'No suggestion — use my voice setting';
  voiceEl.appendChild(none);
  for (const v of voices) {
    const opt = document.createElement('option');
    opt.value = v.id;
    opt.textContent = v.description ? `${v.name} — ${v.description}` : v.name;
    voiceEl.appendChild(opt);
  }
  voiceEl.value = keep;
}

function syncInstrCount() {
  if (instrCountEl) instrCountEl.textContent = String(instrEl.value.length);
}
instrEl.addEventListener('input', () => {
  syncInstrCount();
  if (instrEl.value.trim()) instrErrEl.hidden = true;
});
nameEl.addEventListener('input', () => {
  if (nameEl.value.trim()) nameErrEl.hidden = true;
});

function openEditor(p) {
  editingId = p ? p.id : '';
  dialogTitleEl.textContent = p ? `Edit “${p.name}”` : 'New persona';
  nameEl.value = p ? p.name : '';
  descEl.value = p ? p.description || '' : '';
  instrEl.value = p ? p.instructions || '' : '';
  voiceEl.value = p ? p.voice || '' : '';
  if (voiceEl.value !== (p ? p.voice || '' : '')) voiceEl.value = ''; // unknown stored voice
  nameErrEl.hidden = true;
  instrErrEl.hidden = true;
  syncInstrCount();
  dialogEl.showModal();
  nameEl.focus();
}

formEl.addEventListener('submit', (e) => {
  e.preventDefault();
  const name = nameEl.value.trim();
  const instructions = instrEl.value.trim();
  let bad = false;
  if (!name) {
    nameErrEl.hidden = false;
    bad = true;
  }
  if (!instructions) {
    instrErrEl.hidden = false;
    bad = true;
  }
  if (bad) {
    (name ? instrEl : nameEl).focus();
    return;
  }
  void savePersona({
    name,
    description: descEl.value.trim(),
    instructions,
    voice: voiceEl.value,
  });
});

async function savePersona(payload) {
  saveBtn.disabled = true;
  try {
    if (editingId) {
      await apiJSON(`${PERSONAS_PATH}/${encodeURIComponent(editingId)}`, {
        method: 'PUT',
        json: payload,
      });
      toast('Persona saved.');
    } else {
      await apiJSON(PERSONAS_PATH, { method: 'POST', json: payload });
      toast('Persona created.');
    }
    dialogEl.close();
    await load();
  } catch (err) {
    toast(errText(err, "Couldn't save the persona — try again."), { error: true });
  } finally {
    saveBtn.disabled = false;
  }
}

$('perCancel').addEventListener('click', () => dialogEl.close());
$('perNew').addEventListener('click', () => openEditor(null));

// ---- delete (typed-name confirm) ----------------------------------------

const delDialogEl = $('perDeleteDialog');
const delFormEl = $('perDeleteForm');
const delNameEl = $('perDeleteName');
const delSharedNoteEl = $('perDeleteSharedNote');
const delConfirmEl = $('perDeleteConfirm');
const delErrEl = $('perDeleteErr');
const delGoBtn = $('perDeleteGo');

let deleting = null; // the persona row pending deletion

function openDeleteConfirm(p) {
  deleting = p;
  delNameEl.textContent = `“${p.name}”`;
  delSharedNoteEl.hidden = !p.shared;
  delConfirmEl.value = '';
  delErrEl.hidden = true;
  delGoBtn.disabled = true;
  delDialogEl.showModal();
  delConfirmEl.focus();
}

delConfirmEl.addEventListener('input', () => {
  const match = deleting && delConfirmEl.value.trim() === deleting.name;
  delGoBtn.disabled = !match;
  if (match) delErrEl.hidden = true;
});

delFormEl.addEventListener('submit', (e) => {
  e.preventDefault();
  if (!deleting) return;
  if (delConfirmEl.value.trim() !== deleting.name) {
    delErrEl.hidden = false;
    delConfirmEl.focus();
    return;
  }
  void deletePersona(deleting);
});

async function deletePersona(p) {
  delGoBtn.disabled = true;
  try {
    await apiJSON(`${PERSONAS_PATH}/${encodeURIComponent(p.id)}`, { method: 'DELETE' });
    delDialogEl.close();
    deleting = null;
    toast(`Deleted “${p.name}”.`);
    await load();
  } catch (err) {
    toast(errText(err, "Couldn't delete the persona — try again."), { error: true });
    delGoBtn.disabled = false;
  }
}

$('perDeleteCancel').addEventListener('click', () => {
  deleting = null;
  delDialogEl.close();
});

// ---- wiring --------------------------------------------------------------

$('perRefresh').addEventListener('click', () => void load());
$('perRetry').addEventListener('click', () => void load());

void load();
