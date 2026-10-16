# Operator hotel trial bridge

This isolated service accepts Twilio callbacks. It has no call-creation endpoint and
does not expose a public app feature. Deploy a single replica because consent and
stream authorization are held in memory. A restart closes active streams; do not
retry a hotel automatically.

## Runtime contract

Required environment: PUBLIC_BASE_URL (HTTPS origin), TWILIO_ACCOUNT_SID,
TWILIO_AUTH_TOKEN, TWILIO_API_KEY_SID, TWILIO_API_KEY_SECRET,
AZURE_OPENAI_ENDPOINT (HTTPS resource origin), AZURE_OPENAI_DEPLOYMENT,
AZURE_OPENAI_API_KEY, RECORDING_CALLBACK_URL, EVENT_CALLBACK_URL, RUN_ID,
TARGETS_JSON (array of objects with id, name, phone; unique US E.164 phones, max 20).
PORT defaults to 8080. MAX_CALL_SECONDS defaults to 300, permitted range 30..600.
All callback destinations are fixed operator configuration, never caller input.

Originate each independently authorized call with:
- Url: PUBLIC_BASE_URL/voice?run=<URL-encoded RUN_ID>&target=<URL-encoded target id>
- Method: POST
- TimeLimit: MAX_CALL_SECONDS or less (essential provider-side crash bound)
- Recording disabled at creation. The bridge starts recording only after consent.
- StatusCallback: the runner's signed status receiver.

The number's outbound calling permission, budgets, allowlist selection, concurrency,
call count, status reconciliation, event persistence, recording retention/download,
and result email belong to the runner and recorder. This server does not replace
those gates. The runner must never treat /health as a successful model or audio test.
/health only confirms validated runtime configuration and an HTTP listener.

## Phone-menu navigation

Before consent, /voice listens and signed /navigate callbacks select an announced
front-desk, reception, customer-service, guest-services or operator digit. English
is selected only when no relevant service option is announced. Spoken menus receive
"Front desk or customer service, please." Direct human greetings proceed to consent.
This is bounded English prompt matching, not general-purpose IVR understanding.
Unknown keypad menus and voicemail end without guessing digits or leaving messages.
Hold/transfer announcements stay in navigation. Maximum: 75 seconds, eight listening
turns and four selections/requests, within the provider's existing call limit.
Recording and model conversation remain disabled until a likely human greeting and
explicit recording consent. Navigation logs actions only, not unconsented speech.
A transfer after consent still ends the call; new staff do not inherit consent.

Twilio requires Play digits outside Gather. References:
- https://www.twilio.com/docs/voice/twiml/play
- https://www.twilio.com/docs/voice/twiml/gather

## Consent and evidence

The introduction identifies the AI and asks permission to record. Only a short
allowlist of explicit affirmatives or keypad 1 starts recording; uncertainty gets
one clarification, then the call ends. Decline, empty replies, failed evidence
delivery, and recording-start failures do not enter a model conversation.

Twilio REST starts both tracks with dual channels requested. Actual media is
verified by the recorder; no conference recording channel guarantee is implied.
Recording starts after consent, so the consent utterance itself is not recorded.
The signed consent event records the decision and notice. Transfers are declined
and ended rather than continuing with a new person who did not consent.

Event callback JSON is {runId, callSid, targetId, type, at, data}; HMAC-SHA256 of
exact body bytes, hex encoded, is X-LN-Signature, keyed by TWILIO_AUTH_TOKEN.
Types: call_registered, consent (data.result granted/declined), transcript, usage, summary,
error, call_ended. Transcript data has speaker, text, itemId; summary data has
reason, summary, evidence=staff_report_only. Events are sent synchronously with an
8-second HTTP timeout and redirects forbidden. Evidence delivery failure ends the
conversation. Standard output contains status only, never audio or transcripts.

The Azure GA WebSocket uses /openai/v1/realtime?model=<deployment> with api-key
header and audio/pcmu in both directions. Server VAD detects turns and cancels responses; the bridge explicitly requests each
response after reserving tokens. Responses are limited to 512 output tokens and
6000 post-instruction context tokens. Before every response, 12000 input and 512
output tokens must remain within the aggregate 50000/25000 call caps.
The bridge the bridge
clears Twilio audio and truncates the model item to estimated heard duration.
Playback timing is a wall-clock estimate, not sample-perfect hardware feedback.
End-call waits for the final Twilio mark, bounded by 15 seconds. Session duration,
40-second inactivity, upstream failure, and disconnects also close the call.

## Verification

go test ./cmd/phone-trial-bridge -count=1
go vet ./cmd/phone-trial-bridge
go run ./cmd/phone-trial-bridge -probe

Unit and local WebSocket tests do not establish provider deployment availability,
Twilio permission, live telephone audio quality, or recording availability. Verify
those with an authorized pilot before the remainder of the trial.

References:
- https://learn.microsoft.com/en-us/azure/foundry/openai/how-to/realtime-audio-websockets
- https://learn.microsoft.com/en-us/rest/api/aifoundry/azureopenai/realtime
- https://www.twilio.com/docs/usage/security
- https://www.twilio.com/docs/voice/media-streams/websocket-messages
- https://www.twilio.com/docs/voice/api/recording

The Azure-only -probe mode uses the same validated environment, makes no telephone
call, and must succeed before the first call. It authenticates a real WebSocket,
checks the acknowledged PCMU input/output configuration, requests at most 512 output
tokens, and requires nonempty generated audio. Deadline is 20 seconds. Logs contain
only success or sanitized error code/type/parameter, never audio or transcripts.

Final call_ended includes inputTokens, outputTokens, usageComplete. Complete is true
only for no-model calls or normal, fully-accounted model completion with no failed
evidence writes. A cancelled response, abrupt disconnect, or unaccounted response
is incomplete. The recorder receives final totals before the provider hangup is
requested. Missing final evidence means the runner must retain its full reservation.
No bridge event claims a verified Twilio terminal status; provider callbacks own it.
