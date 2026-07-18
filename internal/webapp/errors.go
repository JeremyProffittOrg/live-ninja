package webapp

import (
	"log/slog"

	"github.com/gofiber/fiber/v2"
)

// ErrorEnvelope is the single canonical JSON error shape returned by every
// Live Ninja web route (and mirrored by the realtime broker and the tool
// router). A caller can always rely on `error.code` for machine dispatch,
// `error.message` for a human-readable explanation, and `error.txId` for a
// support reference that resolves to the exact chain of server log lines.
//
//	{"error": {"code": "quota_exceeded", "message": "...", "txId": "..."}}
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner object of ErrorEnvelope.
type ErrorBody struct {
	// Code is a stable machine-readable identifier (snake_case) the client
	// can branch on; it never changes for a given failure mode even if the
	// human message is reworded.
	Code string `json:"code"`
	// Message is a human-readable, non-sensitive explanation safe to show a
	// user. It must never embed secrets, tokens, or PII.
	Message string `json:"message"`
	// TxID is the per-request transaction id (see TxnMiddleware). The client
	// surfaces it as the "Ref:" the user quotes when reporting the error.
	TxID string `json:"txId"`
}

// errorJSON writes the canonical error envelope for a web route: it stamps
// the request's txId (from TxnMiddleware) into the body, logs the failure
// at ERROR level with that txId — so the returned reference resolves to a
// concrete log line — and sends `{"error":{code,message,txId}}` with the
// given HTTP status.
//
// This is the one path every web route uses to return an error; the status,
// machine code, and human message are the caller's, while the txId
// stamping, logging, and envelope shape are handled here uniformly.
func errorJSON(c *fiber.Ctx, status int, code, message string) error {
	txID := TxID(c)

	requestLogger(c).Error("request failed",
		slog.Int("status", status),
		slog.String("code", code),
		slog.String("message", message),
		slog.String("method", c.Method()),
		slog.String("path", c.Path()),
	)

	return c.Status(status).JSON(ErrorEnvelope{
		Error: ErrorBody{
			Code:    code,
			Message: message,
			TxID:    txID,
		},
	})
}

// requestLogger returns the request-scoped logger TxnMiddleware stashed in
// Locals (already enriched with txId). When it is absent — only in isolated
// handler tests that skip the middleware — it falls back to slog.Default so
// errorJSON never panics.
func requestLogger(c *fiber.Ctx) *slog.Logger {
	if l, ok := c.Locals(localLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
