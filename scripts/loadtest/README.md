# scripts/loadtest/

Load-test tooling for live-ninja's **read-only, unauthenticated-safe** HTTP
surface, per plan.md M7 ("Load tests (k6/Vegeta + synthetic-session
generator): quota/rate limits hold, `ConsumedReadCapacityUnits` flat (no
Scan), broker ephemeral-token mint under connection load").

Scope note: both tools here only exercise `/healthz`, `/v1/compat`, and the
deliberately-401 `/api/v1/me` (no `Authorization` header sent). Neither
mints a realtime broker session or invokes tools — that path is
quota/anomaly-gated (`internal/realtime/quota.go`, M7's per-user
hourly-burn auto-suspend) and requires a real authenticated account, so it
is out of scope for a probe that's safe to run unattended against
production. If broker-mint load testing is ever needed, it requires a
dedicated throwaway test account, explicit sign-off, and should NOT reuse
this script as-is.

## Files

- **`goload.go`** — dependency-free Go load probe (stdlib only, `go run`-able
  with no `go get`). This is what was actually run for the M7 load-test task
  because **k6 is not installed** on this machine. Drives a fixed aggregate
  request rate against the three targets above and prints p50/p90/p99
  latency plus a status-code breakdown per target and overall.

  ```sh
  go run ./scripts/loadtest/goload.go \
    -base https://live.jeremy.ninja -rps 50 -duration 60s
  ```

  Flags: `-base` (default `https://live.jeremy.ninja`), `-rps` (default 50),
  `-duration` (default 60s), `-timeout` (per-request timeout, default 10s).

  Exit code is always 0 — this is a reporting tool. Judge pass/fail from the
  printed report plus a CloudWatch pull (see "Verifying the result" below).

- **`loadtest.js`** — equivalent k6 script, written and committed for when
  k6 becomes available, using a `constant-arrival-rate` executor (50 req/s
  for 60s) with thresholds mirroring the same acceptance bar
  (`http_req_failed rate<1%`, `p(99)<2000ms`, `unexpected_status rate<1%`).

  ```sh
  k6 run scripts/loadtest/loadtest.js
  # or override the target:
  BASE_URL=https://live.jeremy.ninja k6 run scripts/loadtest/loadtest.js
  ```

  Install k6 (not currently installed on this machine):
  - Windows: `choco install k6` or `winget install k6.k6`
  - macOS: `brew install k6`
  - Linux: see https://k6.io/docs/get-started/installation/

## Verifying the result (CloudWatch)

After a run, confirm the public read-only surface did not touch the table
and did not error, via CloudWatch (read-only calls — no deploy, no `aws ...
deploy`, per deploy.md):

```sh
# DynamoDB consumed read capacity for the `live-ninja` table during the
# run window — should be ~0 / flat, proving no Scan (or other read) sits
# behind /healthz, /v1/compat, or the 401 short-circuit on /api/v1/me.
aws cloudwatch get-metric-statistics \
  --namespace AWS/DynamoDB --metric-name ConsumedReadCapacityUnits \
  --dimensions Name=TableName,Value=live-ninja \
  --start-time <window-start-iso> --end-time <window-end-iso> \
  --period 60 --statistics Sum

# API Gateway / ALB 5xx count for the run window — should be 0.
aws cloudwatch get-metric-statistics \
  --namespace AWS/ApiGateway --metric-name 5xx \
  --start-time <window-start-iso> --end-time <window-end-iso> \
  --period 60 --statistics Sum
```

(Exact namespace/dimensions depend on how the HTTP API's execution logs are
wired into CloudWatch — cross-check against the `live-ninja-ops` dashboard
added in template.yaml for the canonical metric math per contracts.)

## Why not just k6 for the recorded M7 run

k6 was not installed on the machine that ran the M7 load-test task, and
installing new tooling was out of scope for that pass (only
`scripts/loadtest/` is owned by that workstream). `goload.go` was written as
a stdlib-only substitute — same targets, same rate/duration, same
pass/fail bar — so the M7 acceptance numbers could be produced without a new
dependency. `loadtest.js` stays in the repo so a future run (or CI job) can
switch to k6 without redesigning the test.
