// Accessible, single-open accordion behavior for the settings drawer.
//
// The template owns the semantics and no-JS initial state. This module only
// synchronizes aria-expanded/hidden, enforces the single-open rule, and adds
// the optional WAI-ARIA accordion header navigation keys.

const TRIGGER_SELECTOR = '[data-settings-accordion-trigger]';

export function initSettingsAccordion(root = document) {
  const triggers = Array.from(root.querySelectorAll(TRIGGER_SELECTOR));

  const panelFor = (trigger) => {
    const panelId = trigger.getAttribute('aria-controls');
    if (!panelId) return null;
    const panel = trigger.ownerDocument.getElementById(panelId);
    return panel && root.contains(panel) ? panel : null;
  };

  const setExpanded = (trigger, expanded) => {
    const panel = panelFor(trigger);
    if (!panel) return false;
    trigger.setAttribute('aria-expanded', expanded ? 'true' : 'false');
    panel.hidden = !expanded;
    return true;
  };

  const open = (triggerId, { focus = false } = {}) => {
    const trigger = triggers.find((candidate) => candidate.id === triggerId);
    if (!trigger || !panelFor(trigger)) return false;
    for (const candidate of triggers) setExpanded(candidate, candidate === trigger);
    if (focus) trigger.focus();
    return true;
  };

  // Normalize the SSR state. The first explicitly expanded panel wins; a
  // malformed second "true" can never leave two panels open.
  let foundOpen = false;
  for (const trigger of triggers) {
    const wantsOpen = !foundOpen && trigger.getAttribute('aria-expanded') === 'true';
    if (setExpanded(trigger, wantsOpen) && wantsOpen) foundOpen = true;
  }

  triggers.forEach((trigger, index) => {
    trigger.addEventListener('click', () => {
      const opening = trigger.getAttribute('aria-expanded') !== 'true';
      if (opening) {
        open(trigger.id);
      } else {
        setExpanded(trigger, false);
      }
    });

    trigger.addEventListener('keydown', (event) => {
      if (event.altKey || event.ctrlKey || event.metaKey) return;
      let nextIndex;
      switch (event.key) {
        case 'ArrowDown':
          nextIndex = (index + 1) % triggers.length;
          break;
        case 'ArrowUp':
          nextIndex = (index - 1 + triggers.length) % triggers.length;
          break;
        case 'Home':
          nextIndex = 0;
          break;
        case 'End':
          nextIndex = triggers.length - 1;
          break;
        default:
          return;
      }
      event.preventDefault();
      triggers[nextIndex].focus();
    });
  });

  return { open, triggers: [...triggers] };
}
