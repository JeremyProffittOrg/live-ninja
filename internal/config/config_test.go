package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFromEnvOpenAIMonthlyBudget(t *testing.T) {
	t.Setenv("OPENAI_MONTHLY_BUDGET_USD", "100")
	assert.Equal(t, 100.0, FromEnv().OpenAIMonthlyBudgetUSD)

	for _, invalid := range []string{"", "not-a-number", "0", "-20", "NaN", "+Inf"} {
		t.Run("invalid_"+invalid, func(t *testing.T) {
			t.Setenv("OPENAI_MONTHLY_BUDGET_USD", invalid)
			assert.Zero(t, FromEnv().OpenAIMonthlyBudgetUSD)
		})
	}
}
