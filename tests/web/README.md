# Web quality gates

Playwright e2e + axe WCAG 2.1 AA + Lighthouse CI for the **public** web surface.
Closes the M3 remainder tracked in `plan.md` WS-1 1.6.

```bash
cd tests/web
npm ci
npx playwright install --with-deps chromium
npx playwright test          # e2e + axe
npx lhci autorun             # Lighthouse assertions
```

Override the target with `LN_BASE_URL` (default `https://live.jeremy.ninja`).

## What is and isn't covered

Only the **unauthenticated** surface. Every interesting screen — `/conversation`,
`/history`, `/memory`, `/personas`, `/downloads` — sits behind a Login-with-Amazon
session that CI has no credentials for. Scanning them without a session would
produce a green result that means nothing, so instead the suite asserts they
*redirect* anonymous visitors and never leak the app shell. Their authed
behaviour stays owner-verified under WS-1 1.4.

The gate runs **after** deploy, against the real origin, because the Fiber app
needs DynamoDB/KMS/SSM to boot — a local harness could only reach `/healthz` and
would miss the CloudFront → API Gateway → authorizer chain where this project's
worst bugs have lived. It is therefore advisory (`continue-on-error`): the deploy
has already happened, so failing it cannot block a bad release. Promote it to a
pre-deploy gate once the app can boot against a local stack.

## Known Windows caveat

`lhci`/`lighthouse` crash on Chrome temp-dir cleanup (`EPERM`) on Windows *after*
writing a valid report. CI is ubuntu-latest and unaffected. To read scores
locally, run `npx lighthouse <url> --output=json --output-path=...` and parse the
file — the report is written before the crash.

## Thresholds

Set in `lighthouserc.json`. Accessibility is pinned at **1.0** deliberately, not
aspirationally: the first run scored 0.98 (`heading-order` — the landing page's
feature cards were `h3` directly under the `h1`) and 0.90 SEO (no meta
description). Both were fixed in the page rather than by lowering the bar, so
the threshold reflects a real, currently-passing state and any regression trips
it. Performance is `warn` only, since it measures a third-party network path.
