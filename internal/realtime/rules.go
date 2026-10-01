package realtime

// Rule index injection: every session mint appends a compact index of the
// user's enabled rules — RULE#<name> items in the user's own partition
// (internal/store/rules.go) — as "name: when it applies" lines. Only the
// name and description are read here (a ProjectionExpression keeps every
// 4000-rune body out of the mint path); the model reads a matching rule's
// body on demand with the rule_load tool. Like guides, the block is
// server-derived and bound into the config-bound ephemeral token, so a
// client can never supply or alter it.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

const (
	// maxIndexedRules caps how many rule lines one session carries (it
	// equals the store's per-user cap, so it only bites on bad data).
	maxIndexedRules = store.MaxRules
	// maxRuleIndexChars caps the rendered index lines so a pathological
	// rule set cannot blow up every session's token count.
	maxRuleIndexChars = 6000
)

// RulesHeader introduces the rule index in the session instructions.
const RulesHeader = "RULES — saved instructions. Each line is name: when it applies. Before you answer a request that matches a rule's description, call rule_load with that name and follow the rule. Do not load rules that do not match."

// RuleIndexEntry is one rule as the mint-time index sees it.
type RuleIndexEntry struct {
	Name        string `dynamodbav:"name"`
	Description string `dynamodbav:"description"`
}

// ruleIndexItem is the projected unmarshal target (sk is the name fallback).
type ruleIndexItem struct {
	SK          string `dynamodbav:"sk"`
	Name        string `dynamodbav:"name"`
	Description string `dynamodbav:"description"`
	Enabled     bool   `dynamodbav:"enabled"`
}

// LoadEnabledRules queries the caller's RULE# prefix (paginated,
// single-partition — never a Scan), projecting only name, description, and
// enabled, and returns the enabled rules sorted by name. When the user has
// no RULE# items at all it returns the seed rule, so a brand-new user's
// first session already carries it: the store seeds on its first
// ListRules/GetRule, and the broker must not write.
func LoadEnabledRules(ctx context.Context, ddb GuideQuerier, table, userID string) ([]RuleIndexEntry, error) {
	var entries []RuleIndexEntry
	total := 0
	var start map[string]types.AttributeValue
	for {
		out, err := ddb.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(table),
			KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :rulePrefix)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":         &types.AttributeValueMemberS{Value: "USER#" + userID},
				":rulePrefix": &types.AttributeValueMemberS{Value: "RULE#"},
			},
			ProjectionExpression:     aws.String("sk, #n, #d, #en"),
			ExpressionAttributeNames: map[string]string{"#n": "name", "#d": "description", "#en": "enabled"},
			ExclusiveStartKey:        start,
		})
		if err != nil {
			return nil, fmt.Errorf("realtime: query rules: %w", err)
		}
		for _, raw := range out.Items {
			total++
			var it ruleIndexItem
			if err := attributevalue.UnmarshalMap(raw, &it); err != nil {
				continue // one malformed item must not break every mint
			}
			if !it.Enabled {
				continue
			}
			if it.Name == "" {
				it.Name = strings.TrimPrefix(it.SK, "RULE#")
			}
			entries = append(entries, RuleIndexEntry{Name: it.Name, Description: it.Description})
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		start = out.LastEvaluatedKey
	}

	if total == 0 {
		seed := store.DefaultRule()
		return []RuleIndexEntry{{Name: seed.Name, Description: seed.Description}}, nil
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// RulesIndexBlock renders the rule index appended to the session
// instructions: a blank line, RulesHeader, then one "- name: description"
// line per rule, capped at maxIndexedRules lines and maxRuleIndexChars of
// line text. Returns "" when there is nothing to index.
//
// Every line is re-sanitized here even though the store already enforces
// the shape: an entry whose name is not a valid slug is skipped, and the
// description is collapsed to one line, so no stored value can open a new
// line in the instructions and pose as something other than an index entry.
func RulesIndexBlock(entries []RuleIndexEntry) string {
	var b strings.Builder
	count, chars := 0, 0
	for _, e := range entries {
		if count == maxIndexedRules {
			break
		}
		if !store.ValidRuleName(e.Name) {
			continue
		}
		desc := store.NormalizeRuleDescription(e.Description)
		if desc == "" {
			continue
		}
		line := "\n- " + e.Name + ": " + desc
		if chars+len(line) > maxRuleIndexChars {
			break
		}
		if count == 0 {
			b.WriteString("\n\n" + RulesHeader)
		}
		b.WriteString(line)
		chars += len(line)
		count++
	}
	return b.String()
}
