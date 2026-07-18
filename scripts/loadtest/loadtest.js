// k6 load script for live-ninja's read-only, unauthenticated-safe surface
// (plan.md M7 "Load tests (k6/Vegeta + synthetic-session generator)").
//
// Scope, deliberately narrow: this exercises /healthz, /v1/compat, and the
// intentionally-401 /api/v1/me path only. It does NOT mint realtime sessions,
// invoke tools, or write anything — it is safe to run directly against
// production. A separate, explicitly-authorized exercise would be needed to
// load-test the broker mint path (POST /api/v1/realtime/session) since that
// requires real auth cookies/JWTs and consumes quota-gated resources; do not
// extend this script to hit that route without a throwaway test account and
// explicit sign-off, since it is subject to internal/realtime/quota.go's
// per-user hourly-burn anomaly check (default threshold 200k tokens/hour ->
// auto-suspend) and the M7 concurrent-session BUCKET#sess counter.
//
// k6 is NOT installed on the ops machine as of M7 (see README.md) — this
// script is written and committed for when k6 becomes available, but the
// actual M7 load-test run in the plan was performed with the dependency-free
// scripts/loadtest/goload.go instead. Run this with:
//
//   k6 run --vus 50 --duration 60s scripts/loadtest/loadtest.js
//
// or shape rate directly with an arrival-rate executor (closer to "50 rps"
// than "50 VUs", since VUs != requests/sec once think-time/latency varies):
//
//   k6 run scripts/loadtest/loadtest.js
//
// (the exported `options` below already configures a 50 req/s constant
// arrival-rate scenario for 60s; override via BASE_URL env var if needed.)

import http from 'k6/http';
import { check } from 'k6';
import { Rate, Trend } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'https://live.jeremy.ninja';

const unexpectedStatus = new Rate('unexpected_status');
const healthzDuration = new Trend('healthz_duration', true);
const compatDuration = new Trend('compat_duration', true);
const meDuration = new Trend('api_v1_me_duration', true);

export const options = {
  scenarios: {
    read_only_probe: {
      executor: 'constant-arrival-rate',
      rate: 50, // requests per second, aggregate across all 3 endpoints below
      timeUnit: '1s',
      duration: '60s',
      preAllocatedVUs: 50,
      maxVUs: 200,
    },
  },
  thresholds: {
    // Fail the run if the read-only surface starts erroring or degrading —
    // these mirror plan.md M7's acceptance bar (0 http 5xx, healthy p99).
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(99)<2000'],
    unexpected_status: ['rate<0.01'],
  },
};

// Round-robins targets across VU iterations so the aggregate 50 rps splits
// roughly evenly across the three endpoints rather than each VU hammering
// one path.
const targets = [
  {
    name: 'healthz',
    method: 'GET',
    url: `${BASE_URL}/healthz`,
    want: [200],
    trend: healthzDuration,
  },
  {
    name: 'v1-compat',
    method: 'GET',
    url: `${BASE_URL}/v1/compat`,
    // 200 once M7's GET /v1/compat ships as a public route (contracts/
    // headers.md). Pre-M7-deploy, observed behavior is 401
    // {"error":"unauthorized"} (falls through to Fiber's own RequireAuth,
    // internal/webapp/middleware.go, since no route is registered yet);
    // 404 accepted too in case routing is reshaped in between. All
    // non-5xx outcomes here are treated as non-failing so this script
    // keeps working across the M7 rollout without edits.
    want: [200, 401, 404],
    trend: compatDuration,
  },
  {
    name: 'api-v1-me-401',
    method: 'GET',
    url: `${BASE_URL}/api/v1/me`,
    // Deliberately unauthenticated. Observed: 403 {"message":"Forbidden"}
    // from the API Gateway Lambda-authorizer layer (DefaultAuthorizer,
    // deny-by-default — the request never reaches the web Lambda at all);
    // 401 {"error":"unauthorized"} is also accepted since Fiber's own
    // RequireAuth middleware independently returns 401 for "no
    // credentials" on routes that do reach the app. Either way this must
    // fail closed, never 5xx, never a DB read on the hot path.
    want: [401, 403],
    trend: meDuration,
  },
];

export default function () {
  const t = targets[__ITER % targets.length];
  const res = http.request(t.method, t.url, null, {
    tags: { name: t.name },
    headers: { 'User-Agent': 'live-ninja-k6-loadtest/1.0' },
  });

  const ok = check(res, {
    [`${t.name}: expected status`]: (r) => t.want.includes(r.status),
  });
  unexpectedStatus.add(!ok);
  t.trend.add(res.timings.duration);

  // No sleep: the constant-arrival-rate executor governs pacing, not the
  // iteration body — a sleep here would just shrink effective concurrency
  // headroom under preAllocatedVUs.
}
