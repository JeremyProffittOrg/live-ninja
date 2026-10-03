package tools

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
)

type forgetStatusMemory struct {
	knowDocs
	fail bool
}

func (f *forgetStatusMemory) Admits(string) bool { return true }
func (f *forgetStatusMemory) ForgetMatching(context.Context, string, string) (int, error) {
	if f.fail {
		return 0, errors.New("provider unavailable")
	}
	return 2, nil
}

func TestForgetReportsPartialCleanupHonestly(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "best_effort", true: "failed"}[fail], func(t *testing.T) {
			fake := &fakeMemory{entities: map[string]*MemoryEntity{"ent-1": {EntityID: "ent-1", Type: "info", Name: "old fact"}}}
			deps := newMemoryDeps(fake)
			deps.AgentMemory = &forgetStatusMemory{fail: fail}
			inv := invocation("forget", map[string]any{"entityId": "ent-1"})
			inv.IdempotencyKey = "forget-partial-001"
			res := newTestRegistry(t, deps).Invoke(context.Background(), inv)
			require.True(t, res.OK)
			require.Equal(t, "entity_removed", res.Output["status"])
			require.Equal(t, map[bool]string{false: "best_effort", true: "failed"}[fail], res.Output["learnedCleanup"])
			require.Contains(t, res.Output["warning"], "not complete topic erasure")
			require.Equal(t, []string{"ent-1"}, fake.forgotten)
		})
	}
}
