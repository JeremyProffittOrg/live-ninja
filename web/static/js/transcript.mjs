// transcript.mjs — live conversation transcript renderer.
//
// Owner: WS-D (M3 web client) — transcript + visualizer workstream.
// Spec: docs/web-ui-spec.md §2.3/§2.4/§2.5/§2.8, plan.md M3 "Web §6",
// PRD.md FR-W04 ("Incremental deltas rendered via text nodes only
// (XSS-safe); role="log" aria-live="polite"; canvas visualizer aria-hidden").
//
// This module owns ONLY the transcript DOM subtree it is constructed with —
// it never reads/writes settings, never touches the realtime datachannel,
// and never fetches anything itself. The caller (realtime.mjs / the
// conversation page's own glue script — out of this file's ownership) is
// responsible for:
//   - calling startTurn()/appendDelta()/completeTurn() as datachannel events
//     (response.output_audio_transcript.delta, response.output_text.delta,
//     conversation.item.input_audio_transcription.*) arrive,
//   - calling appendToolResultCard() when a `function_call_output` is ready,
//   - calling addUserMessage() for a typed composer submission or a
//     non-streamed fallback-turn reply (POST /api/v1/fallback/turn).
//
// Rendering rule (non-negotiable, per FR-W04): every dynamic string reaches
// the DOM via textContent/createTextNode, never innerHTML/insertAdjacentHTML.
// Deltas are appended as individual text nodes to the in-progress bubble —
// this file never re-renders/rebuilds the transcript list on a token tick.
//
// No external dependencies; plain ES module, no bundler assumptions.

/** @typedef {'user'|'assistant'} TranscriptRole */

/**
 * @typedef {Object} TranscriptOptions
 * @property {string} [personaLabel] Initial label used for assistant turns
 *   that don't pass an explicit `label` (e.g. "Scout"). Rendered as
 *   "Live Ninja · {label}". Update later via setPersonaLabel().
 * @property {number} [autoscrollThreshold=48] Distance (px) from the
 *   bottom of the scroll container within which the view still counts as
 *   "pinned" to the latest message.
 * @property {HTMLElement} [pinButtonEl] Optional "jump to latest" control.
 *   If supplied, this module manages its `hidden`/`aria-pressed` state and
 *   wires its click handler; entirely optional — callers that render their
 *   own pin affordance can ignore this and drive setPinned()/scrollToBottom()
 *   directly instead.
 * @property {(pinned: boolean) => void} [onPinChange] Fired whenever the
 *   pinned state changes (user scroll or explicit setPinned()).
 * @property {() => void} [onUnseenContent] Fired when new content is
 *   appended while the view is *not* pinned (caller can surface a badge).
 */

const DEFAULT_THRESHOLD = 48;

/**
 * Formats a Date as a short localized clock time ("9:02 AM"), matching the
 * mockup's `.conv-timestamp` copy style. Uses the browser's locale.
 * @param {Date} date
 * @returns {string}
 */
export function formatTimestamp(date) {
  try {
    return new Intl.DateTimeFormat(undefined, {
      hour: "numeric",
      minute: "2-digit",
    }).format(date);
  } catch {
    // Intl unavailable/misconfigured locale — fall back to a fixed format
    // rather than throwing out of a render path.
    const h = date.getHours();
    const m = String(date.getMinutes()).padStart(2, "0");
    const hour12 = ((h + 11) % 12) + 1;
    return `${hour12}:${m} ${h < 12 ? "AM" : "PM"}`;
  }
}

function prefersReducedMotion() {
  return (
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function" &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches
  );
}

let turnSeq = 0;
function nextTurnId() {
  turnSeq += 1;
  return `turn-${turnSeq}-${Date.now().toString(36)}`;
}

/**
 * Live conversation transcript: incremental, XSS-safe, accessible.
 */
export class Transcript {
  /**
   * @param {HTMLElement} scrollEl The scrollable ancestor (e.g.
   *   `#transcriptScroll`) — receives the `log`/`aria-live` roles and drives
   *   pin-to-bottom detection.
   * @param {HTMLElement} listEl The list container turns are appended into
   *   (e.g. `#transcript`, the mockup's `.ln-transcript`).
   * @param {TranscriptOptions} [options]
   */
  constructor(scrollEl, listEl, options = {}) {
    if (!scrollEl || !listEl) {
      throw new Error("Transcript requires both a scroll container and a list container element");
    }
    this._scrollEl = scrollEl;
    this._listEl = listEl;
    this._threshold = options.autoscrollThreshold ?? DEFAULT_THRESHOLD;
    this._onPinChange = options.onPinChange;
    this._onUnseenContent = options.onUnseenContent;
    this._pinButtonEl = options.pinButtonEl ?? null;
    this._personaLabel = options.personaLabel ?? "";

    this._pinned = true;
    this._turns = new Map(); // turnId -> { role, bubbleEl, contentEl, wrapperEl, tsEl, label }
    this._typingEl = null;

    // §2.8: role="log" aria-live="polite" aria-relevant="additions" on the
    // scroll container. Set defensively (idempotent) — a server-rendered
    // template may already carry these, but this module must not depend on
    // that being done correctly upstream.
    if (!this._scrollEl.hasAttribute("role")) this._scrollEl.setAttribute("role", "log");
    if (!this._scrollEl.hasAttribute("aria-live")) this._scrollEl.setAttribute("aria-live", "polite");
    if (!this._scrollEl.hasAttribute("aria-relevant")) this._scrollEl.setAttribute("aria-relevant", "additions");

    this._onScroll = this._onScroll.bind(this);
    this._scrollEl.addEventListener("scroll", this._onScroll, { passive: true });
    this._scrollRaf = null;

    if (this._pinButtonEl) {
      this._pinButtonEl.hidden = true;
      this._pinButtonEl.setAttribute("aria-pressed", "true");
      this._onPinButtonClick = () => this.jumpToLatest();
      this._pinButtonEl.addEventListener("click", this._onPinButtonClick);
    }
  }

  /** Whether the view is currently pinned to the latest message. */
  get pinned() {
    return this._pinned;
  }

  /** Update the default persona label used by future assistant turns. */
  setPersonaLabel(label) {
    this._personaLabel = label ?? "";
  }

  // ---------------------------------------------------------------------
  // Streaming turn API
  // ---------------------------------------------------------------------

  /**
   * Begins a new turn bubble. Call appendDelta() as text streams in, then
   * completeTurn() once the turn is done (sets the timestamp, finalizes).
   * @param {TranscriptRole} role
   * @param {{label?: string}} [opts] For assistant turns, overrides the
   *   persona label for just this turn (defaults to the current
   *   setPersonaLabel() value).
   * @returns {string} An opaque turn id to pass to appendDelta/completeTurn.
   */
  startTurn(role, opts = {}) {
    const turnId = nextTurnId();
    const wrapperEl = document.createElement("div");
    wrapperEl.className = "ln-flex-col ln-gap-2";
    wrapperEl.style.alignItems = role === "user" ? "flex-end" : "flex-start";

    const bubbleEl = document.createElement("div");
    bubbleEl.className = role === "user" ? "ln-bubble ln-bubble--user" : "ln-bubble ln-bubble--assistant";

    const roleLabelEl = document.createElement("span");
    roleLabelEl.className = "ln-bubble__role";
    const label = role === "user" ? "You" : this._assistantLabel(opts.label);
    roleLabelEl.textContent = label;
    bubbleEl.appendChild(roleLabelEl);

    const contentEl = document.createElement("span");
    contentEl.className = "ln-bubble__content";
    bubbleEl.appendChild(contentEl);

    const tsEl = document.createElement("span");
    tsEl.className = role === "user" ? "conv-timestamp conv-timestamp--user" : "conv-timestamp";
    tsEl.hidden = true; // shown on completeTurn()

    wrapperEl.appendChild(bubbleEl);
    wrapperEl.appendChild(tsEl);

    this._insertBeforeLiveMarkers(wrapperEl);

    this._turns.set(turnId, { role, wrapperEl, bubbleEl, contentEl, tsEl, label, done: false });
    this._afterMutation();
    return turnId;
  }

  /**
   * Appends a chunk of text to an in-progress turn as a plain text node
   * (never innerHTML) — this is the "incremental" part of incremental
   * rendering: no re-render of prior content.
   * @param {string} turnId
   * @param {string} text
   */
  appendDelta(turnId, text) {
    if (!text) return;
    const turn = this._turns.get(turnId);
    if (!turn || turn.done) {
      console.warn("[transcript] appendDelta on unknown/completed turn", turnId);
      return;
    }
    turn.contentEl.appendChild(document.createTextNode(text));
    this._afterMutation();
  }

  /**
   * Finalizes a turn: stamps the timestamp and marks it immutable. Safe to
   * call even if the turn received zero deltas (renders an empty bubble
   * with just the role label — callers should avoid completing empty
   * turns where possible, but this never throws).
   * @param {string} turnId
   * @param {{timestamp?: Date}} [opts]
   */
  completeTurn(turnId, opts = {}) {
    const turn = this._turns.get(turnId);
    if (!turn) {
      console.warn("[transcript] completeTurn on unknown turn", turnId);
      return;
    }
    turn.tsEl.textContent = formatTimestamp(opts.timestamp ?? new Date());
    turn.tsEl.hidden = false;
    turn.done = true;
    this._afterMutation();
  }

  /**
   * Convenience for a turn whose full text is already known up front (a
   * typed composer submission, or a non-streamed fallback-turn reply) —
   * equivalent to startTurn + appendDelta + completeTurn in one call.
   * @param {TranscriptRole} role
   * @param {string} text
   * @param {{label?: string, timestamp?: Date}} [opts]
   * @returns {string} the completed turn's id
   */
  addMessage(role, text, opts = {}) {
    const turnId = this.startTurn(role, { label: opts.label });
    this.appendDelta(turnId, text);
    this.completeTurn(turnId, { timestamp: opts.timestamp });
    return turnId;
  }

  /** Convenience: addMessage('user', text, opts). */
  addUserMessage(text, opts = {}) {
    return this.addMessage("user", text, opts);
  }

  /** Convenience: addMessage('assistant', text, opts). */
  addAssistantMessage(text, opts = {}) {
    return this.addMessage("assistant", text, opts);
  }

  // ---------------------------------------------------------------------
  // Tool result cards (§2.3, §2.8 — "use a real <dl>... never a raw object
  // dump"). Rendered live only, in transcript order — never pre-seeded.
  // ---------------------------------------------------------------------

  /**
   * @param {Object} card
   * @param {string} [card.icon] A single emoji/glyph, decorative (aria-hidden).
   * @param {string} card.title
   * @param {string} [card.subtitle]
   * @param {string} [card.badge] Badge text, e.g. "Confirmed"/"Running".
   * @param {'teal'|'active'|'warn'|'error'|'muted'} [card.badgeVariant='teal']
   * @param {Array<[string, string]>} card.fields Ordered [label, value] pairs
   *   rendered as <dt>/<dd> — the only supported shape, so every value is
   *   guaranteed to reach the DOM as text, never as a raw object dump.
   * @returns {HTMLElement} the inserted card element, in case the caller
   *   needs to update it later (e.g. a running timer's "Remaining" field).
   */
  appendToolResultCard(card) {
    const outer = document.createElement("div");
    outer.className = "conv-toolcard";

    const cardEl = document.createElement("div");
    cardEl.className = "ln-card";

    const head = document.createElement("div");
    head.className = "conv-toolcard__head";

    if (card.icon) {
      const iconEl = document.createElement("span");
      iconEl.className = "conv-toolcard__icon";
      iconEl.setAttribute("aria-hidden", "true");
      iconEl.textContent = card.icon;
      head.appendChild(iconEl);
    }

    const titleWrap = document.createElement("div");
    const titleEl = document.createElement("div");
    titleEl.className = "conv-toolcard__title";
    titleEl.textContent = card.title ?? "";
    titleWrap.appendChild(titleEl);
    if (card.subtitle) {
      const subEl = document.createElement("div");
      subEl.className = "conv-toolcard__sub";
      subEl.textContent = card.subtitle;
      titleWrap.appendChild(subEl);
    }
    head.appendChild(titleWrap);

    if (card.badge) {
      const badgeEl = document.createElement("span");
      const variant = card.badgeVariant ?? "teal";
      badgeEl.className = `ln-badge ln-badge--${variant} ln-badge--dot-none`;
      badgeEl.style.marginLeft = "auto";
      badgeEl.textContent = card.badge;
      head.appendChild(badgeEl);
    }

    cardEl.appendChild(head);

    const dl = document.createElement("dl");
    dl.className = "kv";
    for (const [label, value] of card.fields ?? []) {
      const dt = document.createElement("dt");
      dt.textContent = label;
      const dd = document.createElement("dd");
      dd.textContent = value;
      dl.appendChild(dt);
      dl.appendChild(dd);
    }
    cardEl.appendChild(dl);

    outer.appendChild(cardEl);
    this._insertBeforeLiveMarkers(outer);
    this._afterMutation();
    return outer;
  }

  // ---------------------------------------------------------------------
  // Typing indicator (the "thinking" state's in-progress assistant bubble)
  // ---------------------------------------------------------------------

  /**
   * Shows a typing-dots placeholder bubble (the mockup's `.ln-bubble--typing`
   * pattern) for the given role. Only one typing indicator exists at a
   * time — a second call replaces the first. Automatically hidden by
   * startTurn()/appendDelta() for the same turn once real content lands;
   * callers that show a typing indicator should call hideTypingIndicator()
   * once the corresponding startTurn() is issued.
   * @param {TranscriptRole} [role='assistant']
   */
  showTypingIndicator(role = "assistant") {
    this.hideTypingIndicator();

    const wrapperEl = document.createElement("div");
    wrapperEl.className = "ln-flex-col ln-gap-2";
    wrapperEl.style.alignItems = role === "user" ? "flex-end" : "flex-start";

    const bubbleEl = document.createElement("div");
    bubbleEl.className = role === "user" ? "ln-bubble ln-bubble--user" : "ln-bubble ln-bubble--assistant";

    const roleLabelEl = document.createElement("span");
    roleLabelEl.className = "ln-bubble__role";
    roleLabelEl.textContent = role === "user" ? "You" : this._assistantLabel();
    bubbleEl.appendChild(roleLabelEl);

    const dotsEl = document.createElement("span");
    dotsEl.className = "ln-bubble--typing";
    dotsEl.setAttribute("aria-hidden", "true");
    dotsEl.appendChild(document.createElement("span"));
    dotsEl.appendChild(document.createElement("span"));
    dotsEl.appendChild(document.createElement("span"));
    bubbleEl.appendChild(dotsEl);

    wrapperEl.appendChild(bubbleEl);
    this._insertBeforeLiveMarkers(wrapperEl);
    this._typingEl = wrapperEl;
    this._afterMutation();
  }

  /** Removes the typing indicator, if any is shown. */
  hideTypingIndicator() {
    if (this._typingEl && this._typingEl.parentNode === this._listEl) {
      this._listEl.removeChild(this._typingEl);
    }
    this._typingEl = null;
  }

  // ---------------------------------------------------------------------
  // Pin-to-bottom / autoscroll
  // ---------------------------------------------------------------------

  /** Scrolls to the latest content and re-pins. */
  jumpToLatest() {
    this.setPinned(true, { scroll: true });
  }

  /**
   * @param {boolean} pinned
   * @param {{scroll?: boolean}} [opts] Whether to scroll to bottom
   *   immediately when pinning (default true).
   */
  setPinned(pinned, opts = {}) {
    const scroll = opts.scroll ?? true;
    const changed = this._pinned !== pinned;
    this._pinned = pinned;
    if (pinned && scroll) this.scrollToBottom();
    this._syncPinButton();
    if (changed && typeof this._onPinChange === "function") this._onPinChange(pinned);
  }

  /** Scrolls the transcript to the latest content. */
  scrollToBottom() {
    const behavior = prefersReducedMotion() ? "auto" : "smooth";
    // Guard window: the smooth scroll fires scroll events from positions far
    // above the bottom, which _onScroll used to read as the USER scrolling
    // away — auto-follow silently unpinned itself on the very first turn.
    this._programmaticUntil = Date.now() + 600;
    this._scrollEl.scrollTo({ top: this._scrollEl.scrollHeight, behavior });
  }

  /**
   * Removes all rendered turns/cards from the list container (does not
   * reset the persona label). Intended for a full reset — e.g. starting a
   * brand-new session view — since it empties the container unconditionally.
   */
  clear() {
    this._turns.clear();
    this._typingEl = null;
    while (this._listEl.firstChild) {
      this._listEl.removeChild(this._listEl.firstChild);
    }
    this.setPinned(true, { scroll: false });
  }

  /** Detaches listeners. Call when the conversation view is torn down. */
  destroy() {
    this._scrollEl.removeEventListener("scroll", this._onScroll);
    if (this._scrollRaf) cancelAnimationFrame(this._scrollRaf);
    if (this._pinButtonEl && this._onPinButtonClick) {
      this._pinButtonEl.removeEventListener("click", this._onPinButtonClick);
    }
  }

  // ---------------------------------------------------------------------
  // Internals
  // ---------------------------------------------------------------------

  _assistantLabel(overrideLabel) {
    const label = overrideLabel ?? this._personaLabel;
    return label ? `Live Ninja · ${label}` : "Live Ninja";
  }

  _insertBeforeLiveMarkers(node) {
    // New content always goes at the end of the rendered list; the typing
    // indicator (if present) should stay last, so insert real content
    // before it rather than after.
    if (this._typingEl && this._typingEl.parentNode === this._listEl) {
      this._listEl.insertBefore(node, this._typingEl);
    } else {
      this._listEl.appendChild(node);
    }
  }

  _afterMutation() {
    if (this._pinned) {
      this.scrollToBottom();
    } else if (typeof this._onUnseenContent === "function") {
      this._onUnseenContent();
    }
    this._syncPinButton();
  }

  _syncPinButton() {
    if (!this._pinButtonEl) return;
    this._pinButtonEl.hidden = this._pinned;
    this._pinButtonEl.setAttribute("aria-pressed", String(!this._pinned));
  }

  _onScroll() {
    if (this._scrollRaf) return;
    this._scrollRaf = requestAnimationFrame(() => {
      this._scrollRaf = null;
      const el = this._scrollEl;
      const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
      const shouldPin = distanceFromBottom <= this._threshold;
      // Scroll events raised by our own scrollToBottom animation never
      // unpin; the guard clears once the animation lands (or times out).
      if (this._programmaticUntil && Date.now() < this._programmaticUntil) {
        if (shouldPin) this._programmaticUntil = 0;
        return;
      }
      if (shouldPin !== this._pinned) {
        this.setPinned(shouldPin, { scroll: false });
      }
    });
  }
}
