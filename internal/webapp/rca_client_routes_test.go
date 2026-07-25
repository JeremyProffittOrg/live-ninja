package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

func newClientRCAApp(deps *Deps) *fiber.App {
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "user-1")
		c.Locals(localSessionID, "auth-session")
		c.Locals(localSurface, "web")
		c.Locals(localRole, "owner")
		return c.Next()
	})
	app.Post("/api/v1/rca/client-event", handleRCAClientEvent(deps))
	return app
}

func TestRCAClientEventEnqueuesServerAttributedBreadcrumb(t *testing.T) {
	queue := &fakeSQS{}
	app := newClientRCAApp(&Deps{
		SQS: queue, SQSRcaURL: "https://sqs.example/live-ninja-rca", Log: testLogger(),
	})
	body := `{
		"tool":"get_weather",
		"callId":"call-7",
		"sessionId":"voice-session-9",
		"engine":"openai-realtime",
		"args":{"location":"Charlotte"},
		"error":{"code":"transport_error","message":"connection reset","txId":"original-tx"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rca/client-event", strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	calls := queue.calls()
	if len(calls) != 1 {
		t.Fatalf("SQS calls = %d, want 1", len(calls))
	}
	var got tools.ToolFailure
	if err := json.Unmarshal([]byte(aws.ToString(calls[0].MessageBody)), &got); err != nil {
		t.Fatal(err)
	}
	if got.V != 1 || got.Source != "web-client" || got.UserID != "user-1" ||
		got.Surface != "web" || got.Role != "owner" {
		t.Fatalf("server attribution drifted: %#v", got)
	}
	if got.SessionID != "voice-session-9" || got.Engine != "openai-realtime" ||
		got.TxID != "original-tx" || got.ArgsJSON != `{"location":"Charlotte"}` {
		t.Fatalf("client diagnostic context drifted: %#v", got)
	}
}

func TestRCAClientEventRejectsMalformedAndFailsClosedWhenUnconfigured(t *testing.T) {
	configured := newClientRCAApp(&Deps{
		SQS: &fakeSQS{}, SQSRcaURL: "https://sqs.example/live-ninja-rca", Log: testLogger(),
	})
	for _, body := range []string{
		`{`,
		`{"tool":"","error":{"code":"x","message":"y"}}`,
		`{"tool":"x","args":[],"error":{"code":"x","message":"y"}}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/rca/client-event", strings.NewReader(body))
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		resp, err := configured.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q status = %d, want 400", body, resp.StatusCode)
		}
	}

	unconfigured := newClientRCAApp(&Deps{Log: testLogger()})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rca/client-event",
		strings.NewReader(`{"tool":"x","error":{"code":"x","message":"y"}}`))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := unconfigured.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status = %d, want 503", resp.StatusCode)
	}
}

func TestRCAClientEventDefaultsMissingCodeAndBoundsBody(t *testing.T) {
	queue := &fakeSQS{}
	app := newClientRCAApp(&Deps{
		SQS: queue, SQSRcaURL: "https://sqs.example/live-ninja-rca", Log: testLogger(),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rca/client-event",
		strings.NewReader(`{"tool":"get_weather","error":{"message":"network failed"}}`))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("missing-code status = %d, want 202", resp.StatusCode)
	}
	var got tools.ToolFailure
	if err := json.Unmarshal([]byte(aws.ToString(queue.calls()[0].MessageBody)), &got); err != nil {
		t.Fatal(err)
	}
	if got.ErrorCode != "client_tool_error" {
		t.Fatalf("error code = %q, want client_tool_error", got.ErrorCode)
	}

	oversized := `{"tool":"x","error":{"code":"x","message":"` +
		strings.Repeat("x", maxClientRCABodyBytes) + `"}}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/rca/client-event",
		strings.NewReader(oversized))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want 413", resp.StatusCode)
	}
}
