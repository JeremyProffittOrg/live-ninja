package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// remember_note / recall_note persist and retrieve the user's freeform
// notes as NOTE# items in the caller's own partition (shared-spec NOTE
// shape: pk=USER#<uid>, sk=NOTE#<noteId>, text/tags/createdAt). Note ids
// are millisecond-time-prefixed so sk order is chronological, making
// "most recent notes" a descending single-partition Query — never a
// Scan, and never another user's partition.

const (
	// recallPageLimit caps items fetched per Query page, and
	// recallMaxScanned bounds total items examined per recall even while
	// substring-filtering in code, so a huge note history can't turn one
	// tool call into an unbounded read.
	recallPageLimit  = 100
	recallMaxScanned = 500
)

func rememberNoteDefinition() *Definition {
	return &Definition{
		Name: "remember_note",
		Description: "Save a note for the user to recall later (facts, preferences, lists, " +
			"things to remember). Optionally tag it for easier recall.",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "text", Type: "string", Required: true, MinLen: 1, MaxLen: 2000,
				Description: "The note content to remember."},
			{Name: "tags", Type: "string_array",
				Description: "Optional short tags categorizing the note, e.g. [\"shopping\",\"gift-ideas\"]."},
		},
		Handler: handleRememberNote,
	}
}

func recallNoteDefinition() *Definition {
	return &Definition{
		Name: "recall_note",
		Description: "Recall the user's saved notes, most recent first. Optionally filter by a " +
			"search phrase and/or a tag.",
		Params: []ParamSpec{
			{Name: "query", Type: "string", MaxLen: 200,
				Description: "Case-insensitive phrase to search note text for. Omit to list recent notes."},
			{Name: "tag", Type: "string", MaxLen: 50,
				Description: "Only return notes carrying this tag."},
			{Name: "limit", Type: "integer", Min: floatPtr(1), Max: floatPtr(25),
				Description: "Maximum notes to return (default 10)."},
		},
		Handler: handleRecallNote,
	}
}

func handleRememberNote(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	text := args["text"].(string)
	tags, _ := args["tags"].([]string)
	// Normalize tags: trimmed, lowercase, deduped, bounded.
	seen := make(map[string]bool, len(tags))
	cleanTags := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || len(t) > 50 || seen[t] {
			continue
		}
		seen[t] = true
		cleanTags = append(cleanTags, t)
		if len(cleanTags) == 10 {
			break
		}
	}

	now := deps.Now().UTC()
	noteID := fmt.Sprintf("%013d-%s", now.UnixMilli(), randHex(3))
	err := deps.Store.ConditionalPut(ctx, "USER#"+inv.UserID, "NOTE#"+noteID, map[string]any{
		"noteId":    noteID,
		"text":      text,
		"tags":      cleanTags,
		"createdAt": now.Format(time.RFC3339),
	}, 0)
	if err != nil {
		deps.Log.Error("tools: remember_note put failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "failed to save the note")
	}

	return map[string]any{
		"status": "saved",
		"noteId": noteID,
		"tags":   cleanTags,
	}, nil
}

// noteItem is the unmarshal target for NOTE# items during recall.
type noteItem struct {
	NoteID    string   `dynamodbav:"noteId"`
	SK        string   `dynamodbav:"sk"`
	Text      string   `dynamodbav:"text"`
	Tags      []string `dynamodbav:"tags"`
	CreatedAt string   `dynamodbav:"createdAt"`
}

func handleRecallNote(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.DDB == nil || deps.TableName == "" {
		return nil, toolErrf(CodeNotConfigured, "note recall is not configured")
	}

	query, _ := args["query"].(string)
	query = strings.ToLower(strings.TrimSpace(query))
	tag, _ := args["tag"].(string)
	tag = strings.ToLower(strings.TrimSpace(tag))
	limit := 10
	if l, ok := args["limit"].(int); ok {
		limit = l
	}

	matches := make([]map[string]any, 0, limit)
	scanned := 0
	var exclusiveStart map[string]ddbtypes.AttributeValue

	for len(matches) < limit && scanned < recallMaxScanned {
		out, err := deps.DDB.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(deps.TableName),
			KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :notePrefix)"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":pk":         &ddbtypes.AttributeValueMemberS{Value: "USER#" + inv.UserID},
				":notePrefix": &ddbtypes.AttributeValueMemberS{Value: "NOTE#"},
			},
			ScanIndexForward:  aws.Bool(false), // newest first (time-prefixed noteId)
			Limit:             aws.Int32(recallPageLimit),
			ExclusiveStartKey: exclusiveStart,
		})
		if err != nil {
			deps.Log.Error("tools: recall_note query failed", "error", err.Error())
			return nil, toolErrf(CodeUpstreamError, "failed to search notes")
		}

		for _, raw := range out.Items {
			scanned++
			var n noteItem
			if err := attributevalue.UnmarshalMap(raw, &n); err != nil {
				continue
			}
			if query != "" && !strings.Contains(strings.ToLower(n.Text), query) {
				continue
			}
			if tag != "" && !containsString(n.Tags, tag) {
				continue
			}
			noteID := n.NoteID
			if noteID == "" {
				noteID = strings.TrimPrefix(n.SK, "NOTE#")
			}
			matches = append(matches, map[string]any{
				"noteId":    noteID,
				"text":      n.Text,
				"tags":      n.Tags,
				"createdAt": n.CreatedAt,
			})
			if len(matches) == limit {
				break
			}
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		exclusiveStart = out.LastEvaluatedKey
	}

	return map[string]any{
		"notes": matches,
		"count": len(matches),
	}, nil
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
