package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// WriteEntity is the memory_write core: upsert an entity by (type, name).
// An existing entity with the same type and exact name is updated in
// place (its id is kept, attrs/relations replaced), otherwise a new
// entity id is minted. The row is an ENT# item.
func (m *Service) WriteEntity(ctx context.Context, userID string, e *store.Entity) (*store.Entity, error) {
	if userID == "" {
		return nil, errors.New("memory: userID is required")
	}
	if e == nil || e.Name == "" {
		return nil, errors.New("memory: entity name is required")
	}
	if !store.ValidEntityType(e.Type) {
		return nil, fmt.Errorf("%w: %q", store.ErrInvalidEntityType, e.Type)
	}
	if strings.Contains(e.EntityID, "#") {
		return nil, errors.New("memory: entityId must not contain '#'")
	}

	e.UserID = userID
	if e.EntityID == "" {
		// Upsert-by-name: reuse the existing id when this (type, name)
		// already exists so repeated writes refine one entity instead of
		// spawning duplicates.
		existing, err := m.Store.FindEntityByName(ctx, userID, e.Type, e.Name)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			e.EntityID = existing.EntityID
		} else {
			e.EntityID = uuid.NewString()
		}
	}
	if err := m.Store.PutEntity(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

// PlanUpsert is the plan_upsert core: a plan is an ENT#plan entity whose
// attrs carry {title, steps}. planID empty mints a new plan; a supplied
// planID updates that plan in place (ErrNotFound if it doesn't exist —
// the tool tells the model to search/omit the id instead of inventing
// one).
func (m *Service) PlanUpsert(ctx context.Context, userID, planID, title string, steps []string) (*store.Entity, error) {
	if userID == "" {
		return nil, errors.New("memory: userID is required")
	}
	if title == "" {
		return nil, errors.New("memory: plan title is required")
	}
	if len(steps) == 0 {
		return nil, errors.New("memory: plan needs at least one step")
	}

	e := &store.Entity{
		UserID:   userID,
		Type:     store.EntityTypePlan,
		EntityID: planID,
		Name:     title,
		Attrs: map[string]any{
			"title": title,
			"steps": steps,
		},
	}

	if planID != "" {
		existing, err := m.Store.GetEntity(ctx, userID, store.EntityTypePlan, planID)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			return nil, store.ErrNotFound
		}
		e.Relations = existing.Relations // plan edits keep any linked entities
	}
	return m.WriteEntity(ctx, userID, e)
}
