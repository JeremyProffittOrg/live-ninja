package webapp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func TestOpenAIBudgetWarningUnderTwentyDollars(t *testing.T) {
	status := budgetStatusFromSummary(100, &store.ConversationCostSummary{
		OpenAIUSD:    80.01,
		OpenAICosted: 12,
	})
	assert.True(t, status.Warning)
	assert.InDelta(t, 19.99, status.RemainingUSD, 1e-9)
	assert.Equal(t,
		"OpenAI per-user allowance warning: estimated $19.99 remaining this month.",
		status.warningText())

	// "under $20" is strict: exactly $20 remaining is not warned.
	status = budgetStatusFromSummary(100, &store.ConversationCostSummary{OpenAIUSD: 80})
	assert.False(t, status.Warning)
	assert.Empty(t, status.warningText())
}

func TestOpenAIBudgetAllowanceIsIsolatedPerUser(t *testing.T) {
	ctx := context.Background()
	st := store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja-test")
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	ts := now.Format(time.RFC3339)
	require.NoError(t, st.CreateConversation(ctx, "u1", &store.Conversation{
		SessionID: "one", TS: ts, Engine: "gpt-realtime", CostUSD: 85,
	}))
	require.NoError(t, st.CreateConversation(ctx, "u2", &store.Conversation{
		SessionID: "two", TS: ts, Engine: "gpt-realtime", CostUSD: 5,
	}))
	deps := &Deps{Store: st, Cfg: config.App{OpenAIMonthlyBudgetUSD: 100}}

	first, err := currentOpenAIBudgetStatus(ctx, deps, "u1", now)
	require.NoError(t, err)
	second, err := currentOpenAIBudgetStatus(ctx, deps, "u2", now)
	require.NoError(t, err)
	assert.Equal(t, 15.0, first.RemainingUSD)
	assert.True(t, first.Warning)
	assert.Equal(t, 95.0, second.RemainingUSD)
	assert.False(t, second.Warning)
}

func TestAppendSessionWarningPreservesQuotaSignal(t *testing.T) {
	assert.Equal(t, "OpenAI budget warning",
		appendSessionWarning("", "OpenAI budget warning"))
	assert.Equal(t, "daily_minutes=83%; OpenAI budget warning",
		appendSessionWarning("daily_minutes=83%", "OpenAI budget warning"))
	assert.Equal(t, "daily_minutes=83%",
		appendSessionWarning("daily_minutes=83%", ""))
}
