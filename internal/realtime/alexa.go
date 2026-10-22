package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const AlexaTextModel = "gpt-6-luna"

// AlexaTurn shares the server tool manifest and prompt rendering while keeping
// the Alexa-only model, token limit, standard processing and no-retry contract.
func (c *FallbackClient) AlexaTurn(ctx context.Context, persona Persona, messages []ChatMessage, directives, model string) (*TurnResult, error) {
	if err := ValidateChatMessages(messages); err != nil {
		return nil, err
	}
	if model == "" {
		model = AlexaTextModel
	}
	if model != AlexaTextModel {
		return nil, fmt.Errorf("Alexa model has not passed its configured compatibility gate")
	}
	directives += "\nALEXA: Answer concisely for speech. Keep the first answer under 600 characters when possible. Use the conversation context for follow-up references. Alexa controls its voice. Do not claim perpetual listening or completed actions without tool evidence. If a tool requires website approval, direct the user to Live Ninja; do not treat spoken yes as website approval. Never speak tokens, credentials, raw tool JSON, code dumps, or long URLs.\n"
	persona.Instructions = ComposeCustomInstructions(persona.Style)
	body, err := buildToolTurnRequestForPersona(ctx, persona, messages, directives)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	payload["model"] = model
	payload["reasoning_effort"] = "none"
	payload["service_tier"] = "default"
	payload["max_completion_tokens"] = 512
	payload["parallel_tool_calls"] = false
	payload["store"] = false
	body, err = json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	// Bytes are a conservative upper bound on text tokens. This ceiling also
	// makes the five-round acceptance allowance enforceable before a paid turn.
	if len(body) > 128<<10 {
		return nil, fmt.Errorf("Alexa request exceeds its bounded prompt budget")
	}
	raw, err := c.doJSONRetries(ctx, c.chatURL, "application/json", body, nil)
	if err != nil {
		return nil, err
	}
	return parseToolTurnResponse(raw)
}

// AlexaUsageCost is a list-price estimate, not an invoice. The request uses
// Standard processing; unknown model identities must not be priced as Luna.
func AlexaUsageCost(u TextUsage) (float64, error) {
	if u.Model != AlexaTextModel && !strings.HasPrefix(u.Model, AlexaTextModel+"-") {
		return 0, fmt.Errorf("unknown Alexa model pricing")
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedTokens < 0 || u.CachedTokens > u.InputTokens {
		return 0, fmt.Errorf("invalid Alexa usage")
	}
	return (float64(u.InputTokens-u.CachedTokens)*0.10 + float64(u.CachedTokens)*0.01 + float64(u.OutputTokens)*0.50) / 1e6, nil
}
