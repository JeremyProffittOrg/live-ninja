package tools

// agentcore-memory seam (plan.md memory-tools-cutover). The memory tools
// keep their DynamoDB entity store during dual running (locked decision 7)
// and additionally read and write the AgentCore long-term records through
// this interface. deps.AgentMemory == nil, or Admits() false for the caller,
// leaves every tool behaving exactly as before.

import (
	"context"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/agentmemory"
)

// AgentMemoryRecord is one extracted long-term record as the tools return it.
type AgentMemoryRecord struct {
	ID        string
	Text      string
	Score     float64
	CreatedAt time.Time
}

// AgentMemoryService is what the memory tool handlers need from AgentCore.
type AgentMemoryService interface {
	// Admits is the rollout gate for the invoking user's verified role.
	Admits(role string) bool
	// Search is semantic retrieval over the user's records.
	Search(ctx context.Context, userID, query string, limit int) ([]AgentMemoryRecord, error)
	// RememberFact records an explicit "remember this" exchange.
	RememberFact(ctx context.Context, userID, sessionID, text string) error
	// ForgetMatching deletes the records whose text names the entity.
	ForgetMatching(ctx context.Context, userID, name string) (int, error)
}

// NewAgentMemoryService adapts the concrete seam. A nil *Service yields a
// nil interface (not a typed nil), so `deps.AgentMemory != nil` stays a
// truthful check.
func NewAgentMemoryService(svc *agentmemory.Service) AgentMemoryService {
	if svc == nil {
		return nil
	}
	return &agentMemoryAdapter{svc: svc}
}

type agentMemoryAdapter struct{ svc *agentmemory.Service }

func (a *agentMemoryAdapter) Admits(role string) bool { return a.svc.Admits(role) }

func (a *agentMemoryAdapter) Search(ctx context.Context, userID, query string, limit int) ([]AgentMemoryRecord, error) {
	recs, err := a.svc.Retrieve(ctx, userID, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]AgentMemoryRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, AgentMemoryRecord{ID: r.ID, Text: r.Text, Score: r.Score, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

func (a *agentMemoryAdapter) RememberFact(ctx context.Context, userID, sessionID, text string) error {
	return a.svc.RememberFact(ctx, userID, sessionID, text)
}

func (a *agentMemoryAdapter) ForgetMatching(ctx context.Context, userID, name string) (int, error) {
	return a.svc.ForgetMatching(ctx, userID, name)
}
