// telemetry_routes.go owns POST /api/v1/telemetry
// (contracts/telemetry.schema.json): batched, client-side-sampled events
// from the web/Android surfaces (M5Stack's own telemetry travels over
// MQTT -> the IoT Rule -> the same Firehose stream, not through this
// HTTP route) validated server-side against the schema's shape and
// forwarded to the M7 telemetry lake (Kinesis Firehose Direct PUT -> S3
// -> Glue/Athena, wired in template.yaml).
//
// Hard invariant (schema + review discipline both enforce this): a
// telemetry event NEVER carries transcript content. This handler
// independently rejects any event whose attrs object contains a banned
// transcript-shaped key, so a client bug — or a modified/malicious
// client — can't leak raw speech into the analytics lake even if the
// client-side check is skipped.
package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	firehosetypes "github.com/aws/aws-sdk-go-v2/service/firehose/types"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
)

// FirehosePutBatchAPI is the subset of the Kinesis Firehose client the
// telemetry route needs (PutRecordBatch onto the telemetry Direct-PUT
// delivery stream). Interface-typed so tests inject a fake without a
// live delivery stream.
type FirehosePutBatchAPI interface {
	PutRecordBatch(ctx context.Context, params *firehose.PutRecordBatchInput, optFns ...func(*firehose.Options)) (*firehose.PutRecordBatchOutput, error)
}

// telemetryMaxBatch is contracts/telemetry.schema.json / plan.md M7's
// "max 25/batch" cap. A batch over this size is a client bug (violating
// its own sampling/batching contract), not a transient condition —
// reject the whole request rather than silently truncating it, so the
// client's bug surfaces instead of silently losing events forever.
const telemetryMaxBatch = 25

// telemetryBannedAttrKeys are the transcript-content-carrier attr key
// names contracts/telemetry.schema.json's `attrs.not` clause bans
// outright. Enforced here too (defense in depth) independent of the
// schema-level check any client-side validator performs.
var telemetryBannedAttrKeys = []string{
	"transcript", "utterance", "responseText", "userSpeech", "assistantSpeech", "text",
}

// telemetryKnownEvents mirrors the schema's `event` enum for
// observability only (an EMF metric flags an unrecognized-but-otherwise-
// well-shaped event name) — it is NEVER used to reject an event:
// contracts/README.md rule 1 says new event names may be added
// additively without a schema bump, so this handler must accept them.
var telemetryKnownEvents = map[string]bool{
	"session_started": true, "session_ended": true, "wake_word_detected": true,
	"barge_in": true, "tool_invoked": true, "tool_result": true,
	"fallback_engaged": true, "quota_warning": true, "quota_exceeded": true,
	"settings_synced": true, "wakeword_model_verify_failed": true,
	"wakeword_model_unsupported_format": true, "device_heartbeat": true,
	"device_error": true, "auth_new_signin": true, "ota_started": true,
	"ota_completed": true, "ota_rolled_back": true,
}

var telemetrySurfaces = map[string]bool{"web": true, "android": true, "m5stack": true, "backend": true}

// telemetryEvent mirrors contracts/telemetry.schema.json's envelope.
// decodeTelemetryEvent (not this struct's tags alone) enforces the
// schema's top-level `additionalProperties: false` via
// json.Decoder.DisallowUnknownFields.
type telemetryEvent struct {
	Event     string         `json:"event"`
	DeviceID  *string        `json:"deviceId"`
	Surface   string         `json:"surface"`
	TS        string         `json:"ts"`
	SessionID string         `json:"sessionId"`
	UserID    *string        `json:"userId"`
	Attrs     map[string]any `json:"attrs"`
}

// RegisterTelemetryRoutes mounts POST /api/v1/telemetry behind its own
// authenticated /api/v1 group — same posture as every other route-file
// registrar in this package (deliverables_routes.go, history_routes.go,
// etc.): RequireAuth is applied locally so this file's route is
// fail-closed independent of any other registrar's group.
func RegisterTelemetryRoutes(app *fiber.App, deps *Deps) {
	api := app.Group("/api/v1", RequireAuth())
	api.Post("/telemetry", handleTelemetryIngest(deps))
}

func handleTelemetryIngest(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Firehose == nil || strings.TrimSpace(deps.TelemetryStreamName) == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "not_configured", "message": "the telemetry lake is not configured",
			})
		}

		userID := UserID(c)

		var body struct {
			Events []json.RawMessage `json:"events"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if len(body.Events) == 0 {
			return apiBadRequest(c, "events must be a non-empty array")
		}
		if len(body.Events) > telemetryMaxBatch {
			return apiBadRequest(c, fmt.Sprintf("events batch exceeds the max of %d per contracts/telemetry.schema.json", telemetryMaxBatch))
		}

		records := make([]firehosetypes.Record, 0, len(body.Events))
		rejected := 0
		for _, raw := range body.Events {
			ev, err := decodeTelemetryEvent(raw)
			if err != nil {
				deps.Log.Warn("telemetry: rejected malformed event",
					slog.String("error", err.Error()), slog.String("userId", userID))
				rejected++
				continue
			}
			if !telemetryKnownEvents[ev.Event] {
				observ.EmitMetric("LiveNinja/Telemetry", "UnknownEventName", 1, "Count",
					map[string]string{"event": ev.Event, "surface": ev.Surface})
			}

			// Never trust a client-asserted userId over the verified auth
			// context (NFR-02's anti-confused-deputy posture, applied here
			// too) — the authenticated caller's own id always wins.
			if userID != "" {
				ev.UserID = &userID
			}

			data, err := json.Marshal(ev)
			if err != nil {
				deps.Log.Error("telemetry: marshal accepted event failed", slog.String("error", err.Error()))
				rejected++
				continue
			}
			records = append(records, firehosetypes.Record{Data: append(data, '\n')})
		}

		accepted := len(records)
		if accepted > 0 {
			out, err := deps.Firehose.PutRecordBatch(c.Context(), &firehose.PutRecordBatchInput{
				DeliveryStreamName: aws.String(deps.TelemetryStreamName),
				Records:            records,
			})
			switch {
			case err != nil:
				// Best-effort sink: a Firehose outage must never fail the
				// client's batch flush (the client would otherwise retry
				// the same batch and pile up) — log and report zero
				// accepted rather than propagating a 5xx.
				deps.Log.Error("telemetry: firehose PutRecordBatch failed",
					slog.String("error", err.Error()), slog.Int("count", accepted))
				accepted = 0
			case out != nil && aws.ToInt32(out.FailedPutCount) > 0:
				failed := int(aws.ToInt32(out.FailedPutCount))
				deps.Log.Warn("telemetry: partial firehose put failure",
					slog.Int("failedPutCount", failed), slog.Int("submitted", len(records)))
				accepted -= failed
			}
		}

		return c.JSON(fiber.Map{"ok": true, "accepted": accepted, "rejected": rejected})
	}
}

// decodeTelemetryEvent parses and validates one batch element against
// contracts/telemetry.schema.json: required fields present and
// well-typed, `surface` in the known enum, `ts` a valid RFC-3339
// timestamp, `sessionId` non-empty (the literal "none" is a valid value
// for session-less events, not special-cased here), `attrs` present, and
// no banned transcript-content-carrier key in `attrs`.
// DisallowUnknownFields enforces the schema's top-level
// `additionalProperties: false` — a stray top-level field is a client
// bug, unlike `attrs`' own keys, which ARE additive per the schema.
func decodeTelemetryEvent(raw json.RawMessage) (telemetryEvent, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var ev telemetryEvent
	if err := dec.Decode(&ev); err != nil {
		return telemetryEvent{}, fmt.Errorf("decode: %w", err)
	}

	if strings.TrimSpace(ev.Event) == "" {
		return telemetryEvent{}, errors.New("event is required")
	}
	if !telemetrySurfaces[ev.Surface] {
		return telemetryEvent{}, fmt.Errorf("surface %q is not one of web|android|m5stack|backend", ev.Surface)
	}
	if strings.TrimSpace(ev.TS) == "" {
		return telemetryEvent{}, errors.New("ts is required")
	}
	if _, err := time.Parse(time.RFC3339, ev.TS); err != nil {
		return telemetryEvent{}, fmt.Errorf("ts is not a valid RFC-3339 timestamp: %w", err)
	}
	if strings.TrimSpace(ev.SessionID) == "" {
		return telemetryEvent{}, errors.New(`sessionId is required (use "none" when there is no session context)`)
	}
	if ev.Attrs == nil {
		return telemetryEvent{}, errors.New("attrs is required")
	}
	for _, banned := range telemetryBannedAttrKeys {
		if _, present := ev.Attrs[banned]; present {
			return telemetryEvent{}, fmt.Errorf("attrs.%s is a banned transcript-content-carrier key", banned)
		}
	}

	return ev, nil
}
