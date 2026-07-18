package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// ErrEmbedFailed wraps an embedding/EMB-write failure after the ENT item
// itself was persisted: the entity IS saved (embedded=false records the
// gap honestly), it just isn't semantically searchable yet. Callers
// (the memory_write tool) surface this as "saved, indexing pending" —
// a retry re-embeds because WriteEntity always re-writes the vector.
var ErrEmbedFailed = errors.New("memory: entity saved but embedding failed")

// WriteEntity is the memory_write core: upsert an entity by (type, name)
// — an existing entity with the same type and exact name is updated in
// place (its id is kept, attrs/relations replaced), otherwise a new
// entity id is minted. The entity is then embedded and its EMB item
// written, after which embedded=true is flipped on the ENT item.
//
// On embedding failure the persisted entity is returned together with an
// error wrapping ErrEmbedFailed (partial success — see above).
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
	e.Embedded = false

	if err := m.Store.PutEntity(ctx, e); err != nil {
		return nil, err
	}

	if err := m.embedEntity(ctx, e); err != nil {
		return e, fmt.Errorf("%w: %v", ErrEmbedFailed, err)
	}
	e.Embedded = true
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

// embedEntity builds the entity's embed text, gets its vector, writes
// the EMB item, and marks the ENT item embedded.
func (m *Service) embedEntity(ctx context.Context, e *store.Entity) error {
	vec, err := m.Embedder.Embed(ctx, EmbedText(e))
	if err != nil {
		return err
	}
	if err := m.Store.PutEmbedding(ctx, &store.Embedding{
		UserID:     e.UserID,
		EntityID:   e.EntityID,
		EntityType: e.Type,
		Vector:     vec,
		Model:      EmbedModelID,
	}); err != nil {
		return err
	}
	if err := m.Store.MarkEntityEmbedded(ctx, e.UserID, e.Type, e.EntityID); err != nil {
		// ErrNotFound here means the entity was forgotten between the Put
		// and now; remove the freshly-written orphan vector and report.
		if errors.Is(err, store.ErrNotFound) {
			_ = m.Store.DeleteEmbedding(ctx, e.UserID, e.EntityID)
		}
		return err
	}
	return nil
}

// EmbedText flattens an entity into the deterministic text that gets
// embedded: type, name, then attrs as sorted key: value lines, then
// relation edges. Deterministic ordering keeps re-embeds of an unchanged
// entity byte-identical.
func EmbedText(e *store.Entity) string {
	var b strings.Builder
	b.WriteString(e.Type)
	b.WriteString(": ")
	b.WriteString(e.Name)

	keys := make([]string, 0, len(e.Attrs))
	for k := range e.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("\n")
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(flattenAttr(e.Attrs[k]))
	}
	for _, r := range e.Relations {
		b.WriteString("\n")
		b.WriteString(r.Type)
		b.WriteString(" -> ")
		b.WriteString(r.TargetID)
	}
	return b.String()
}

// flattenAttr renders an attr value as embeddable text (lists joined,
// nested maps as k=v pairs, scalars via Sprint).
func flattenAttr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []string:
		return strings.Join(t, "; ")
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, flattenAttr(item))
		}
		return strings.Join(parts, "; ")
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+flattenAttr(t[k]))
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(v)
	}
}
