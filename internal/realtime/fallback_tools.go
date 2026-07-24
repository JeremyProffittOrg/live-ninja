package realtime

// Tool-capable fallback turn (plan.md M2 fallback cascade follow-up): the
// text-only degraded path historically ran a bare chat completion with no
// tools, so a typed message with no live realtime session got "I can't do
// that" for anything tool-shaped. This file gives the broker's
// "fallback-turn" mode the SAME tool catalog every realtime session gets:
// the static manifest in mint.go is re-rendered (not forked) into the
// chat-completions `tools` wrapper, and TurnWithTools returns the model's
// tool_calls verbatim — execution never happens here. The broker holds
// only the OpenAI key; the web function owns the tool loop and executes
// each call through internal/tools (the same re-authz/idempotency/audit
// pipeline as POST /api/v1/tools/invoke).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ChatToolCall is one model-requested function call from a chat
// completion, flattened from OpenAI's {id, type:"function",
// function:{name, arguments}} wire shape. Arguments is the raw JSON
// string exactly as the model produced it — the executing side parses
// and schema-validates it (internal/tools), never this package.
type ChatToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatMessage is one turn in a fallback tool-loop conversation, in the
// broker's invoke-payload shape (the web function builds these; the
// broker converts them to OpenAI chat-completions messages).
//
//   - role "user"/"assistant": Content carries the text; an assistant
//     message additionally carries the ToolCalls it requested.
//   - role "tool": the result of one executed call — ToolCallID pairs it
//     with the assistant's ChatToolCall.ID and Content carries the
//     serialized tool Result JSON.
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []ChatToolCall `json:"toolCalls,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
}

// TurnResult is one tool-capable fallback turn's outcome: either the
// final assistant text, or the tool calls the model wants executed
// (in which case the caller executes them and re-invokes with the
// results appended).
type TurnResult struct {
	Text      string         `json:"text,omitempty"`
	ToolCalls []ChatToolCall `json:"toolCalls,omitempty"`
}

// chatCompletionTools is the realtime tool manifest (mint.go — the single
// catalog source) re-wrapped into the chat-completions `tools` shape:
// Realtime declares functions flat ({type, name, description, parameters})
// while chat completions nests them ({type, function:{...}}). Derived once
// at init so the two views can never drift.
var chatCompletionTools = func() []map[string]any {
	out := make([]map[string]any, 0, len(toolManifest))
	for _, t := range toolManifest {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t["name"],
				"description": t["description"],
				"parameters":  t["parameters"],
			},
		})
	}
	return out
}()

// ValidateChatMessages enforces the broker-side invariants on a
// caller-supplied fallback message array before it is sent anywhere:
// non-empty, roles limited to user/assistant/tool (the system prompt is
// always the server-resolved persona — never client input), tool
// messages paired to a call id, and non-tool messages carrying either
// text or tool calls.
func ValidateChatMessages(msgs []ChatMessage) error {
	if len(msgs) == 0 {
		return fmt.Errorf("messages must be a non-empty array")
	}
	for i, m := range msgs {
		switch m.Role {
		case "user":
			if strings.TrimSpace(m.Content) == "" {
				return fmt.Errorf("messages[%d]: a user message requires non-empty content", i)
			}
		case "assistant":
			if strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0 {
				return fmt.Errorf("messages[%d]: an assistant message requires content or toolCalls", i)
			}
			for j, tc := range m.ToolCalls {
				if tc.ID == "" || tc.Name == "" {
					return fmt.Errorf("messages[%d].toolCalls[%d]: id and name are required", i, j)
				}
			}
		case "tool":
			if m.ToolCallID == "" {
				return fmt.Errorf("messages[%d]: a tool message requires toolCallId", i)
			}
			if m.Content == "" {
				return fmt.Errorf("messages[%d]: a tool message requires content", i)
			}
		default:
			return fmt.Errorf("messages[%d]: role must be one of: user, assistant, tool", i)
		}
	}
	return nil
}

// TurnWithTools runs one tool-capable fallback turn: the conversation so
// far (user text, prior assistant tool requests, executed tool results)
// against the fallback chat model with the full realtime tool catalog
// bound. It returns the model's answer verbatim — final text, or the
// tool_calls it wants executed — and never executes anything itself.
// extraSystem carries the same server-composed directive block the realtime
// path appends (base knowledge + guides, M15); pass "" for none.
func (c *FallbackClient) TurnWithTools(ctx context.Context, personaID string, messages []ChatMessage, extraSystem string) (*TurnResult, error) {
	if err := ValidateChatMessages(messages); err != nil {
		return nil, fmt.Errorf("realtime: invalid fallback messages: %w", err)
	}

	body, err := buildToolTurnRequest(personaID, messages, extraSystem)
	if err != nil {
		return nil, err
	}
	respBody, err := c.doJSON(ctx, c.chatURL, "application/json", body)
	if err != nil {
		return nil, err
	}
	return parseToolTurnResponse(respBody)
}

// buildToolTurnRequest assembles the chat-completions payload: the
// resolved persona's instructions as the system prompt, the caller's
// messages converted to OpenAI wire shape, and the full tool catalog.
func buildToolTurnRequest(personaID string, messages []ChatMessage, extraSystem string) ([]byte, error) {
	persona := ResolvePersona(personaID)

	wire := make([]map[string]any, 0, len(messages)+1)
	wire = append(wire, map[string]any{"role": "system", "content": persona.Instructions + extraSystem})
	for _, m := range messages {
		switch m.Role {
		case "tool":
			wire = append(wire, map[string]any{
				"role":         "tool",
				"tool_call_id": m.ToolCallID,
				"content":      m.Content,
			})
		case "assistant":
			msg := map[string]any{"role": "assistant", "content": m.Content}
			if len(m.ToolCalls) > 0 {
				calls := make([]map[string]any, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					calls = append(calls, map[string]any{
						"id":   tc.ID,
						"type": "function",
						"function": map[string]any{
							"name":      tc.Name,
							"arguments": tc.Arguments,
						},
					})
				}
				msg["tool_calls"] = calls
			}
			wire = append(wire, msg)
		default:
			wire = append(wire, map[string]any{"role": m.Role, "content": m.Content})
		}
	}

	body, err := json.Marshal(map[string]any{
		"model":    fallbackChatModel,
		"messages": wire,
		"tools":    chatCompletionTools,
	})
	if err != nil {
		return nil, fmt.Errorf("realtime: marshal tool turn request: %w", err)
	}
	return body, nil
}

// parseToolTurnResponse decodes the completion into a TurnResult,
// passing any tool_calls through untouched (id/name/raw arguments).
func parseToolTurnResponse(respBody []byte) (*TurnResult, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("realtime: decode tool turn response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("realtime: tool turn returned no choices")
	}

	msg := out.Choices[0].Message
	res := &TurnResult{Text: msg.Content}
	for _, tc := range msg.ToolCalls {
		if tc.Type != "" && tc.Type != "function" {
			continue // only function calls exist today; skip anything else defensively
		}
		res.ToolCalls = append(res.ToolCalls, ChatToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return res, nil
}
