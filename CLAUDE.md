# CLAUDE.md

Read **[deploy.md](deploy.md)** before any infrastructure, workflow, or credential work —
it defines the only allowed deployment path (GitHub Actions + OIDC, no local deploys, no
static AWS keys) and the credential policy (agents never see secret values; use
`scripts/set-secret.sh`).

Agent configuration is shared with [agents.md](agents.md); keep the two consistent.

- Commit and push directly to `main` (no feature branches or PRs unless asked).
- Pushing to `main` IS the deploy trigger — treat every push as a production deploy.
- No stubs or placeholder implementations; ask when blocked.
- No `Co-Authored-By: Claude` trailers in commit messages.

## Help section maintenance

Live Ninja ships an in-product Help slide-out (the `?` tab above Settings on
`/conversation`). Its copy is hand-written and is the only place users are told what the
app can do, so **any change to a feature, setting, capability, page, or tool updates the
Help copy in the same commit.**

- Content: `web/templates/pages/conversation.html`, the `HELP DRAWER` block
  (`<dialog id="helpDrawer">`).
- Wiring: `web/static/js/conversation.mjs` ("docked help drawer" block).
- Styling: `web/static/css/app.css` — `.conv-settings-tab--help`, `.conv-help__*`.
- Guard: `internal/webapp/help_drawer_ui_test.go` (`go test ./internal/webapp/ -run TestHelpDrawer`).

The full checklist and the entry template live in
[agents.md → Help section maintenance](agents.md#help-section-maintenance) — follow it
there; this list is only the pointer.
