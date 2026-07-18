package realtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func seedGuide(t *testing.T, fake *testutil.FakeDynamo, userID, guideID, title, text string, enabled bool, priority int) {
	t.Helper()
	av, err := attributevalue.MarshalMap(map[string]any{
		"pk":        "USER#" + userID,
		"sk":        "GUIDE#" + guideID,
		"guideId":   guideID,
		"title":     title,
		"text":      text,
		"enabled":   enabled,
		"priority":  priority,
		"version":   1,
		"updatedAt": "2026-07-17T00:00:00Z",
	})
	require.NoError(t, err)
	fake.SeedItem(av)
}

func TestLoadEnabledGuidesFiltersAndSorts(t *testing.T) {
	fake := testutil.NewFakeDynamo()
	seedGuide(t, fake, "u1", "g-b", "Second", "lower priority directive", true, 20)
	seedGuide(t, fake, "u1", "g-a", "First", "highest priority directive", true, 10)
	seedGuide(t, fake, "u1", "g-off", "Disabled", "must never appear", false, 1)
	seedGuide(t, fake, "u1", "g-empty", "Empty", "   ", true, 5)
	// Another user's guide must never leak in (single-partition Query).
	seedGuide(t, fake, "u2", "g-x", "Other user", "other user's guide", true, 1)
	// A non-guide item under the same pk must be ignored by the sk prefix.
	av, err := attributevalue.MarshalMap(map[string]any{
		"pk": "USER#u1", "sk": "NOTE#123", "text": "a note, not a guide",
	})
	require.NoError(t, err)
	fake.SeedItem(av)

	guides, err := LoadEnabledGuides(context.Background(), fake, "live-ninja-test", "u1")
	require.NoError(t, err)
	require.Len(t, guides, 2)
	assert.Equal(t, "g-a", guides[0].GuideID, "priority asc")
	assert.Equal(t, "g-b", guides[1].GuideID)
}

func TestGuideInstructionsFormat(t *testing.T) {
	suffix := GuideInstructions([]Guide{
		{GuideID: "g1", Title: "AI is an emerging technology", Priority: 10,
			Text: "Prefer sources from the last 30 days; defer to Anthropic/OpenAI official docs; cite dates."},
		{GuideID: "g2", Priority: 20, Text: "Keep answers under three sentences."},
	})

	assert.True(t, strings.HasPrefix(suffix, "\n\n"), "suffix must append cleanly to persona instructions")
	assert.Contains(t, suffix, "Standing user guides")
	assert.Contains(t, suffix, "1. AI is an emerging technology: Prefer sources from the last 30 days")
	// A guide without a title renders as a bare numbered directive.
	assert.Contains(t, suffix, "2. Keep answers under three sentences.")
	one := strings.Index(suffix, "1. ")
	two := strings.Index(suffix, "2. ")
	assert.True(t, one >= 0 && two > one, "priority order preserved in rendering")
}

func TestGuideInstructionsEmpty(t *testing.T) {
	assert.Equal(t, "", GuideInstructions(nil))
	assert.Equal(t, "", GuideInstructions([]Guide{}))
}

func TestGuideInstructionsCapsCountAndSize(t *testing.T) {
	var many []Guide
	for i := 0; i < maxInjectedGuides+5; i++ {
		many = append(many, Guide{
			GuideID:  fmt.Sprintf("g-%02d", i),
			Priority: i,
			Text:     fmt.Sprintf("directive %02d", i),
		})
	}
	suffix := GuideInstructions(many)
	assert.Contains(t, suffix, fmt.Sprintf("%d. directive %02d", maxInjectedGuides, maxInjectedGuides-1))
	assert.NotContains(t, suffix, fmt.Sprintf("directive %02d", maxInjectedGuides), "guide count capped")

	// One enormous guide must not blow past the character budget.
	huge := []Guide{{GuideID: "g-huge", Text: strings.Repeat("x", maxGuideInstructionChars+100)}}
	assert.Equal(t, "", GuideInstructions(huge), "an over-budget first guide injects nothing")
	assert.LessOrEqual(t, len(suffix), maxGuideInstructionChars+len("\n\nStanding user guides — follow every one of these directives throughout this session, in priority order:"))
}

func TestLoadEnabledGuidesPropagatesQueryError(t *testing.T) {
	// A guide-load failure must surface as an error (the broker logs it
	// and mints without guides — availability over injection).
	_, err := LoadEnabledGuides(context.Background(), failingQuerier{}, "t", "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query guides")
}

type failingQuerier struct{}

func (failingQuerier) Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	return nil, fmt.Errorf("boom")
}
