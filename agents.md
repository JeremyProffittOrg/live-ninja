# agents.md

Instructions for ALL coding agents (Claude Code, Copilot, Codex, Cursor, etc.) working in
this repo. Claude-specific notes live in [CLAUDE.md](CLAUDE.md); this file and that one
reference each other and must stay consistent.

## Non-negotiables

1. **Read [deploy.md](deploy.md) first.** It is the authoritative guide for deployment
   (GitHub Actions + OIDC via `vars.AWS_DEPLOY_ROLE_ARN`), stack standards (tags, arm64,
   no secrets managers, no DynamoDB Scan on serving paths), and verification.
2. **Never deploy from a local machine.** Deploys happen only by pushing to `main`.
3. **Never handle credential values.** Do not ask the user to paste a secret into the
   conversation, do not print/echo/commit one. To add or update a secret, run
   `./scripts/set-secret.sh NAME` — the user types the value into a hidden terminal
   prompt and the script pushes it to GitHub. Non-secret config goes in GitHub
   variables (`gh variable set`).
4. **Never add static AWS keys** (`aws-access-key-id`, `AWS_ACCESS_KEY_ID` env, IAM user
   keys) anywhere. OIDC only.
5. Work on `main`, push after committing, and watch the triggered run to confirm the
   deploy is green before declaring success.

## Help section maintenance

The app ships an in-product **Help** slide-out: the `?` tab in the upper-LEFT corner of
`/conversation`, directly below the Settings tab (both are 40px corner tabs at every width
as of 2026-08-01 — this used to say "right-edge, above Settings" and was left stale by that
move). It is the only place a user is told what Live Ninja can
do, and it is static hand-written copy — nothing generates it, so nothing catches it going
stale except this rule.

**Rule: any change to a feature, setting, capability, page, or tool updates the Help copy
in the SAME commit.** A shipped feature the Help panel does not mention is an incomplete
change, not a follow-up.

Where it lives:

| What | Path |
| --- | --- |
| Help content (all user-facing copy) | `web/templates/pages/conversation.html` — the `HELP DRAWER` block, `<dialog id="helpDrawer">` |
| Open/close wiring | `web/static/js/conversation.mjs` — the "docked help drawer" block |
| Panel styling | `web/static/css/app.css` — `.conv-settings-tab--help` and the `.conv-help__*` rules |
| Drift guard | `internal/webapp/help_drawer_ui_test.go` |

The drawer chrome (`.conv-drawer`, `.conv-settings-tab`, the slide-in animation) is shared
with the Settings drawer on purpose. Reuse those classes; do not clone them, or the two
panels drift apart visually.

Checklist when you add or change something user-visible:

- [ ] Named a new **settings section**? Add a `<dt>`/`<dd>` pair under _Settings explained_,
      using the section's own title verbatim so `help_drawer_ui_test.go` matches it.
- [ ] Added an **assistant capability / tool** (something the user can ask for)? Add a bullet
      under _What you can ask for_, phrased as the thing the user says, not the tool name.
- [ ] Added a **page** or a rail control? Add it under _Where everything lives_ or
      _Getting started_.
- [ ] Changed a **default** or removed an option? Fix every sentence that still describes
      the old behaviour — search the help block for the option's name.
- [ ] Added a new **failure mode** users will hit? Add a bullet under
      _Tips and troubleshooting_ that says what to do, not what went wrong.
- [ ] Ran `go test ./internal/webapp/ -run TestHelpDrawer`.

Writing template for a new entry:

```html
<dt>Feature name, exactly as the UI labels it</dt>
<dd>What it does for the user in one sentence. Then, if it has a non-obvious
    default or a common mistake, one more sentence. No implementation detail,
    no API names, no milestone numbers.</dd>
```

Tone: second person, present tense, short sentences, scannable. Describe what the user
gets, never how it is built. Do not document anything that is gated off or unreleased —
the panel must stay honest about what actually works today.
