package webapp

// M17 phase 2: authenticated web-client breadcrumbs for tool failures that
// occur before POST /api/v1/tools/invoke reaches the tool registry. Identity
// is always derived from the verified access token; the browser only supplies
// bounded diagnostic context.

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/rca"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

const (
	clientRCAEnvelopeVersion = 1
	clientRCASource          = "web-client"

	maxClientRCAToolRunes      = 128
	maxClientRCAErrorCodeRunes = 128
	maxClientRCAMessageRunes   = 1024
	maxClientRCAIDRunes        = 256
	maxClientRCAArgsBytes      = 2048
	maxClientRCABodyBytes      = 16 << 10
)

type clientRCAEvent struct {
	Tool      string          `json:"tool"`
	CallID    string          `json:"callId"`
	SessionID string          `json:"sessionId"`
	Engine    string          `json:"engine"`
	Args      json.RawMessage `json:"args"`
	Error     struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		TxID    string `json:"txId"`
	} `json:"error"`
}

func handleRCAClientEvent(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		enqueuer := rca.NewSQSEnqueuer(deps.SQS, deps.SQSRcaURL)
		if enqueuer == nil {
			return errorJSON(c, fiber.StatusServiceUnavailable, tools.CodeNotConfigured,
				"Client failure reporting is not configured.")
		}
		if len(c.Body()) > maxClientRCABodyBytes {
			return errorJSON(c, fiber.StatusRequestEntityTooLarge, "payload_too_large",
				"Client failure report is too large.")
		}

		var body clientRCAEvent
		if err := json.Unmarshal(c.Body(), &body); err != nil {
			return apiBadRequest(c, "body must be valid JSON")
		}

		tool := strings.TrimSpace(body.Tool)
		code := strings.TrimSpace(body.Error.Code)
		message := strings.TrimSpace(body.Error.Message)
		if tool == "" || message == "" {
			return apiBadRequest(c, "tool and error message are required")
		}
		if code == "" {
			code = "client_tool_error"
		}

		argsJSON := "{}"
		if len(body.Args) > 0 && string(body.Args) != "null" {
			var args map[string]any
			if err := json.Unmarshal(body.Args, &args); err != nil {
				return apiBadRequest(c, "args must be a JSON object")
			}
			if encoded, err := json.Marshal(args); err == nil {
				if len(encoded) <= maxClientRCAArgsBytes {
					argsJSON = string(encoded)
				} else {
					// Keep the envelope valid and bounded. The failure identity
					// still carries the tool and error; oversized client args
					// must not approach SQS/DynamoDB/prompt limits.
					argsJSON = `{"_truncated":true}`
				}
			}
		}

		failure := tools.ToolFailure{
			V:            clientRCAEnvelopeVersion,
			Source:       clientRCASource,
			Tool:         clampClientRCARunes(tool, maxClientRCAToolRunes),
			ErrorCode:    clampClientRCARunes(code, maxClientRCAErrorCodeRunes),
			ErrorMessage: clampClientRCARunes(message, maxClientRCAMessageRunes),
			ArgsJSON:     argsJSON,
			CallID:       clampClientRCARunes(strings.TrimSpace(body.CallID), maxClientRCAIDRunes),
			TxID:         clampClientRCARunes(strings.TrimSpace(body.Error.TxID), maxClientRCAIDRunes),
			UserID:       UserID(c),
			SessionID:    clampClientRCARunes(strings.TrimSpace(body.SessionID), maxClientRCAIDRunes),
			Surface:      Surface(c),
			Role:         Role(c),
			Engine:       clampClientRCARunes(strings.TrimSpace(body.Engine), maxClientRCAIDRunes),
			OccurredAt:   time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := enqueuer.EnqueueToolFailure(c.UserContext(), failure); err != nil {
			return apiInternalError(c, deps, "enqueue client RCA event", err)
		}
		return c.SendStatus(fiber.StatusAccepted)
	}
}

func clampClientRCARunes(value string, max int) string {
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	return string([]rune(value)[:max])
}
