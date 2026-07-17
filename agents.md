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
