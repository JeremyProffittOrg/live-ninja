package observ

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

// TestWithTxnStampsTxIDField verifies the shared WithTxn helper (txn.go)
// adds the txId under the canonical TxnKey field to every subsequent line.
func TestWithTxnStampsTxIDField(t *testing.T) {
	var buf bytes.Buffer
	logger := WithTxn(NewLogger(&buf, "info"), "tx-123")
	logger.Info("hello")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, buf.String())
	}
	if got := line[TxnKey]; got != "tx-123" {
		t.Fatalf("expected %s=tx-123, got %v", TxnKey, got)
	}
	if TxnKey != "txId" {
		t.Fatalf("TxnKey must be the wire field name %q, got %q", "txId", TxnKey)
	}
}

// TestNewTxnIDIsUniqueUUID verifies fresh ids are distinct, non-empty, and
// UUID-shaped (36 chars, v4 layout).
func TestNewTxnIDIsUniqueUUID(t *testing.T) {
	a, b := NewTxnID(), NewTxnID()
	if a == "" || b == "" {
		t.Fatal("NewTxnID returned empty string")
	}
	if a == b {
		t.Fatalf("NewTxnID returned duplicate ids: %q", a)
	}
	if len(a) != 36 {
		t.Fatalf("expected 36-char UUID, got %d-char %q", len(a), a)
	}
}

func TestRedactHidesSensitiveValues(t *testing.T) {
	in := map[string]string{
		"Authorization":        "Bearer sk-supersecret",
		"authorization":        "Bearer lowercase-variant",
		"Cookie":               "__Host-ln_rt=sess.secret",
		"Set-Cookie":           "__Host-ln_csrf=csrfsecret",
		"X-LN-CSRF":            "csrfsecret",
		"Proxy-Authorization":  "Basic abc",
		"X-Amz-Security-Token": "tok",
		"Content-Type":         "application/json",
		"X-LN-Txn":             "tx-123",
		"Accept":               "text/html",
	}
	out := Redact(in)

	// Sensitive headers redacted (case-insensitive on the name).
	for _, k := range []string{
		"Authorization", "authorization", "Cookie", "Set-Cookie",
		"X-LN-CSRF", "Proxy-Authorization", "X-Amz-Security-Token",
	} {
		if out[k] != redactedValue {
			t.Errorf("header %q should be redacted, got %q", k, out[k])
		}
	}
	// Non-sensitive headers passed through unchanged.
	if out["Content-Type"] != "application/json" {
		t.Errorf("Content-Type should pass through, got %q", out["Content-Type"])
	}
	if out["X-LN-Txn"] != "tx-123" {
		t.Errorf("X-LN-Txn should pass through, got %q", out["X-LN-Txn"])
	}
	if out["Accept"] != "text/html" {
		t.Errorf("Accept should pass through, got %q", out["Accept"])
	}

	// The original map must never be mutated.
	if in["Authorization"] != "Bearer sk-supersecret" {
		t.Errorf("Redact mutated the input map: %q", in["Authorization"])
	}

	// Redacted output must not contain any secret substring.
	blob, _ := json.Marshal(out)
	for _, secret := range []string{"supersecret", "sess.secret", "csrfsecret", "lowercase-variant"} {
		if bytes.Contains(blob, []byte(secret)) {
			t.Errorf("redacted output leaked secret %q: %s", secret, blob)
		}
	}
}

func TestRedactNilAndEmpty(t *testing.T) {
	if Redact(nil) != nil {
		t.Error("Redact(nil) should return nil")
	}
	if got := Redact(map[string]string{}); len(got) != 0 {
		t.Errorf("Redact(empty) should be empty, got %v", got)
	}
}

// TestRedactComposesWithLogger is a belt-and-suspenders check that a
// redacted header map logs without leaking values.
func TestRedactComposesWithLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, "info")
	logger.Info("request", slog.Any("headers", Redact(map[string]string{
		"Authorization": "Bearer secret-token",
	})))
	if bytes.Contains(buf.Bytes(), []byte("secret-token")) {
		t.Errorf("logged output leaked Authorization value: %s", buf.String())
	}
}
