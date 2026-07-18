package realtime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func chatCompletionBody(t *testing.T, content any) []byte {
	t.Helper()
	raw, err := json.Marshal(content)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"content": string(raw)}},
		},
	})
	require.NoError(t, err)
	return body
}

func TestBuildExtractTopicsRequest(t *testing.T) {
	body, err := buildExtractTopicsRequest("user: hi\nassistant: hello\n",
		[]TopicOption{{ID: "t1", Name: "Cooking"}})
	require.NoError(t, err)

	var req map[string]any
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, "gpt-4o-mini", req["model"])

	msgs, ok := req["messages"].([]any)
	require.True(t, ok)
	require.Len(t, msgs, 2)
	system := msgs[0].(map[string]any)["content"].(string)
	assert.Contains(t, system, `"id":"t1"`)
	assert.Contains(t, system, "Cooking")

	// Strict JSON schema response contract.
	rf, ok := req["response_format"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "json_schema", rf["type"])
	js := rf["json_schema"].(map[string]any)
	assert.Equal(t, true, js["strict"])
}

func TestParseExtractTopicsResponseSanitizes(t *testing.T) {
	existing := []TopicOption{{ID: "t1", Name: "Cooking"}, {ID: "t2", Name: "Travel"}}

	body := chatCompletionBody(t, map[string]any{
		"topicIds": []string{"t1", "t1", "ghost", "t2"},
		"newTopics": []string{
			"  cooking ", // folds onto t1 (case/space-insensitive) — already tagged, no dup
			"Gardening",
			"gardening",              // dup of the proposal above
			"",                       // dropped
			strings.Repeat("x", 100), // length-capped
		},
	})

	res, err := parseExtractTopicsResponse(body, existing)
	require.NoError(t, err)
	assert.Equal(t, []string{"t1", "t2"}, res.TopicIDs, "unknown ids dropped, dups collapsed")
	require.Len(t, res.NewTopics, 2)
	assert.Equal(t, "Gardening", res.NewTopics[0])
	assert.Equal(t, extractMaxNameLen, len([]rune(res.NewTopics[1])))
}

func TestParseExtractTopicsResponseFoldsNewNameOntoExistingID(t *testing.T) {
	existing := []TopicOption{{ID: "t9", Name: "Home Automation"}}
	body := chatCompletionBody(t, map[string]any{
		"topicIds":  []string{},
		"newTopics": []string{"home automation"},
	})
	res, err := parseExtractTopicsResponse(body, existing)
	require.NoError(t, err)
	assert.Equal(t, []string{"t9"}, res.TopicIDs)
	assert.Empty(t, res.NewTopics)
}

func TestParseExtractTopicsResponseCapsCombinedCount(t *testing.T) {
	existing := []TopicOption{
		{ID: "a", Name: "A"}, {ID: "b", Name: "B"}, {ID: "c", Name: "C"},
		{ID: "d", Name: "D"}, {ID: "e", Name: "E"}, {ID: "f", Name: "F"},
	}
	body := chatCompletionBody(t, map[string]any{
		"topicIds":  []string{"a", "b", "c", "d", "e", "f"},
		"newTopics": []string{"G", "H"},
	})
	res, err := parseExtractTopicsResponse(body, existing)
	require.NoError(t, err)
	assert.Len(t, res.TopicIDs, extractMaxTopics, "existing-id tags capped")
	assert.Empty(t, res.NewTopics, "no room left for proposals")

	// Ids take priority; proposals fill remaining room only.
	body = chatCompletionBody(t, map[string]any{
		"topicIds":  []string{"a", "b", "c", "d"},
		"newTopics": []string{"G", "H"},
	})
	res, err = parseExtractTopicsResponse(body, existing)
	require.NoError(t, err)
	assert.Len(t, res.TopicIDs, 4)
	assert.Equal(t, []string{"G"}, res.NewTopics)
}

func TestParseExtractTopicsResponseErrors(t *testing.T) {
	existing := []TopicOption{}

	_, err := parseExtractTopicsResponse([]byte(`{"choices":[]}`), existing)
	require.Error(t, err)

	_, err = parseExtractTopicsResponse([]byte(`not json`), existing)
	require.Error(t, err)

	// Content that isn't the schema'd JSON (model misbehaving despite
	// strict mode) is a loud error, not a silent empty tag set.
	body, errM := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": "sorry, no"}}},
	})
	require.NoError(t, errM)
	_, err = parseExtractTopicsResponse(body, existing)
	require.Error(t, err)
}
