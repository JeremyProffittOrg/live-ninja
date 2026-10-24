package webapp

import (
	"context"
	"encoding/json"

	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

type textTurnResult struct {
	Text     string
	Messages []brokerChatMessage
	Calls    []*tools.Result
	Limited  bool
	Failure  *brokerResponse
}

// runTextTurn is the one orchestration path for the HTTP composer and Alexa
// worker. Identity, transport and per-call authority stay with each caller.
func runTextTurn(ctx context.Context, messages []brokerChatMessage,
	model func(context.Context, []brokerChatMessage) (*brokerResponse, error),
	invoke func(context.Context, brokerChatToolCall, int, int) *tools.Result) (*textTurnResult, error) {
	r := &textTurnResult{Messages: append([]brokerChatMessage(nil), messages...), Calls: make([]*tools.Result, 0, 4)}
	for iter := 0; iter < maxFallbackToolIterations; iter++ {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		resp, err := model(ctx, r.Messages)
		if err != nil {
			return r, err
		}
		if resp.Error != "" {
			r.Failure = resp
			return r, nil
		}
		if len(resp.ToolCalls) == 0 {
			r.Text = resp.Text
			r.Messages = append(r.Messages, brokerChatMessage{Role: "assistant", Content: resp.Text})
			return r, nil
		}
		r.Messages = append(r.Messages, brokerChatMessage{Role: "assistant", Content: resp.Text, ToolCalls: resp.ToolCalls})
		for i, tc := range resp.ToolCalls {
			if err := ctx.Err(); err != nil {
				return r, err
			}
			res := invoke(ctx, tc, iter, i)
			r.Calls = append(r.Calls, res)
			out, err := json.Marshal(res)
			if err != nil {
				out = []byte(`{"ok":false,"error":{"code":"internal_error","message":"the tool result could not be serialized"}}`)
			}
			r.Messages = append(r.Messages, brokerChatMessage{Role: "tool", ToolCallID: tc.ID, Content: string(out)})
		}
	}
	r.Text = fallbackToolLimitText
	r.Limited = true
	r.Messages = append(r.Messages, brokerChatMessage{Role: "assistant", Content: r.Text})
	return r, nil
}
