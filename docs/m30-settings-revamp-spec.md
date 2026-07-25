# M30 settings revamp — interaction spec

This is the pre-code design pass for the web settings revamp. Android should use
the same section order, labels, single-open rule, and open/close language when
its half is implemented.

## Persona checks

| User lens | Design consequence |
|---|---|
| Daily voice user | The drawer stays one action from the conversation; compact section bars replace the long wall of controls. |
| First-time or occasional user | Bars retain a short description, so labels such as “Turn detection” are understandable before opening them. |
| Touch or small-screen user | The whole section bar is the target, and both viewport-edge controls are tall, fixed targets rather than icon-sized circles. |
| Keyboard or screen-reader user | Every bar is a real button with explicit expanded state and panel ownership; no behavior depends on position, color, or a pointer. |
| Privacy-conscious user | Privacy and Account remain distinct final sections, so destructive account actions are not mixed with retention choices. |

## Sections and flow

Keep the existing settings controls and autosave behavior. Order the bars by the
way a conversation is configured:

1. Personal context: **About you**
2. Conversation identity and start: **Wake word**, **Persona**, **Voice engine**
3. Listening behavior and hardware: **Turn detection**, **Appearance**, **Microphone**
4. Trust and ownership: **Privacy**, **Account**

`About you` starts expanded because it can contain pending profile suggestions;
all other panels start collapsed. Only one panel is open at a time. Activating a
closed bar opens it and closes the previous panel; activating the open bar
collapses it. The state lasts for the current page only. Opening Settings while
its suggestion badge is present opens `About you` so the counted work is
visible.

## Accordion semantics and input

- Each section title is a heading containing a `button`. The button has
  `aria-expanded` and `aria-controls`; its panel has `role="region"` and
  `aria-labelledby` pointing back to the button.
- Enter or Space uses native button activation. Tab and Shift+Tab follow normal
  order through headers and only the controls in the expanded panel.
- Down/Up move to the next/previous header and wrap. Home/End move to the
  first/last header. Toggling leaves focus on the header; it never jumps into a
  form field.
- Collapsed panels use `hidden`, so their controls cannot be focused or
  announced. A screen reader hears the section name, its short description,
  “button”, and expanded/collapsed state.

## Drawer edge controls

- Closed: a fixed right-edge bar spans exactly 40% of the dynamic viewport
  height and is vertically centered. Its visible text is **Settings**; its
  accessible name is **Open settings** (plus the pending-suggestion count when
  present). It exposes `aria-haspopup="dialog"`, `aria-controls`, and current
  `aria-expanded`.
- Open: a visually matching, fixed left-edge bar spans the same 40% and says
  **Close**. It is the dialog's first focusable control and its accessible name
  is **Close settings**. Escape and scrim click remain equivalent close paths.
- Opening moves focus into the modal; closing returns focus to the right-edge
  opener. The page behind remains inert through the native modal dialog.
- Both bars retain a minimum 44 px width, a non-color icon/text cue, visible
  focus treatment, theme tokens, and reduced-motion behavior.
