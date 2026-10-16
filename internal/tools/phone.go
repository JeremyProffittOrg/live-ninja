package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// PhoneMessages uses authenticated owner identity, never a model-supplied user.
type PhoneMessages interface {
	Send(context.Context, string, string, string, string) (map[string]any, *ToolError)
	Status(context.Context, string, string) (map[string]any, *ToolError)
}

func callMessageDefinition() *Definition {
	return &Definition{Name: "call_message", OwnerOnly: true, SideEffecting: true,
		Description: "Only when explicitly requested, call one of the owner's configured cell numbers and read the supplied message. primary is the owner's main cell; secondary is the other configured cell. No arbitrary numbers, automatic redials, or general AI task execution. Returns acceptance/status, not proof a person heard it. Use phone_message_status to check.",
		Params:      []ParamSpec{{Name: "destination", Type: "string", Enum: []string{"primary", "secondary"}, Description: "Configured owner cell alias; default primary."}, {Name: "message", Type: "string", Required: true, MinLen: 1, MaxLen: 1000, Description: "Exact plain-text message to read aloud. No SSML or instructions to change the recipient."}},
		Handler: func(ctx context.Context, d *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
			if d.Phone == nil {
				return nil, toolErrf(CodeNotConfigured, "Owner phone messages are not configured.")
			}
			destination, _ := args["destination"].(string)
			if destination == "" {
				destination = "primary"
			}
			message, _ := args["message"].(string)
			sum := sha256.Sum256([]byte(inv.UserID + "\n" + inv.IdempotencyKey))
			return d.Phone.Send(ctx, inv.UserID, hex.EncodeToString(sum[:]), destination, message)
		}}
}
func phoneMessageStatusDefinition() *Definition {
	return &Definition{Name: "phone_message_status", OwnerOnly: true,
		Description: "Check one owner phone-message request using its returned requestId. Reports provider state, playback evidence and recording availability. Completed playback is not proof a person heard the message.",
		Params:      []ParamSpec{{Name: "requestId", Type: "string", Required: true, MinLen: 1, MaxLen: 100, Description: "Exact requestId returned by call_message."}},
		Handler: func(ctx context.Context, d *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
			if d.Phone == nil {
				return nil, toolErrf(CodeNotConfigured, "Owner phone messages are not configured.")
			}
			return d.Phone.Status(ctx, inv.UserID, args["requestId"].(string))
		}}
}
