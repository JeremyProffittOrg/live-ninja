package memory

import (
	"context"
	"errors"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// Forget is the forget-tool core (FR-MEM-05): delete the entity AND its
// embedding so removal propagates to both the structured store and the
// semantic index in one call. Returns the entity that was removed, or
// store.ErrNotFound when no such entity exists.
//
// The EMB item is deleted first: if the process dies between the two
// deletes, the leftover is a plain ENT item the user can still see and
// forget again — never an invisible orphan vector that keeps surfacing
// "forgotten" facts in search.
func (m *Service) Forget(ctx context.Context, userID, entityID string) (*store.Entity, error) {
	if userID == "" || entityID == "" {
		return nil, errors.New("memory: userID and entityID are required")
	}

	ent, err := m.Store.GetEntityByID(ctx, userID, entityID)
	if err != nil {
		return nil, err
	}
	if ent == nil {
		return nil, store.ErrNotFound
	}

	if err := m.Store.DeleteEmbedding(ctx, userID, entityID); err != nil {
		return nil, err
	}
	if err := m.Store.DeleteEntity(ctx, userID, ent.Type, entityID); err != nil {
		// A racing forget already removed the ENT item; the outcome the
		// caller asked for (entity gone everywhere) holds, so report the
		// entity we removed rather than a spurious failure.
		if errors.Is(err, store.ErrNotFound) {
			return ent, nil
		}
		return nil, err
	}
	return ent, nil
}
