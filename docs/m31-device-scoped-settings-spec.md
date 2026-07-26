# M31 device-scoped settings — interaction spec

This is the pre-code multi-persona design pass for named devices and per-device
settings on web and Android. Both surfaces use the same terms, section IDs,
inheritance rules, targeting actions, and safety constraints.

## Persona checks

| User lens | Design consequence |
|---|---|
| One-device owner | Nothing looks like fleet administration by default. Each section says which named device is being edited, and ordinary edits save to that current device. |
| Multi-device owner | One button in every configurable section opens a stacked device view with effective values and inherited/custom badges. The same values can be applied to explicit checkboxes or to all devices. |
| First-time user | A safe suggested name such as “Chrome on Windows” or “Samsung Galaxy Tab” appears automatically and can be renamed. “Uses account default” explains inheritance without configuration jargon. |
| Privacy-conscious user | The UI explains that names and coarse model/platform information are stored, but serial numbers, MAC/IMEI, Android ID, IP, raw user agent, and browser fingerprints are not collected. |
| Touch or small-screen user | Device rows stack at 320 px; controls retain 44 px targets; no comparison requires a horizontally scrolling table. |
| Keyboard or screen-reader user | Scope controls are native buttons/fieldsets/checkboxes with named regions, expanded state, status text, and focus restoration. State never depends on color alone. |
| Mixed-capability owner | Unsupported settings are labeled and excluded rather than silently copied. A browser/Android microphone identifier is never sent to another host; only “System default” is portable. |
| Offline or racing client | The current effective settings remain usable. A failed save keeps the edited state marked unsaved, and a version conflict re-reads before retrying instead of overwriting another device. |

## Mental model and defaults

- The existing top-level settings are **account defaults**.
- A named device may customize any of the eight configurable sections.
- A missing device override means **Uses account default**.
- Normal controls edit **this device** after it has registered. Merely viewing
  another device never changes the current runtime.
- **Apply to selected devices** writes the visible section to checked devices.
- **Apply to all devices** updates the account default and removes that section's
  overrides from every device, so present and future devices truly agree.
- **Use account default** removes the selected device's override.
- Account is device management, not a copyable settings section.

## Per-section control

Inside each expanded configurable accordion panel, before its ordinary fields:

1. A button reads **Device settings · _device name_** and exposes
   `aria-expanded` plus `aria-controls`.
2. Its inline panel identifies the current viewing source, then lists owned
   devices as stacked rows. Each row shows editable/display name, safe host
   summary, capability status, inherited/custom status, and a concise effective
   value summary for this section.
3. Expanding a row shows every effective value for that host. Choosing
   **Copy settings from _device_** selects an immutable apply-source snapshot;
   Android may edit that separate preview. Neither action applies the preview
   to the local theme, microphone, wake service, or conversation.
4. Checkboxes feed an explicit **Apply these settings to selected devices**
   action. **Apply to all devices** includes the device count and requires a
   confirmation because it clears divergent overrides.
5. Status is announced through a polite live region: saving, saved, conflict,
   offline, unsupported, or failed.

The eight section IDs and owned top-level keys are:

| Section | Keys |
|---|---|
| `aboutYou` | `profile` |
| `wakeWord` | `wakeWord`, `wakeEngine`, `sensitivity`, supported device wake/display toggles |
| `persona` | `persona`, `voice`, `voiceAccent`, `personaPrefs` |
| `voiceEngine` | `voiceEngine`, `geminiVoice` |
| `turnDetection` | `turnDetection`, `micEagerness`, `keepListeningSeconds` |
| `appearance` | `theme`, `appearance`, legacy Android `appStyle` |
| `microphone` | `micDeviceId`, supported local microphone guards |
| `privacy` | `privacy`, supported diagnostics preference |

Android controls that operate the OS rather than the Live Ninja settings
document—runtime permissions, background wake-service state, screen/battery
behavior, and local diagnostic-log capture—are visually labeled **This Android
device only**. They are not part of a portable section payload and never imply
that selecting another host changed that host's operating-system state.

## Naming and host information

- Browser installations persist a random UUID and infer only browser family and
  platform for a label such as **Chrome on Windows**. Browsers cannot read the
  machine hostname.
- Android installations persist a random UUID and prefer the user-configured
  device name when available, otherwise normalized manufacturer/model.
- Paired hardware keeps its existing device ID and name, with product/thing
  metadata used only for display.
- A rename is user-owned and later background registration cannot replace it.
- Duplicate display names are allowed; a short non-secret device-ID suffix is
  shown only when needed to disambiguate.

## Capability and destructive-action rules

- Controls or copy targets unsupported on a device are disabled with an inline
  explanation. They are never reported as successfully applied.
- A non-null `micDeviceId` is local to one browser/app profile and cannot be
  copied. The portable value is `null`, labeled **System default**.
- Account sign-out, revoke, and deletion remain separate confirmed actions.
- Device metadata is never authorization input. The authenticated owner and
  server-side device ownership check authorize every selected target.
- If a browser or app installation UUID is already owned by a different
  account or was revoked, the client replaces that random UUID and retries
  registration once. A revoked installation can register its replacement only
  after fresh authentication with an unbound session; an old session bound to
  the revoked device remains rejected, so rotation cannot bypass revocation.
  The old device row is never transferred.
