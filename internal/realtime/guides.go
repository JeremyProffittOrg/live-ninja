package realtime

// Guide Entity injection (M10, FR-MEM-07): every session mint appends the
// user's enabled guides — user-managed standing directives stored as
// GUIDE#<guideId> items in the user's own partition (title, text, enabled,
// priority, version, updatedAt) — to the persona's system instructions.
// The broker Lambda already holds table access, so it Queries the GUIDE#
// prefix directly at mint time (single-partition Query, never a Scan) and
// the resulting suffix is bound server-side into the config-bound ephemeral
// token: clients never see or supply instruction text, so guide injection
// keeps the anti-injection property of persona resolution.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	// maxInjectedGuides caps how many guides are appended per session.
	maxInjectedGuides = 20
	// maxGuideInstructionChars caps the total appended instruction text so
	// a pathological guide set can't blow up every session's token count.
	maxGuideInstructionChars = 6000
)

// Guide is one GUIDE#<guideId> item (locked M10 shape).
type Guide struct {
	SK        string `dynamodbav:"sk"`
	GuideID   string `dynamodbav:"guideId"`
	Title     string `dynamodbav:"title"`
	Text      string `dynamodbav:"text"`
	Enabled   bool   `dynamodbav:"enabled"`
	Priority  int    `dynamodbav:"priority"`
	Version   int    `dynamodbav:"version"`
	UpdatedAt string `dynamodbav:"updatedAt"`
}

// GuideQuerier is the single DynamoDB operation guide loading needs; a
// *dynamodb.Client satisfies it, tests inject a fake.
type GuideQuerier interface {
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// LoadEnabledGuides queries the caller's GUIDE# prefix (paginated,
// single-partition — never a Scan) and returns the enabled guides sorted
// by priority ascending (ties broken by guideId for determinism).
func LoadEnabledGuides(ctx context.Context, ddb GuideQuerier, table, userID string) ([]Guide, error) {
	var guides []Guide
	var start map[string]types.AttributeValue
	for {
		out, err := ddb.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(table),
			KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :guidePrefix)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":          &types.AttributeValueMemberS{Value: "USER#" + userID},
				":guidePrefix": &types.AttributeValueMemberS{Value: "GUIDE#"},
			},
			ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, fmt.Errorf("realtime: query guides: %w", err)
		}
		for _, raw := range out.Items {
			var g Guide
			if err := attributevalue.UnmarshalMap(raw, &g); err != nil {
				continue // one malformed item must not break every mint
			}
			if !g.Enabled || strings.TrimSpace(g.Text) == "" {
				continue
			}
			if g.GuideID == "" {
				g.GuideID = strings.TrimPrefix(g.SK, "GUIDE#")
			}
			guides = append(guides, g)
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		start = out.LastEvaluatedKey
	}

	sort.SliceStable(guides, func(i, j int) bool {
		if guides[i].Priority != guides[j].Priority {
			return guides[i].Priority < guides[j].Priority
		}
		return guides[i].GuideID < guides[j].GuideID
	})
	return guides, nil
}

// GuideInstructions renders enabled guides (already priority-sorted) as
// the system-instruction suffix appended to the persona at mint time.
// Returns "" when there is nothing to inject.
func GuideInstructions(guides []Guide) string {
	if len(guides) == 0 {
		return ""
	}
	var b strings.Builder
	header := "\n\nStanding user guides — follow every one of these directives throughout this session, in priority order:"
	count, chars := 0, 0
	for _, g := range guides {
		if count == maxInjectedGuides {
			break
		}
		text := strings.TrimSpace(g.Text)
		line := fmt.Sprintf("\n%d. ", count+1)
		if title := strings.TrimSpace(g.Title); title != "" {
			line += title + ": "
		}
		line += text
		if chars+len(line) > maxGuideInstructionChars {
			break
		}
		if count == 0 {
			b.WriteString(header)
		}
		b.WriteString(line)
		chars += len(line)
		count++
	}
	if count == 0 {
		return ""
	}
	return b.String()
}
