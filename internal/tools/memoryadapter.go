package tools

// memoryAdapter bridges internal/memory.Service — the real M10 memory
// core (Bedrock titan-embed embeddings + ENT#/EMB# items, Query-only) —
// onto the MemoryService seam the tool handlers consume. The web function
// wires tools.Deps.Memory = tools.NewMemoryService(memorySvc); tests keep
// injecting lightweight fakes of the seam.

import (
	"context"
	"errors"

	"github.com/JeremyProffittOrg/live-ninja/internal/memory"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// NewMemoryService adapts the concrete memory core to the tool seam.
func NewMemoryService(svc *memory.Service) MemoryService {
	return &memoryAdapter{svc: svc}
}

type memoryAdapter struct{ svc *memory.Service }

func (a *memoryAdapter) Search(ctx context.Context, userID, query, entityType string, limit int) ([]MemoryHit, error) {
	// The core ranks without a type filter; when the tool asks for one
	// type, over-fetch the core's cap and filter post-rank so the caller
	// still gets up to `limit` matching entities.
	topK := limit
	if entityType != "" {
		topK = memory.MaxTopK
	}
	results, err := a.svc.Search(ctx, userID, query, topK)
	if err != nil {
		return nil, err
	}

	hits := make([]MemoryHit, 0, limit)
	for _, r := range results {
		if entityType != "" && r.Entity.Type != entityType {
			continue
		}
		e := r.Entity
		hits = append(hits, MemoryHit{Entity: *fromStoreEntity(&e), Score: r.Score})
		if len(hits) == limit {
			break
		}
	}
	return hits, nil
}

func (a *memoryAdapter) Write(ctx context.Context, userID string, in MemoryWriteInput) (*MemoryEntity, error) {
	// The core upserts blindly at a supplied id; the tool contract is that
	// updating an unknown id is not_found (the model must not invent ids),
	// so existence is checked first (one GetItem).
	if in.EntityID != "" {
		existing, err := a.svc.Store.GetEntityByID(ctx, userID, in.EntityID)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			return nil, nil
		}
	}

	attrs := make(map[string]any, len(in.Attrs))
	for k, v := range in.Attrs {
		attrs[k] = v
	}
	rels := make([]store.Relation, 0, len(in.Relations))
	for _, r := range in.Relations {
		rels = append(rels, store.Relation{Type: r.Type, TargetID: r.TargetID})
	}

	ent, err := a.svc.WriteEntity(ctx, userID, &store.Entity{
		EntityID:  in.EntityID,
		Type:      in.Type,
		Name:      in.Name,
		Attrs:     attrs,
		Relations: rels,
	})
	return adaptWriteResult(ent, err)
}

func (a *memoryAdapter) Get(ctx context.Context, userID, entityID string) (*MemoryEntity, error) {
	ent, err := a.svc.Store.GetEntityByID(ctx, userID, entityID)
	if err != nil {
		return nil, err
	}
	if ent == nil {
		return nil, nil
	}
	return fromStoreEntity(ent), nil
}

func (a *memoryAdapter) UpsertPlan(ctx context.Context, userID, planID, title string, steps []string) (*MemoryEntity, error) {
	ent, err := a.svc.PlanUpsert(ctx, userID, planID, title, steps)
	return adaptWriteResult(ent, err)
}

func (a *memoryAdapter) Forget(ctx context.Context, userID, entityID string) (bool, error) {
	if _, err := a.svc.Forget(ctx, userID, entityID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// adaptWriteResult maps the core's write outcomes onto the seam contract:
// ErrEmbedFailed with a persisted entity is a partial success ("saved,
// indexing pending"), store.ErrNotFound is the seam's (nil, nil).
func adaptWriteResult(ent *store.Entity, err error) (*MemoryEntity, error) {
	if err != nil {
		if errors.Is(err, memory.ErrEmbedFailed) && ent != nil {
			out := fromStoreEntity(ent)
			out.IndexPending = true
			return out, nil
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return fromStoreEntity(ent), nil
}

func fromStoreEntity(e *store.Entity) *MemoryEntity {
	rels := make([]EntityRelation, 0, len(e.Relations))
	for _, r := range e.Relations {
		rels = append(rels, EntityRelation{Type: r.Type, TargetID: r.TargetID})
	}
	return &MemoryEntity{
		EntityID:  e.EntityID,
		Type:      e.Type,
		Name:      e.Name,
		Attrs:     e.Attrs,
		Relations: rels,
		UpdatedAt: e.UpdatedAt,
	}
}
