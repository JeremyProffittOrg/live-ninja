# deploy.md — how this repo deploys (read me before touching infra or credentials)

This repo is part of **JeremyProffittOrg** and follows the org-wide deployment standard
(established 2026-07-05, see the private `credential-rotation` repo for the full audit/design).
Every agent — Claude Code, Copilot, Codex, or human — follows these rules exactly.

## The one deployment path

**Deployment = push/merge to `main`.** GitHub Actions runs the pipeline; the pipeline is the
only thing that touches AWS.

- **NEVER deploy from a local machine.** No `aws cloudformation deploy`, no `sam deploy`,
  no `sam sync`, no `cdk deploy`, no `aws s3 sync` to production buckets. If a deploy is
  needed and no pipeline exists, build/extend the GitHub Actions workflow instead.
- Verify a deploy by watching the run (`gh run watch <id>`); report a one-line summary
  ("Deploy: completed in 2m15s"), not the full log. Show detail only on failure.

## AWS authentication — OIDC only, no static keys

Workflows authenticate with the org-wide **`gha-deploy`** IAM role via GitHub OIDC.
The role ARN comes from the repo variable **`AWS_DEPLOY_ROLE_ARN`** (set at repo creation;
org-level variables do NOT reach private repos on our GitHub plan, so it must be repo-level).

The canonical job shape:

```yaml
jobs:
  deploy:
    runs-on: ubuntu-latest
    permissions:
      id-token: write     # required for OIDC
      contents: read      # use `write` only if the job pushes commits/releases
    steps:
      - uses: actions/checkout@v4
      - name: Configure AWS credentials (OIDC)
        uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: ${{ vars.AWS_DEPLOY_ROLE_ARN }}
          role-session-name: gha-${{ github.event.repository.name }}-${{ github.run_id }}
          aws-region: us-east-1
```

- **NEVER add `aws-access-key-id` / `aws-secret-access-key` inputs** or `AWS_ACCESS_KEY_ID`
  env exports to a workflow. Never create IAM users or access keys for CI.
- Adding a `permissions:` block replaces the default grants — keep whatever write
  permissions the job already relies on.
- The role trusts `main`/`master`/`develop` branches, `v*` tags, and GitHub environments —
  PRs and feature branches deliberately cannot assume it.
- Target account is in the repo variable `DEPLOY_AWS_ACCOUNT`
  (759775734231 = jeremy.ninja · 584444171698 = caps/pappraisal · 231545823618 = artphotography).
- If the role is denied an `iam:PassRole` to a new AWS service, the fix goes in
  `credential-rotation/infra/github-oidc-deploy.yaml` (org repo), not in a per-repo key.

## Credentials — agents NEVER handle secret values

**Do not ask the user to paste a credential into chat. Do not echo, log, or commit secret
values. Do not put secrets in workflow files, samconfig.toml, or source.** GitHub secrets
are write-only; there is no legitimate reason for an agent to see a secret value.

To add or update a secret, run the script and let the **user type the value directly into
the terminal prompt** (it never enters the conversation):

```bash
./scripts/set-secret.sh SECRET_NAME            # prompts user (hidden input), pushes via gh
./scripts/set-secret.sh SECRET_NAME --file p   # multiline secrets (keys/certs) from a file
```

The script sets the secret on this repo, verifies `updated_at`, and prints only a
fingerprint. If a workflow needs a secret that doesn't exist yet: reference it as
`${{ secrets.NAME }}`, tell the user to run the script, and wait — never invent a value,
never stub around it.

**Non-secret configuration** (domains, ARNs, IDs, emails, usernames, regions, buckets) goes
in **variables**, not secrets: `gh variable set NAME --body "value"`. If a value would be
fine printed in a log, it is a variable.

Org-wide rotation is handled by the `credential-rotation` repo's manifest + `rotate-secrets.sh` —
don't build per-repo rotation machinery.

## Stack standards (mandatory)

- **Cost-allocation tags** on every stack, set ONCE at stack level in `samconfig.toml`
  (`tags = "..."` under `[default.deploy.parameters]`), never per-resource:
  `Project=<repo-name> CostCenter=<bucket> Environment=prod ManagedBy=sam DeployedVia=github-actions Owner=jeremy`
  (CostCenter buckets: caps, photography, mcp-servers, media, voice-ai, home-iot, utility.)
- **Lambda = arm64 (Graviton)** unless a dependency forces x86_64. For compiled runtimes,
  the build flags and `Architectures: [arm64]` MUST change together
  (Go: `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`).
- **No secrets managers** (AWS Secrets Manager, Vault, …) — use Lambda env vars, SSM
  Parameter Store standard tier, or GitHub secrets.
- **No DynamoDB `Scan` on any serving path** (API/feed/list endpoints). Key lookups,
  `Query`, or a cached/S3-snapshot representation only.
- Production-only: assume no staging environment. Verify before you deploy.

## Repo bootstrap facts (filled in at creation)

| | |
|---|---|
| Repo | `JeremyProffittOrg/live-ninja` |
| Domain | `live.jeremy.ninja` |
| AWS account | `759775734231` (repo var `DEPLOY_AWS_ACCOUNT`) |
| Deploy role | repo var `AWS_DEPLOY_ROLE_ARN` |
| Pre-set variables | `ADMIN_USER, DOMAIN_NAME, HOSTED_ZONE_ID, CERTIFICATE_ARN, CERTIFICATE_ARN_US_EAST_2, CLOUDFORMATION_S3_BUCKET, DEPLOY_AWS_ACCOUNT, AWS_DEPLOY_ROLE_ARN` |
| Pre-set secrets | **none** — add only what a workflow actually references, via `scripts/set-secret.sh` |
