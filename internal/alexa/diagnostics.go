package alexa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/aws/smithy-go"
)

// DiagnosticCode never includes an error's message, URL, headers or payload.
func DiagnosticCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrEnvelope):
		return "invalid_envelope"
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrExpired):
		return "expired"
	case errors.Is(err, ErrBusy):
		return "busy"
	case errors.Is(err, ErrReplyQueueFull):
		return "reply_queue_full"
	case errors.Is(err, ErrCancelled), errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, ErrConflict):
		return "conflict"
	}
	var limit *QuotaError
	var quota *realtime.QuotaExceededError
	if errors.As(err, &limit) || errors.As(err, &quota) {
		return "quota_exceeded"
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch code := api.ErrorCode(); code {
		case "AccessDeniedException", "ValidationException", "ResourceNotFoundException", "ThrottlingException", "ProvisionedThroughputExceededException", "TransactionCanceledException", "ConditionalCheckFailedException", "ExpiredTokenException", "ServiceUnavailableException", "InternalServerError":
			return code
		}
	}
	return "dependency_failed"
}

// DiagnosticRef correlates requests without exposing provider/session identities.
func DiagnosticRef(value string) string {
	if value == "" {
		return "missing"
	}
	return digest(value)
}

// LogDiagnostic accepts only code-owned stage names and metadata. Callers must
// never pass conversation text, tool arguments/results, tokens or native IDs.
func LogDiagnostic(ctx context.Context, logger *slog.Logger, stage string, err error, attrs ...slog.Attr) {
	if logger == nil {
		return
	}
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelWarn
	}
	fields := []slog.Attr{slog.String("stage", stage), slog.String("errorCode", DiagnosticCode(err))}
	if err != nil {
		fields = append(fields, slog.String("errorType", fmt.Sprintf("%T", err)))
		var response interface{ HTTPStatusCode() int }
		if errors.As(err, &response) {
			if status := response.HTTPStatusCode(); status >= 100 && status <= 599 {
				fields = append(fields, slog.Int("httpStatus", status))
			}
		}
	}
	logger.LogAttrs(ctx, level, "Alexa diagnostic", append(fields, attrs...)...)
}
