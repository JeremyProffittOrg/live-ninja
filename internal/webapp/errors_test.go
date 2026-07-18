package webapp

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// TestErrorJSONEnvelopeShape: errorJSON emits the canonical
// {"error":{code,message,txId}} body, sets the given status, and stamps the
// txId that TxnMiddleware assigned (matching the X-LN-Txn header).
func TestErrorJSONEnvelopeShape(t *testing.T) {
	logger, _ := newTestLogger()
	app := fiber.New()
	app.Use(TxnMiddleware(logger))
	app.Get("/boom", func(c *fiber.Ctx) error {
		return errorJSON(c, fiber.StatusForbidden, "owner_only", "You don't have access.")
	})

	req := httptest.NewRequest("GET", "/boom", nil)
	req.Header.Set("x-amzn-request-context", `{"requestId":"tx-err-9"}`)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, fiber.MIMEApplicationJSON) {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	var env ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not the error envelope: %v (%s)", err, body)
	}
	if env.Error.Code != "owner_only" {
		t.Errorf("code = %q, want owner_only", env.Error.Code)
	}
	if env.Error.Message != "You don't have access." {
		t.Errorf("message = %q", env.Error.Message)
	}
	if env.Error.TxID != "tx-err-9" {
		t.Errorf("txId = %q, want tx-err-9", env.Error.TxID)
	}
	// Envelope txId must equal the response header the client also reads.
	if h := resp.Header.Get(TxnHeaderName); h != env.Error.TxID {
		t.Errorf("X-LN-Txn header %q != envelope txId %q", h, env.Error.TxID)
	}

	// Assert the exact JSON structure (nested "error" object, three keys).
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(body, &raw)
	if _, ok := raw["error"]; !ok || len(raw) != 1 {
		t.Fatalf("top-level must be a single \"error\" object, got %s", body)
	}
	var inner map[string]json.RawMessage
	_ = json.Unmarshal(raw["error"], &inner)
	for _, k := range []string{"code", "message", "txId"} {
		if _, ok := inner[k]; !ok {
			t.Errorf("error object missing %q key: %s", k, body)
		}
	}
}

// TestErrorJSONLogsAtErrorWithTxID: the helper logs the failure at ERROR
// with the txId so the returned reference resolves to a concrete log line.
func TestErrorJSONLogsAtErrorWithTxID(t *testing.T) {
	logger, buf := newTestLogger()
	app := fiber.New()
	app.Use(TxnMiddleware(logger))
	app.Get("/boom", func(c *fiber.Ctx) error {
		return errorJSON(c, fiber.StatusInternalServerError, "internal", "boom")
	})

	req := httptest.NewRequest("GET", "/boom", nil)
	req.Header.Set("x-amzn-request-context", `{"requestId":"tx-log-1"}`)
	if _, err := app.Test(req); err != nil {
		t.Fatal(err)
	}

	var errLine map[string]any
	for _, l := range parseJSONLines(t, buf.Bytes()) {
		if l["level"] == "ERROR" && l["msg"] == "request failed" {
			errLine = l
		}
	}
	if errLine == nil {
		t.Fatalf("no ERROR log line emitted: %s", buf.String())
	}
	if errLine["txId"] != "tx-log-1" {
		t.Errorf("ERROR line txId = %v, want tx-log-1", errLine["txId"])
	}
	if errLine["code"] != "internal" {
		t.Errorf("ERROR line code = %v, want internal", errLine["code"])
	}
	if status, _ := errLine["status"].(float64); status != 500 {
		t.Errorf("ERROR line status = %v, want 500", errLine["status"])
	}
}

// TestErrorJSONWithoutMiddleware: in an isolated handler test that skips
// TxnMiddleware, errorJSON still produces a valid envelope (empty txId) and
// does not panic on the missing request logger.
func TestErrorJSONWithoutMiddleware(t *testing.T) {
	app := fiber.New()
	app.Get("/boom", func(c *fiber.Ctx) error {
		return errorJSON(c, fiber.StatusBadRequest, "bad_request", "nope")
	})
	resp, err := app.Test(httptest.NewRequest("GET", "/boom", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var env ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("not an envelope: %v (%s)", err, body)
	}
	if env.Error.Code != "bad_request" || env.Error.TxID != "" {
		t.Errorf("unexpected envelope %+v", env.Error)
	}
}
