// personaeditor.mjs — the persona editor pop-up ("we need a persona, not a
// voice"). Personas are the unit of voice identity: every persona carries
// its own voice AND accent, stored in settings.personaPrefs[personaId]
// (contracts/settings.schema.json), and custom personas additionally edit
// their name/description/instructions through the persona CRUD API.
//
//   openPersonaEditor(personaId)  — the single export; the conversation
//     page shell calls it from the Edit button next to the persona picker.
//
// Markup lives in templates/partials/persona_editor.html (a native
// <dialog id="personaEditor">; showModal() supplies the focus trap and
// Escape-to-close). This module populates the Voice/Accent selects from
// GET /api/v1/realtime/voices (populated controls, never blind text),
// toggles the field groups per persona kind:
//   - own custom persona  → editable name/description/instructions;
//   - built-in / shared   → read-only text + "Duplicate to edit text"
//     (POST /api/v1/personas {copyOf} — the server-side copy API, the only
//     way built-in/shared instruction text reaches an editable persona);
//   - the "custom" settings preset → voice/accent only (its free-text
//     instructions live on the Settings page);
// and saves:
//   - persona text via PUT /api/v1/personas/:id (own personas only);
//   - voice/accent into personaPrefs[personaId] via the settings PUT with
//     the optimistic-concurrency 409 retry-once rule (same contract as
//     conversation.mjs/settings.mjs — re-GET, re-apply, retry once).
// On success it dispatches a 'personachanged' CustomEvent on window
// (detail: {personaId, voice, accent}) so conversation.mjs refreshes its
// select labels, and pings the ln.settings.version cross-tab channel.

import { apiJSON, ApiError } from './toolclient.mjs';

const SETTINGS_PATH = '/api/v1/settings';
const VOICES_PATH = '/api/v1/realtime/voices';
const PERSONAS_PATH = '/api/v1/personas';
const SETTINGS_PING_KEY = 'ln.settings.version';

const $ = (id) => document.getElementById(id);

// Module-level open guard: a second openPersonaEditor while the dialog is
// up (double-click, duplicate listener) is a no-op instead of a crashed
// showModal().
let isOpen = false;
let wired = false;

function setError(msg) {
  const el = $('personaEditorError');
  if (!el) return;
  el.textContent = msg || '';
  el.hidden = !msg;
}

function setBusy(busy) {
  const save = $('personaEditorSave');
  if (!save) return;
  save.disabled = busy;
  if (busy) save.setAttribute('aria-busy', 'true');
  else save.removeAttribute('aria-busy');
}

function fillCatalogSelect(sel, rows, selectedId, labelOf) {
  sel.replaceChildren();
  let found = false;
  for (const row of rows) {
    const opt = document.createElement('option');
    opt.value = row.id;
    opt.textContent = labelOf(row);
    if (row.id === selectedId) {
      opt.selected = true;
      found = true;
    }
    sel.appendChild(opt);
  }
  if (!found && selectedId) {
    // Forward-compat: an unknown stored value is kept selectable, never
    // silently dropped (settings.schema.json rule).
    const opt = document.createElement('option');
    opt.value = selectedId;
    opt.textContent = `${selectedId} (kept as-is)`;
    opt.selected = true;
    sel.appendChild(opt);
  }
}

/** Persona-kind classification from the GET /api/v1/personas/:id shapes
 * (personas_routes.go): own personas carry their instructions text;
 * built-ins carry builtin:true; everything else reachable is shared. */
function kindOf(personaId, persona) {
  if (personaId === 'custom') return 'custom-preset';
  if (persona && persona.builtin) return 'builtin';
  if (persona && typeof persona.instructions === 'string') return 'own';
  return 'shared';
}

/** Load the persona record; the settings "custom" preset is a client-side
 * concept with no server persona, so it synthesizes locally. */
async function loadPersona(personaId) {
  if (personaId === 'custom') {
    return {
      id: 'custom',
      name: 'Custom instructions',
      description: 'Your free-text instructions from the Settings page.',
      voice: '',
    };
  }
  return apiJSON(`${PERSONAS_PATH}/${encodeURIComponent(personaId)}`);
}

/** Write personaPrefs[personaId] = {voice, accent, updatedAt} through the
 * settings PUT with the 409 retry-once rule. Unknown sibling fields on the
 * entry (and everywhere else in the document) are preserved. */
async function savePersonaPrefs(settingsDoc, personaId, voice, accent) {
  const attempt = async (doc) => {
    const version = Number(doc.version) || 1;
    const settings = structuredClone(doc);
    delete settings.version;
    if (!settings.personaPrefs || typeof settings.personaPrefs !== 'object') {
      settings.personaPrefs = {};
    }
    settings.personaPrefs[personaId] = {
      ...(typeof settings.personaPrefs[personaId] === 'object' ? settings.personaPrefs[personaId] : {}),
      voice,
      accent,
      updatedAt: new Date().toISOString(),
    };
    const resp = await apiJSON(SETTINGS_PATH, {
      method: 'PUT',
      json: { settings, version },
    });
    try {
      // Cross-tab channel: other tabs re-GET the doc (conversation.mjs).
      localStorage.setItem(SETTINGS_PING_KEY, String(resp.version));
    } catch {
      /* storage blocked — cross-tab sync degrades gracefully */
    }
    return resp;
  };

  try {
    return await attempt(settingsDoc);
  } catch (err) {
    if (!(err instanceof ApiError) || err.code !== 'version_conflict') throw err;
    // Another surface wrote first: re-read, re-apply this one edit on the
    // fresh document, retry once. A second conflict surfaces to the caller.
    const fresh = await apiJSON(SETTINGS_PATH);
    return attempt(fresh);
  }
}

/**
 * Open the persona editor for one persona. Resolves when the dialog has
 * opened (not when it closes). Safe to call repeatedly — a second call
 * while open is ignored.
 * @param {string} personaId persona.presetId-space id: a built-in id, one
 *   of the user's own custom-persona ids, a shared-catalog id, or "custom".
 */
export async function openPersonaEditor(personaId) {
  const dlg = $('personaEditor');
  if (!dlg) {
    throw new Error('personaeditor: #personaEditor dialog missing — is the persona_editor partial included on this page?');
  }
  if (isOpen || dlg.open) return;
  isOpen = true;

  // State for the current open (captured by the wired handlers via `state`).
  const state = {
    personaId: String(personaId || 'default'),
    kind: 'builtin',
    persona: null,
    settingsDoc: null,
    original: { name: '', description: '', instructions: '' },
  };
  dlg.__peState = state;

  if (!wired) {
    wired = true;
    $('personaEditorCancel').addEventListener('click', () => dlg.close());
    dlg.addEventListener('close', () => {
      isOpen = false;
    });
    const instr = $('peInstructions');
    instr.addEventListener('input', () => {
      $('peInstrCount').textContent = `${instr.value.length} / 4000`;
    });
    $('peDuplicateBtn').addEventListener('click', () => void duplicatePersona(dlg));
    $('personaEditorSave').addEventListener('click', () => void savePersona(dlg));
  }

  // Loading state first, then showModal (native focus trap + Escape).
  $('personaEditorLoading').hidden = false;
  $('personaEditorFields').hidden = true;
  $('personaEditorTitle').textContent = 'Edit persona';
  $('personaEditorSubtitle').textContent = '';
  setError('');
  setBusy(false);
  dlg.showModal();

  let persona;
  let settingsDoc;
  let catalogs;
  try {
    [persona, settingsDoc, catalogs] = await Promise.all([
      loadPersona(state.personaId),
      apiJSON(SETTINGS_PATH),
      apiJSON(VOICES_PATH),
    ]);
  } catch (err) {
    if (err && err.name === 'AuthLostError') {
      dlg.close();
      return;
    }
    $('personaEditorLoading').hidden = true;
    setError(
      (err instanceof ApiError && err.status === 404)
        ? 'This persona no longer exists — it may have been deleted or unshared.'
        : "Couldn't load this persona — check your connection and try again.",
    );
    return;
  }

  state.persona = persona;
  state.settingsDoc = settingsDoc;
  state.kind = kindOf(state.personaId, persona);
  renderEditor(dlg, state, catalogs);
}

function renderEditor(dlg, state, catalogs) {
  const { persona, settingsDoc, kind } = state;
  const voices = Array.isArray(catalogs && catalogs.voices) ? catalogs.voices : [];
  const accents = Array.isArray(catalogs && catalogs.accents) ? catalogs.accents : [];

  $('personaEditorTitle').textContent = persona.name || state.personaId;
  $('personaEditorSubtitle').textContent =
    kind === 'own'
      ? 'Your custom persona — text, voice, and accent are all yours to edit.'
      : kind === 'shared'
        ? `Shared by ${persona.owner || 'another user'} — pick its voice and accent for your conversations.`
        : kind === 'custom-preset'
          ? 'Voice and accent for your custom instructions (edit the text on the Settings page).'
          : 'Built-in persona — pick its voice and accent for your conversations.';

  // Field groups per kind.
  const own = kind === 'own';
  $('peNameField').hidden = !own;
  $('peDescField').hidden = !own;
  $('peInstrField').hidden = !own;
  $('peReadonly').hidden = own || kind === 'custom-preset';
  if (own) {
    $('peName').value = persona.name || '';
    $('peDescription').value = persona.description || '';
    $('peInstructions').value = persona.instructions || '';
    $('peInstrCount').textContent = `${$('peInstructions').value.length} / 4000`;
    state.original = {
      name: persona.name || '',
      description: persona.description || '',
      instructions: persona.instructions || '',
    };
  } else if (kind !== 'custom-preset') {
    $('peReadonlyDesc').textContent = persona.description || '';
  }

  // Voice select: personaPrefs entry ?? the persona's suggested voice ??
  // the account fallback ?? cedar — mirroring the broker's mint chain
  // (internal/realtime/voiceprefs.go) so what's shown is what will sound.
  const prefs =
    settingsDoc && typeof settingsDoc.personaPrefs === 'object' && settingsDoc.personaPrefs
      ? settingsDoc.personaPrefs
      : {};
  const entry = typeof prefs[state.personaId] === 'object' && prefs[state.personaId] ? prefs[state.personaId] : null;
  const selectedVoice =
    (entry && typeof entry.voice === 'string' && entry.voice) ||
    (persona.voice || '') ||
    (typeof settingsDoc.voice === 'string' && settingsDoc.voice) ||
    'cedar';
  fillCatalogSelect(
    $('peVoice'), voices, selectedVoice,
    (v) => `${v.name || v.id}${v.gender ? ` (${v.gender})` : ''}${v.description ? ` — ${v.description}` : ''}`,
  );

  // Accent select: an entry whose `accent` key is PRESENT wins even when
  // "" (explicitly no accent); otherwise the account fallback applies.
  const hasEntryAccent = !!entry && Object.prototype.hasOwnProperty.call(entry, 'accent') && typeof entry.accent === 'string';
  const storedAccent = hasEntryAccent
    ? entry.accent
    : (typeof settingsDoc.voiceAccent === 'string' ? settingsDoc.voiceAccent : '');
  fillCatalogSelect($('peAccent'), accents, storedAccent || 'none', (a) => a.label || a.id);

  $('personaEditorLoading').hidden = true;
  $('personaEditorFields').hidden = false;
  setError('');
  // Autofocus the first meaningful control for keyboard users.
  (own ? $('peName') : $('peVoice')).focus();
}

/** Duplicate a built-in/shared persona server-side and switch the editor
 * to the fresh, fully-editable copy (the copy API is the only path that
 * moves built-in/shared instruction text into an editable persona). */
async function duplicatePersona(dlg) {
  const state = dlg.__peState;
  if (!state) return;
  const btn = $('peDuplicateBtn');
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  setError('');
  try {
    const copy = await apiJSON(PERSONAS_PATH, {
      method: 'POST',
      json: { copyOf: state.personaId },
    });
    // The library changed (a new "Mine" persona) — let the page refresh
    // its persona select before any save happens.
    window.dispatchEvent(new CustomEvent('personachanged', { detail: { personaId: copy.id } }));
    state.personaId = copy.id;
    state.persona = copy;
    state.kind = 'own';
    // Re-render text fields as editable; keep the user's current voice/
    // accent selections — they save under the copy's id.
    const keepVoice = $('peVoice').value;
    const keepAccent = $('peAccent').value;
    $('peNameField').hidden = false;
    $('peDescField').hidden = false;
    $('peInstrField').hidden = false;
    $('peReadonly').hidden = true;
    $('peName').value = copy.name || '';
    $('peDescription').value = copy.description || '';
    $('peInstructions').value = copy.instructions || '';
    $('peInstrCount').textContent = `${$('peInstructions').value.length} / 4000`;
    state.original = {
      name: copy.name || '',
      description: copy.description || '',
      instructions: copy.instructions || '',
    };
    $('peVoice').value = keepVoice;
    $('peAccent').value = keepAccent;
    $('personaEditorTitle').textContent = copy.name || copy.id;
    $('personaEditorSubtitle').textContent =
      'Your editable copy — saving stores its text plus this voice and accent.';
    $('peName').focus();
  } catch (err) {
    if (err && err.name === 'AuthLostError') return;
    setError(
      (err instanceof ApiError && err.body && err.body.error && err.body.error.message) ||
        (err instanceof ApiError && typeof err.message === 'string' && err.message) ||
        "Couldn't duplicate this persona — try again.",
    );
  } finally {
    btn.disabled = false;
    btn.removeAttribute('aria-busy');
  }
}

async function savePersona(dlg) {
  const state = dlg.__peState;
  if (!state || !state.settingsDoc) return;
  setError('');

  // Own-persona text edits go through the persona CRUD API first.
  let textPayload = null;
  if (state.kind === 'own') {
    const name = $('peName').value.trim();
    const description = $('peDescription').value.trim();
    const instructions = $('peInstructions').value.trim();
    if (!name) {
      setError('Enter a name for this persona.');
      $('peName').focus();
      return;
    }
    if (!instructions) {
      setError('Enter the persona instructions — how it should speak and behave.');
      $('peInstructions').focus();
      return;
    }
    const o = state.original;
    if (name !== o.name || description !== o.description || instructions !== o.instructions) {
      textPayload = { name, description, instructions };
    }
  }

  const voice = $('peVoice').value;
  const accent = $('peAccent').value === 'none' ? '' : $('peAccent').value;

  setBusy(true);
  try {
    if (textPayload) {
      const updated = await apiJSON(`${PERSONAS_PATH}/${encodeURIComponent(state.personaId)}`, {
        method: 'PUT',
        json: textPayload,
      });
      state.persona = updated;
      state.original = {
        name: updated.name || '',
        description: updated.description || '',
        instructions: updated.instructions || '',
      };
    }
    await savePersonaPrefs(state.settingsDoc, state.personaId, voice, accent);
    window.dispatchEvent(new CustomEvent('personachanged', {
      detail: { personaId: state.personaId, voice, accent },
    }));
    dlg.close();
  } catch (err) {
    if (err && err.name === 'AuthLostError') return;
    if (err instanceof ApiError && err.code === 'version_conflict') {
      setError('Your settings changed on another device while saving — close and try again.');
    } else {
      setError(
        (err instanceof ApiError && typeof err.message === 'string' && err.message) ||
          "Couldn't save — check your connection and try again.",
      );
    }
  } finally {
    setBusy(false);
  }
}
