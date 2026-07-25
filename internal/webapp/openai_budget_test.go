package webapp

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

func TestOpenAIBudgetWarningUnderTwentyDollars(t *testing.T) {
	status := budgetStatusFromSummary(100, &store.ConversationCostSummary{
		OpenAIUSD:    80.01,
		OpenAICosted: 12,
	})
	assert.True(t, status.Warning)
	assert.InDelta(t, 19.99, status.RemainingUSD, 1e-9)
	assert.Equal(t,
		"OpenAI budget warning: estimated $19.99 remaining this month.",
		status.warningText())

	// "under $20" is strict: exactly $20 remaining is not warned.
	status = budgetStatusFromSummary(100, &store.ConversationCostSummary{OpenAIUSD: 80})
	assert.False(t, status.Warning)
	assert.Empty(t, status.warningText())
}

func TestAppendSessionWarningPreservesQuotaSignal(t *testing.T) {
	assert.Equal(t, "OpenAI budget warning",
		appendSessionWarning("", "OpenAI budget warning"))
	assert.Equal(t, "daily_minutes=83%; OpenAI budget warning",
		appendSessionWarning("daily_minutes=83%", "OpenAI budget warning"))
	assert.Equal(t, "daily_minutes=83%",
		appendSessionWarning("daily_minutes=83%", ""))
}
