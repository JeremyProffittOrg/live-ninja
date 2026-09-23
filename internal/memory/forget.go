package memory

import (
	"context"
	"errors"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// Forget deletes one ENT# item. Returns the entity that was removed, or
// store.ErrNotFound when no such entity exists.
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

	if err := m.Store.DeleteEntity(ctx, userID, ent.Type, entityID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ent, nil
		}
		return nil, err
	}
	return ent, nil
}
