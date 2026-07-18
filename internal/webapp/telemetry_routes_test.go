package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	firehosetypes "github.com/aws/aws-sdk-go-v2/service/firehose/types"
	"github.com/gofiber/fiber/v2"
)

// fakeFirehose captures every PutRecordBatch call so tests can assert on
// what was actually sent, and can be configured to fail (or partially
// fail) the next call.
type fakeFirehose struct {
	calls      [][]firehosetypes.Record
	streamName string
	err        error
	failCount  int32
}

func (f *fakeFirehose) PutRecordBatch(_ context.Context, params *firehose.PutRecordBatchInput, _ ...func(*firehose.Options)) (*firehose.PutRecordBatchOutput, error) {
	f.streamName = aws.ToString(params.DeliveryStreamName)
	f.calls = append(f.calls, params.Records)
	if f.err != nil {
		return nil, f.err
	}
	return &firehose.PutRecordBatchOutput{FailedPutCount: aws.Int32(f.failCount)}, nil
}

func newTelemetryApp(t *testing.T, firehoseClient FirehosePutBatchAPI, stream string) (*fiber.App, *Deps) {
	t.Helper()
	deps := &Deps{
		Log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		Firehose:            firehoseClient,
		TelemetryStreamName: stream,
	}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		return c.Next()
	})
	RegisterTelemetryRoutes(app, deps)
	return app, deps
}

func validEvent(event string) map[string]any {
	return map[string]any{
		"event":     event,
		"surface":   "web",
		"ts":        "2026-07-17T20:31:00Z",
		"sessionId": "sess-1",
		"attrs":     map[string]any{"engine": "openai-realtime"},
	}
}

func TestTelemetryRouteNotConfigured(t *testing.T) {
	app, _ := newTelemetryApp(t, nil, "")
	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/telemetry", map[string]any{
		"events": []any{validEvent("session_started")},
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "not_configured" {
		t.Errorf("error = %v, want not_configured", body["error"])
	}
}

func TestTelemetryRouteAcceptsValidBatch(t *testing.T) {
	fake := &fakeFirehose{}
	app, _ := newTelemetryApp(t, fake, "ln-telemetry-stream")

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/telemetry", map[string]any{
		"events": []any{validEvent("session_started"), validEvent("barge_in")},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if int(body["accepted"].(float64)) != 2 {
		t.Errorf("accepted = %v, want 2", body["accepted"])
	}
	if int(body["rejected"].(float64)) != 0 {
		t.Errorf("rejected = %v, want 0", body["rejected"])
	}
	if len(fake.calls) != 1 || len(fake.calls[0]) != 2 {
		t.Fatalf("firehose calls = %+v, want one call with 2 records", fake.calls)
	}
	if fake.streamName != "ln-telemetry-stream" {
		t.Errorf("stream name = %q, want ln-telemetry-stream", fake.streamName)
	}

	// The authenticated caller's own userId must be stamped onto every
	// record, overriding whatever (if anything) the client sent.
	var sent telemetryEvent
	if err := json.Unmarshal(fake.calls[0][0].Data, &sent); err != nil {
		t.Fatalf("decode sent record: %v", err)
	}
	if sent.UserID == nil || *sent.UserID != "u1" {
		t.Errorf("sent userId = %v, want u1", sent.UserID)
	}
}

func TestTelemetryRouteRejectsBannedAttrKeys(t *testing.T) {
	fake := &fakeFirehose{}
	app, _ := newTelemetryApp(t, fake, "stream")

	bad := validEvent("session_ended")
	bad["attrs"] = map[string]any{"transcript": "hello there"}

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/telemetry", map[string]any{
		"events": []any{validEvent("session_started"), bad},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (partial acceptance)", resp.StatusCode)
	}
	if int(body["accepted"].(float64)) != 1 {
		t.Errorf("accepted = %v, want 1", body["accepted"])
	}
	if int(body["rejected"].(float64)) != 1 {
		t.Errorf("rejected = %v, want 1", body["rejected"])
	}
	if len(fake.calls) != 1 || len(fake.calls[0]) != 1 {
		t.Fatalf("firehose calls = %+v, want one call with exactly the good record", fake.calls)
	}
}

func TestTelemetryRouteRejectsUnknownTopLevelField(t *testing.T) {
	app, _ := newTelemetryApp(t, &fakeFirehose{}, "stream")

	ev := validEvent("session_started")
	ev["utterance"] = "not allowed at all" // banned key AND unknown top-level field

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/telemetry", map[string]any{
		"events": []any{ev},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if int(body["accepted"].(float64)) != 0 || int(body["rejected"].(float64)) != 1 {
		t.Errorf("accepted/rejected = %v/%v, want 0/1", body["accepted"], body["rejected"])
	}
}

func TestTelemetryRouteRejectsOversizedBatch(t *testing.T) {
	app, _ := newTelemetryApp(t, &fakeFirehose{}, "stream")

	events := make([]any, telemetryMaxBatch+1)
	for i := range events {
		events[i] = validEvent("session_started")
	}
	resp, _ := doJSON(t, app, http.MethodPost, "/api/v1/telemetry", map[string]any{"events": events})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestTelemetryRouteRejectsEmptyBatch(t *testing.T) {
	app, _ := newTelemetryApp(t, &fakeFirehose{}, "stream")
	resp, _ := doJSON(t, app, http.MethodPost, "/api/v1/telemetry", map[string]any{"events": []any{}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestTelemetryRouteFirehoseFailureDegradesGracefully(t *testing.T) {
	fake := &fakeFirehose{err: errors.New("throttled")}
	app, _ := newTelemetryApp(t, fake, "stream")

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/telemetry", map[string]any{
		"events": []any{validEvent("session_started")},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a Firehose outage must not fail the client's flush)", resp.StatusCode)
	}
	if int(body["accepted"].(float64)) != 0 {
		t.Errorf("accepted = %v, want 0", body["accepted"])
	}
}

func TestTelemetryRoutePartialFirehoseFailure(t *testing.T) {
	fake := &fakeFirehose{failCount: 1}
	app, _ := newTelemetryApp(t, fake, "stream")

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/telemetry", map[string]any{
		"events": []any{validEvent("session_started"), validEvent("barge_in")},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if int(body["accepted"].(float64)) != 1 {
		t.Errorf("accepted = %v, want 1 (2 submitted, 1 failed)", body["accepted"])
	}
}

func TestDecodeTelemetryEvent(t *testing.T) {
	marshal := func(m map[string]any) json.RawMessage {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		return b
	}

	if _, err := decodeTelemetryEvent(marshal(validEvent("session_started"))); err != nil {
		t.Errorf("valid event rejected: %v", err)
	}

	missingEvent := validEvent("session_started")
	missingEvent["event"] = ""
	if _, err := decodeTelemetryEvent(marshal(missingEvent)); err == nil {
		t.Error("empty event name accepted, want rejection")
	}

	badSurface := validEvent("session_started")
	badSurface["surface"] = "desktop"
	if _, err := decodeTelemetryEvent(marshal(badSurface)); err == nil {
		t.Error("unknown surface accepted, want rejection")
	}

	badTS := validEvent("session_started")
	badTS["ts"] = "not-a-timestamp"
	if _, err := decodeTelemetryEvent(marshal(badTS)); err == nil {
		t.Error("malformed ts accepted, want rejection")
	}

	noSession := validEvent("session_started")
	noSession["sessionId"] = ""
	if _, err := decodeTelemetryEvent(marshal(noSession)); err == nil {
		t.Error("empty sessionId accepted, want rejection")
	}

	noAttrs := validEvent("session_started")
	delete(noAttrs, "attrs")
	if _, err := decodeTelemetryEvent(marshal(noAttrs)); err == nil {
		t.Error("missing attrs accepted, want rejection")
	}

	for _, banned := range telemetryBannedAttrKeys {
		ev := validEvent("session_started")
		ev["attrs"] = map[string]any{banned: "some content"}
		if _, err := decodeTelemetryEvent(marshal(ev)); err == nil {
			t.Errorf("banned attrs key %q accepted, want rejection", banned)
		}
	}

	stray := validEvent("session_started")
	stray["notAFieldInTheSchema"] = "x"
	if _, err := decodeTelemetryEvent(marshal(stray)); err == nil {
		t.Error("unknown top-level field accepted, want rejection (additionalProperties: false)")
	}

	// Forward-compat: an unrecognized-but-well-shaped event name must be
	// accepted (contracts/README.md rule 1 — additive event names).
	if _, err := decodeTelemetryEvent(marshal(validEvent("a_brand_new_future_event"))); err != nil {
		t.Errorf("forward-compat event name rejected: %v", err)
	}
}
