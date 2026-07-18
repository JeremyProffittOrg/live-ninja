package memory

import (
	"errors"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// Service wires the embedder to the DynamoDB entity/embedding store and
// exposes the memory core operations the tool registry maps onto:
// Search (memory_search), WriteEntity (memory_write), PlanUpsert
// (plan_upsert), Forget (forget); entity_get goes straight to
// store.GetEntityByID.
type Service struct {
	Store    *store.Store
	Embedder Embedder
}

// NewService builds the memory core over an existing store and embedder.
func NewService(st *store.Store, emb Embedder) (*Service, error) {
	if st == nil {
		return nil, errors.New("memory: store is required")
	}
	if emb == nil {
		return nil, errors.New("memory: embedder is required")
	}
	return &Service{Store: st, Embedder: emb}, nil
}
