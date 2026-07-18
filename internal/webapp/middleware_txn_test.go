package webapp

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})), &buf
}

// TestTxnMiddlewarePrefersAPIGatewayRequestID: when the forwarded request
// context carries a requestId, the txId (and X-LN-Txn header) is exactly it.
func TestTxnMiddlewarePrefersAPIGatewayRequestID(t *testing.T) {
	logger, _ := newTestLogger()
	app := fiber.New()
	app.Use(TxnMiddleware(logger))
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString(TxID(c))
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("x-amzn-request-context", `{"requestId":"apigw-req-42","http":{"method":"GET"}}`)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get(TxnHeaderName); got != "apigw-req-42" {
		t.Fatalf("X-LN-Txn = %q, want apigw-req-42", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "apigw-req-42" {
		t.Fatalf("Locals txId = %q, want apigw-req-42", body)
	}
}

// TestTxnMiddlewareGeneratesUUIDWhenNoRequestID: no request context ->
// a freshly minted UUID v4 in both the header and Locals, and they match.
func TestTxnMiddlewareGeneratesUUIDWhenNoRequestID(t *testing.T) {
	logger, _ := newTestLogger()
	app := fiber.New()
	app.Use(TxnMiddleware(logger))
	app.Get("/", func(c *fiber.Ctx) error { return c.SendString(TxID(c)) })

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	hdr := resp.Header.Get(TxnHeaderName)
	if len(hdr) != 36 {
		t.Fatalf("expected 36-char UUID in X-LN-Txn, got %q", hdr)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != hdr {
		t.Fatalf("Locals txId %q != header %q", body, hdr)
	}
}

// TestTxnMiddlewareIgnoresMalformedRequestContext: garbage in the header
// falls back to a UUID rather than erroring or emitting an empty txId.
func TestTxnMiddlewareIgnoresMalformedRequestContext(t *testing.T) {
	logger, _ := newTestLogger()
	app := fiber.New()
	app.Use(TxnMiddleware(logger))
	app.Get("/", func(c *fiber.Ctx) error { return c.SendStatus(200) })

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("x-amzn-request-context", "not-json{{{")
	resp, _ := app.Test(req)
	if got := resp.Header.Get(TxnHeaderName); len(got) != 36 {
		t.Fatalf("expected UUID fallback, got %q", got)
	}
}

// TestTxnMiddlewareLogsRequestAndResponse: verbose pair emitted with txId,
// status, latency, query keys (keys only, not values), and redacted auth.
func TestTxnMiddlewareLogsRequestAndResponse(t *testing.T) {
	logger, buf := newTestLogger()
	app := fiber.New()
	app.Use(TxnMiddleware(logger))
	app.Get("/thing", func(c *fiber.Ctx) error {
		return c.Status(201).SendString("ok")
	})

	req := httptest.NewRequest("GET", "/thing?email=secret@example.com&code=abc", nil)
	req.Header.Set("Authorization", "Bearer super-secret-token")
	req.Header.Set("x-amzn-request-context", `{"requestId":"rq-1"}`)
	if _, err := app.Test(req); err != nil {
		t.Fatal(err)
	}

	lines := parseJSONLines(t, buf.Bytes())
	var reqLine, respLine map[string]any
	for _, l := range lines {
		switch l["msg"] {
		case "request":
			reqLine = l
		case "response":
			respLine = l
		}
	}
	if reqLine == nil || respLine == nil {
		t.Fatalf("missing request/response log lines, got %d lines: %s", len(lines), buf.String())
	}

	// txId threaded into both lines.
	if reqLine["txId"] != "rq-1" || respLine["txId"] != "rq-1" {
		t.Errorf("txId not on both lines: req=%v resp=%v", reqLine["txId"], respLine["txId"])
	}
	// query keys logged as names only, no values.
	if qk, _ := reqLine["query_keys"].(string); qk != "code,email" {
		t.Errorf("query_keys = %q, want \"code,email\"", qk)
	}
	// response line carries the real status.
	if status, _ := respLine["status"].(float64); status != 201 {
		t.Errorf("response status = %v, want 201", respLine["status"])
	}
	// No secret leaked anywhere in the logs.
	all := buf.String()
	for _, secret := range []string{"super-secret-token", "secret@example.com"} {
		if strings.Contains(all, secret) {
			t.Errorf("logs leaked secret %q: %s", secret, all)
		}
	}
}

func parseJSONLines(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ln, &m); err != nil {
			t.Fatalf("bad log line %q: %v", ln, err)
		}
		out = append(out, m)
	}
	return out
}
