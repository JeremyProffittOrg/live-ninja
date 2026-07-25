package webapp

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

const openAIBudgetWarningThresholdUSD = 20.0

// openAIBudgetStatus is option (a) from plan.md M29: compare the owner-set
// monthly OpenAI budget with costUsd already persisted on CONV rows. It needs
// no new credential and deliberately describes the result as an estimate.
type openAIBudgetStatus struct {
	BudgetUSD           float64
	SpentUSD            float64
	RemainingUSD        float64
	WarningThresholdUSD float64
	Warning             bool
	Costed              int
}

func currentOpenAIBudgetStatus(
	ctx context.Context,
	deps *Deps,
	userID string,
	now time.Time,
) (*openAIBudgetStatus, error) {
	budget := deps.Cfg.OpenAIMonthlyBudgetUSD
	if budget <= 0 {
		return nil, nil
	}
	monthStart := time.Date(
		now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC,
	).Format(time.RFC3339)
	sum, err := deps.Store.SumConversationCosts(ctx, userID, monthStart, "")
	if err != nil {
		return nil, err
	}
	return budgetStatusFromSummary(budget, sum), nil
}

func budgetStatusFromSummary(
	budget float64,
	sum *store.ConversationCostSummary,
) *openAIBudgetStatus {
	remaining := budget - sum.OpenAIUSD
	// Avoid a surprising "-0.00" caused by floating-point subtraction.
	if math.Abs(remaining) < 0.000_000_1 {
		remaining = 0
	}
	return &openAIBudgetStatus{
		BudgetUSD:           budget,
		SpentUSD:            sum.OpenAIUSD,
		RemainingUSD:        remaining,
		WarningThresholdUSD: openAIBudgetWarningThresholdUSD,
		Warning:             remaining < openAIBudgetWarningThresholdUSD,
		Costed:              sum.OpenAICosted,
	}
}

func (s *openAIBudgetStatus) warningText() string {
	if s == nil || !s.Warning {
		return ""
	}
	return fmt.Sprintf(
		"OpenAI budget warning: estimated $%.2f remaining this month.",
		s.RemainingUSD,
	)
}

func appendSessionWarning(existing, warning string) string {
	if warning == "" {
		return existing
	}
	if existing == "" {
		return warning
	}
	return existing + "; " + warning
}
