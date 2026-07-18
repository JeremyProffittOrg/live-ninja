package observ

import (
	"log/slog"

	"github.com/google/uuid"
)

// TxnKey is the JSON field name every structured log line carries for the
// per-transaction correlation id ("txId"). A single request/invocation is
// stamped with one txId at ingress (the API Gateway requestId when present,
// otherwise a fresh UUID v4) and that id is threaded — via WithTxn — into
// every downstream slog line and every error returned to the client, so a
// user-reported "Ref: <txId>" pins the exact transaction in CloudWatch.
const TxnKey = "txId"

// WithTxn returns a logger that stamps every line with the transaction id
// under the standard TxnKey field. It composes with WithRequest: callers
// typically wrap the request-enriched logger so requestId, userId, surface,
// and txId all appear together.
func WithTxn(logger *slog.Logger, txID string) *slog.Logger {
	return logger.With(slog.String(TxnKey, txID))
}

// NewTxnID mints a fresh transaction id (UUID v4). Use it at ingress when
// the caller did not already supply a txId (or an upstream requestId to
// reuse) so every transaction is correlatable end to end.
func NewTxnID() string {
	return uuid.NewString()
}
