package memory

import (
	"errors"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// Service is the memory core the tool registry maps onto: Search
// (memory_search), WriteEntity (memory_write), PlanUpsert (plan_upsert),
// Forget (forget). entity_get goes straight to store.GetEntityByID.
// Facts live as ENT# items. Recalled conversation text is AgentCore
// Memory, wired beside this service. This service does not embed.
type Service struct {
	Store *store.Store
}

// NewService builds the memory core over an existing store.
func NewService(st *store.Store) (*Service, error) {
	if st == nil {
		return nil, errors.New("memory: store is required")
	}
	return &Service{Store: st}, nil
}
