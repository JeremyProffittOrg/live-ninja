// tooldetails.mjs — shared "Details" popup for tool-call cards.
//
// One lazily-created, reused <dialog> backs every call site: the live
// conversation transcript's tool-result cards, the fallback-turn cards
// (POST /api/v1/fallback/turn's executed toolCalls), and the History
// detail view's tool-call cards (history.mjs). Building it once here
// keeps the markup/behavior in exactly one place instead of duplicated
// per page.
//
// Every dynamic string reaches the DOM via textContent (pre.textContent
// for the prettified JSON) — never innerHTML/insertAdjacentHTML. This is
// the one place a raw object dump is the *point* (a debug/export view),
// so it's fenced off behind an explicit "Details" tap rather than shown
// inline on the card itself (spec §2.8's "never a raw object dump" governs
// the compact card; this is its deliberate escape hatch).
//
// Native <dialog>.showModal() supplies the focus trap + Escape-to-close;
// a click that lands on the dialog element itself (never inside
// .tooldetails__inner-ish content, since this dialog has no separate
// padded wrapper) is the ::backdrop scrim, mirroring #micTestDialog /
// #settingsDrawer.

const FILENAME_TOOL_MAX = 60;

let dialogEl = null;
let titleEl = null;
let inputPreEl = null;
let outputLabelEl = null;
let outputPreEl = null;
let saveBtnEl = null;
let copyBtnEl = null;
let copyResetTimer = 0;

// The entry currently shown — Save/Copy read from this rather than
// re-deriving it from the DOM text (keeps the saved/copied JSON exact,
// independent of how the <pre> happens to wrap).
let current = null;

/** Same copy pattern as conversation.mjs's copyText(): Clipboard API first,
 * a hidden-textarea execCommand('copy') fallback for permission/focus edge
 * cases. Kept local (not imported) so this module stays a standalone,
 * dependency-free shared component. */
async function copyText(text) {
  if (navigator.clipboard && typeof navigator.clipboard.writeText === "function") {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      /* fall through to the legacy path */
    }
  }
  try {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    ta.remove();
    return ok;
  } catch {
    return false;
  }
}

/** Parses a value that may already be an object/array, a JSON string, or a
 * plain non-JSON string — the shape varies by call site (realtime tool-call
 * args arrive as either; History's stored output is always a possibly
 * server-truncated string). Falls back to the raw string when it isn't
 * valid JSON rather than throwing. */
function parseLoose(value) {
  if (typeof value !== "string") return value;
  const trimmed = value.trim();
  if (trimmed === "") return undefined;
  try {
    return JSON.parse(trimmed);
  } catch {
    return value; // not JSON — the caller shows it verbatim
  }
}

/** Prettified JSON.stringify(value, null, 2) of whatever `value` parses to;
 * a value that turns out to be a plain (non-JSON) string is shown as-is. */
function prettyPrint(value) {
  if (value === undefined || value === null) return "";
  const parsed = parseLoose(value);
  if (typeof parsed === "string") return parsed;
  try {
    return JSON.stringify(parsed, null, 2);
  } catch {
    return String(parsed);
  }
}

function toolTitleText(tool) {
  const name = String(tool || "Tool call");
  return name.replace(/[_-]+/g, " ").replace(/\b\w/g, (ch) => ch.toUpperCase());
}

function safeFilenamePart(s) {
  const cleaned = String(s || "tool").replace(/[^a-zA-Z0-9-]+/g, "_").slice(0, FILENAME_TOOL_MAX);
  return cleaned || "tool";
}

/** The combined JSON both Save-as-file and Copy-to-clipboard operate on —
 * exactly what the dialog displays (input + whichever of output/error is
 * showing), so "what you see is what you get/export". */
function currentPayload() {
  if (!current) return {};
  const payload = {
    tool: current.tool || "",
    ts: new Date(current.ts || Date.now()).toISOString(),
  };
  if (current.callId) payload.callId = current.callId;
  payload.input = parseLoose(current.args);
  const hasError = current.error !== undefined && current.error !== null && current.error !== "";
  if (hasError) {
    payload.error = typeof current.error === "string" ? current.error : current.error;
  } else {
    payload.output = parseLoose(current.result);
  }
  return payload;
}

function currentPayloadJSON() {
  try {
    return JSON.stringify(currentPayload(), null, 2);
  } catch {
    return "{}";
  }
}

function saveCurrentAsFile() {
  if (!current) return;
  const text = currentPayloadJSON();
  const blob = new Blob([text], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  const stamp = current.ts || Date.now();
  a.href = url;
  a.download = `tool-call-${safeFilenamePart(current.tool)}-${stamp}.json`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Give the download a moment to actually start before freeing the blob.
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function copyCurrentToClipboard() {
  if (!current || !copyBtnEl) return;
  const text = currentPayloadJSON();
  const original = "Copy to clipboard";
  copyText(text).then((ok) => {
    clearTimeout(copyResetTimer);
    copyBtnEl.textContent = ok ? "Copied" : "Press ⌘/Ctrl+C";
    copyBtnEl.classList.toggle("is-copied", ok);
    copyResetTimer = setTimeout(() => {
      copyBtnEl.textContent = original;
      copyBtnEl.classList.remove("is-copied");
    }, 1600);
  });
}

function buildDialog() {
  const dialog = document.createElement("dialog");
  dialog.className = "tooldetails";
  dialog.setAttribute("aria-labelledby", "toolDetailsTitle");

  const head = document.createElement("div");
  head.className = "tooldetails__head";

  const title = document.createElement("h2");
  title.className = "tooldetails__title";
  title.id = "toolDetailsTitle";
  head.appendChild(title);

  const closeIconBtn = document.createElement("button");
  closeIconBtn.type = "button";
  closeIconBtn.className = "ln-iconbtn";
  closeIconBtn.setAttribute("aria-label", "Close");
  closeIconBtn.textContent = "×"; // ×
  closeIconBtn.addEventListener("click", () => dialog.close());
  head.appendChild(closeIconBtn);

  dialog.appendChild(head);

  const body = document.createElement("div");
  body.className = "tooldetails__body";

  const inputSection = document.createElement("div");
  inputSection.className = "tooldetails__section";
  const inputLabel = document.createElement("div");
  inputLabel.className = "tooldetails__label";
  inputLabel.textContent = "Input";
  const inputPre = document.createElement("pre");
  inputPre.className = "tooldetails__pre";
  inputSection.appendChild(inputLabel);
  inputSection.appendChild(inputPre);
  body.appendChild(inputSection);

  const outputSection = document.createElement("div");
  outputSection.className = "tooldetails__section";
  const outputLabel = document.createElement("div");
  outputLabel.className = "tooldetails__label";
  const outputPre = document.createElement("pre");
  outputPre.className = "tooldetails__pre";
  outputSection.appendChild(outputLabel);
  outputSection.appendChild(outputPre);
  body.appendChild(outputSection);

  dialog.appendChild(body);

  const actions = document.createElement("div");
  actions.className = "tooldetails__actions";

  const saveBtn = document.createElement("button");
  saveBtn.type = "button";
  saveBtn.className = "ln-btn ln-btn--ghost";
  saveBtn.textContent = "Save as file";
  saveBtn.addEventListener("click", saveCurrentAsFile);
  actions.appendChild(saveBtn);

  const copyBtn = document.createElement("button");
  copyBtn.type = "button";
  copyBtn.className = "ln-btn ln-btn--ghost";
  copyBtn.textContent = "Copy to clipboard";
  copyBtn.addEventListener("click", copyCurrentToClipboard);
  actions.appendChild(copyBtn);

  const closeBtn = document.createElement("button");
  closeBtn.type = "button";
  closeBtn.className = "ln-btn ln-btn--primary";
  closeBtn.textContent = "Close";
  closeBtn.addEventListener("click", () => dialog.close());
  actions.appendChild(closeBtn);

  dialog.appendChild(actions);

  // Scrim click closes (mirrors #micTestDialog / #settingsDrawer); Escape
  // is native <dialog> behavior, no extra wiring needed.
  dialog.addEventListener("click", (e) => {
    if (e.target === dialog) dialog.close();
  });
  dialog.addEventListener("close", () => {
    current = null;
  });

  document.body.appendChild(dialog);

  dialogEl = dialog;
  titleEl = title;
  inputPreEl = inputPre;
  outputLabelEl = outputLabel;
  outputPreEl = outputPre;
  saveBtnEl = saveBtn;
  copyBtnEl = copyBtn;
}

/**
 * Opens the shared tool-call Details dialog.
 * @param {Object} entry
 * @param {string} [entry.tool] Tool name (formatted into a title, e.g.
 *   "get_weather" -> "Get Weather").
 * @param {string} [entry.callId] Shown nowhere yet but carried into the
 *   saved/copied JSON for correlation with server logs.
 * @param {*} [entry.args] The call's input — object, a JSON string, or a
 *   plain string; parsed defensively.
 * @param {*} [entry.result] The call's output when it succeeded — same
 *   parsing rule as `args`. Ignored when `error` is present.
 * @param {*} [entry.error] The failure detail (a string, or an
 *   {message,code} object) — when present, replaces the Output section
 *   with an Error section.
 * @param {number} [entry.ts] Epoch-ms timestamp; defaults to now. Drives
 *   the saved filename and the exported `ts`.
 */
export function openToolDetails(entry) {
  if (!dialogEl) buildDialog();
  current = { ts: Date.now(), ...(entry || {}) };

  titleEl.textContent = toolTitleText(current.tool);

  const inputText = prettyPrint(current.args);
  inputPreEl.textContent = inputText || "(no input recorded)";

  const hasError = current.error !== undefined && current.error !== null && current.error !== "";
  if (hasError) {
    outputLabelEl.textContent = "Error";
    const errText = typeof current.error === "string" ? current.error : prettyPrint(current.error);
    outputPreEl.textContent = errText || "(no error detail)";
  } else {
    outputLabelEl.textContent = "Output";
    const outputText = prettyPrint(current.result);
    outputPreEl.textContent = outputText || "(no output recorded)";
  }

  saveBtnEl.textContent = "Save as file";
  copyBtnEl.textContent = "Copy to clipboard";
  copyBtnEl.classList.remove("is-copied");
  clearTimeout(copyResetTimer);

  if (typeof dialogEl.showModal === "function") dialogEl.showModal();
}
