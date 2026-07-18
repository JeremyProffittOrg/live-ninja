package realtime

// Post-session topic extraction (M11, FR-TOP-01): a cheap, engine-agnostic
// gpt-4o-mini chat completion that reads a finished conversation's
// transcript, maps it onto the user's EXISTING topic taxonomy (returning
// stable topic ids), and proposes new topic names only when nothing fits.
// Lives on FallbackClient because it shares the same key path (the broker
// is the sole OpenAI key holder), the same chat-completions endpoint, and
// the same retry/backoff discipline (doJSON) as the fallback cascade.
//
// The model is forced into a strict JSON schema via response_format, and
// its output is then re-validated server-side anyway (sanitizeExtract):
// unknown topic ids are dropped, proposed names matching an existing topic
// (case-insensitively) collapse onto that topic's id, and the combined tag
// count is capped — the model can suggest, never corrupt the taxonomy.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// TopicOption is one existing-taxonomy entry offered to the extractor
// (id + display name; the model returns ids, names are for matching).
type TopicOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ExtractResult is the sanitized extraction outcome: ids of existing
// topics this conversation belongs to, plus proposed brand-new topic
// names (the caller — cmd/topics-extract — creates those and assigns ids).
type ExtractResult struct {
	TopicIDs  []string `json:"topicIds"`
	NewTopics []string `json:"newTopics"`
}

const (
	// extractMaxTopics caps the combined (existing + new) tag count per
	// conversation (FR-TOP-01: "1..N topic tags", kept small and useful).
	extractMaxTopics = 5
	// extractMaxNameLen caps a proposed topic name's length in runes.
	extractMaxNameLen = 48
)

// ExtractTopics runs one extraction call. transcript is the flattened
// "role: text" conversation; existing is the user's active taxonomy.
func (c *FallbackClient) ExtractTopics(ctx context.Context, transcript string, existing []TopicOption) (*ExtractResult, error) {
	if strings.TrimSpace(transcript) == "" {
		return nil, fmt.Errorf("realtime: extract-topics transcript is empty")
	}

	body, err := buildExtractTopicsRequest(transcript, existing)
	if err != nil {
		return nil, err
	}
	respBody, err := c.doJSON(ctx, chatCompletionsURL, "application/json", body)
	if err != nil {
		return nil, err
	}
	return parseExtractTopicsResponse(respBody, existing)
}

// buildExtractTopicsRequest assembles the chat-completions payload:
// gpt-4o-mini, a tagging system prompt carrying the taxonomy, and a
// strict-JSON-schema response_format so the reply is machine-parseable.
func buildExtractTopicsRequest(transcript string, existing []TopicOption) ([]byte, error) {
	taxonomy, err := json.Marshal(existing)
	if err != nil {
		return nil, fmt.Errorf("realtime: marshal taxonomy: %w", err)
	}

	system := "You tag finished voice-assistant conversations with topics.\n" +
		"The user's existing topic taxonomy (JSON array of {id,name}):\n" +
		string(taxonomy) + "\n\n" +
		"Rules:\n" +
		"- Choose 1 to " + fmt.Sprint(extractMaxTopics) + " topics that describe what the conversation was about.\n" +
		"- STRONGLY prefer existing topics: return their ids in topicIds.\n" +
		"- Only propose a name in newTopics when no existing topic fits; keep names short (1-3 words, Title Case), general enough to reuse.\n" +
		"- Never invent ids; topicIds must come from the taxonomy above.\n" +
		"- Ignore small talk, greetings and assistant boilerplate when judging topics."

	payload := map[string]any{
		"model":       fallbackChatModel,
		"temperature": 0.2,
		"max_tokens":  300,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": transcript},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "topic_extraction",
				"strict": true,
				"schema": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"topicIds", "newTopics"},
					"properties": map[string]any{
						"topicIds":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"newTopics": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("realtime: marshal extract-topics request: %w", err)
	}
	return body, nil
}

// parseExtractTopicsResponse decodes the completion and sanitizes the
// model's answer against the real taxonomy.
func parseExtractTopicsResponse(respBody []byte, existing []TopicOption) (*ExtractResult, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("realtime: decode extract-topics response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("realtime: extract-topics returned no choices")
	}

	var raw ExtractResult
	if err := json.Unmarshal([]byte(out.Choices[0].Message.Content), &raw); err != nil {
		return nil, fmt.Errorf("realtime: extract-topics content is not the expected JSON: %w", err)
	}
	return sanitizeExtract(raw, existing), nil
}

// sanitizeExtract enforces the server-side invariants regardless of what
// the model said: only real existing ids survive; proposed names that
// (case-insensitively) equal an existing topic's name fold onto that id;
// duplicates collapse; names are trimmed and length-capped; the combined
// count is capped at extractMaxTopics with existing-id matches taking
// priority over new proposals.
func sanitizeExtract(raw ExtractResult, existing []TopicOption) *ExtractResult {
	validIDs := make(map[string]bool, len(existing))
	idByName := make(map[string]string, len(existing))
	for _, t := range existing {
		validIDs[t.ID] = true
		idByName[strings.ToLower(strings.TrimSpace(t.Name))] = t.ID
	}

	res := &ExtractResult{TopicIDs: []string{}, NewTopics: []string{}}
	seenIDs := map[string]bool{}
	for _, id := range raw.TopicIDs {
		id = strings.TrimSpace(id)
		if !validIDs[id] || seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		res.TopicIDs = append(res.TopicIDs, id)
	}

	seenNames := map[string]bool{}
	for _, name := range raw.NewTopics {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if utf8.RuneCountInString(name) > extractMaxNameLen {
			name = string([]rune(name)[:extractMaxNameLen])
		}
		lower := strings.ToLower(name)
		if id, ok := idByName[lower]; ok {
			// "New" topic already exists — fold onto the stable id.
			if !seenIDs[id] {
				seenIDs[id] = true
				res.TopicIDs = append(res.TopicIDs, id)
			}
			continue
		}
		if seenNames[lower] {
			continue
		}
		seenNames[lower] = true
		res.NewTopics = append(res.NewTopics, name)
	}

	// Cap combined count: existing-id tags win, proposals fill what's left.
	if len(res.TopicIDs) > extractMaxTopics {
		res.TopicIDs = res.TopicIDs[:extractMaxTopics]
	}
	if room := extractMaxTopics - len(res.TopicIDs); len(res.NewTopics) > room {
		res.NewTopics = res.NewTopics[:room]
	}
	return res
}
