# Dated hotel phone trial

The owner authorized twenty California hotel test calls on 2026-10-15 and an
HTML results email with seven-day recording links. This is a bounded operator
trial, not a declaration that the future in-app dialer or Android Auto work is
released. The active requirements and original owner quotes remain in plan.md.

## Execution

The `Phone trial 2026-10-15` GitHub workflow tests the bridge/recorder, deploys
through AWS and Azure OIDC, and runs an actual Azure audio probe before dialing.
It waits until 09:00 America/Los_Angeles and refuses calls after 17:00 or on
another date. It makes one attempt per target, never automatically redials,
and uses both provider and bridge three-minute limits. Existing number routing
is not changed. It uses the uniquely named Live Ninja caller number, or acquires
a dedicated local number within the reserved allowance if none is designated.

Calls ask permission before recording. Refusal or ambiguity ends the call.
The Azure model asks about connection steps and reports staff statements, not
verified VPN compatibility. Transfers to new people end the trial call rather
than silently recording an unconsented participant.

The runner checks current-month Twilio usage, reserves $8 for infrastructure,
and reserves $1.20 per call before dialing. Azure GlobalStandard retail rates
are conservatively applied to all tokens as audio, with no cache/credit discount.
Incomplete model accounting retains the reserve. New calls stop before the
$20 threshold; an explicit owner $5 daily grant is required to continue beyond
it. The trial never creates such a grant itself. The report labels this allocation
as conservative, not an invoice.

## Owner rehearsal

`Owner hotel-call rehearsal 2026-10-16` (`phone-owner-rehearsal.yml`, manual dispatch,
`takes` 1..3) runs the same runner, bridge, recorder and report with the owner playing the
front desk. The only destination is the owner allowlisted number in the
`OWNER_REHEARSAL_NUMBER` secret; the runner refuses any other number. Takes run strictly one
after another with a 45 s pause; every per-call guard above still applies, except the hotel
09:00-17:00 window. The workflow points the bridge at the owner target only for the run and
restores the hotel targets afterwards.

## Recordings and report

The AWS recorder validates signatures, call/recording bindings and media types,
copies recordings to private encrypted S3, and only then marks them ready.
The existing deliverables bucket retains audio for 180 days. Metadata is in the
dedicated trial table; log groups retain operational logs for seven days.
No request bodies or download tokens are logged by the API.

The email sender uses the operator HTML template and SES us-east-1, from
`jeremy@jeremy.ninja` to `proffitt.jeremy@gmail.com`, with that Gmail reply address.
The email contains only real ready-recording links. A seven-day capability URL
redirects to a fresh short-lived S3 URL; an STS presigned URL by itself cannot
guarantee seven days. Cost-only polling creates no links, so the seven-day clock
starts at report generation. Repeated reports do not extend it.

## Recovery

The workflow is serialized. Calls are recorded in a durable intent ledger before
creation. A timeout with unknown call creation blocks automatic continuation.
A blocked run can resume only after known prior calls are terminal; already
attempted targets are skipped. Execution has a three-attempt ceiling. Recording
copy retries are separately limited to three and never cause another phone call.
The cloud bridge scales to zero after the run; S3 recordings and the download
service remain available. No local scheduler or local deployment is used.

## Verification

From the repository root:

    go test -race ./cmd/phone-trial-bridge -count=1
    go vet ./cmd/phone-trial-bridge
    python -B -m unittest discover -s tools/phone-trial -p 'test_*.py' -v

Live acceptance additionally requires the Azure probe, real call results,
recording copy state, and SES MessageId. Mock tests do not count as calls.
