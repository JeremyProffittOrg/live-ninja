package tools

// Round-trip tests of the memoryAdapter over the REAL memory core
// (internal/memory.Service) backed by the in-memory DynamoDB fake and a
// deterministic embedder — proving the tool seam and the core agree on
// write→search→get→plan→forget semantics without Bedrock or AWS.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/memory"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// wordEmbedder embeds deterministically: axis 0 fires on "sarah", axis 1
// on everything else, so cosine ranking is fully predictable.
type wordEmbedder struct{}

func (wordEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if strings.Contains(strings.ToLower(text), "sarah") {
		return []float32{1, 0}, nil
	}
	return []float32{0, 1}, nil
}

func newAdapterUnderTest(t *testing.T) MemoryService {
	t.Helper()
	st := store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja-test")
	svc, err := memory.NewService(st, wordEmbedder{})
	require.NoError(t, err)
	return NewMemoryService(svc)
}

func TestMemoryAdapterRoundTrip(t *testing.T) {
	ctx := context.Background()
	adapter := newAdapterUnderTest(t)

	// Write two entities of different types.
	person, err := adapter.Write(ctx, "u1", MemoryWriteInput{
		Type: "person", Name: "Sarah (sister)",
		Attrs:     map[string]string{"birthday": "March 3"},
		Relations: []EntityRelation{{Type: "sibling", TargetID: "ent-owner"}},
	})
	require.NoError(t, err)
	require.NotNil(t, person)
	require.NotEmpty(t, person.EntityID)
	assert.False(t, person.IndexPending)

	place, err := adapter.Write(ctx, "u1", MemoryWriteInput{
		Type: "place", Name: "Lake house",
		Attrs: map[string]string{"city": "Boone"},
	})
	require.NoError(t, err)
	require.NotNil(t, place)

	// Search ranks the person first for a "sarah" query.
	hits, err := adapter.Search(ctx, "u1", "when is sarah's birthday", "", 5)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	assert.Equal(t, person.EntityID, hits[0].Entity.EntityID)
	assert.Greater(t, hits[0].Score, 0.9)

	// Type filter drops non-matching entities.
	placeHits, err := adapter.Search(ctx, "u1", "somewhere to stay", "place", 5)
	require.NoError(t, err)
	require.Len(t, placeHits, 1)
	assert.Equal(t, place.EntityID, placeHits[0].Entity.EntityID)

	// Get resolves by bare id; unknown ids are (nil, nil).
	got, err := adapter.Get(ctx, "u1", person.EntityID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Sarah (sister)", got.Name)
	assert.Equal(t, "March 3", got.Attrs["birthday"])
	missing, err := adapter.Get(ctx, "u1", "no-such-id")
	require.NoError(t, err)
	assert.Nil(t, missing)

	// Updating an unknown entityId is the seam's not-found (nil, nil).
	notFound, err := adapter.Write(ctx, "u1", MemoryWriteInput{
		EntityID: "no-such-id", Type: "person", Name: "ghost",
	})
	require.NoError(t, err)
	assert.Nil(t, notFound)

	// Another user cannot see u1's memories.
	otherHits, err := adapter.Search(ctx, "u2", "sarah", "", 5)
	require.NoError(t, err)
	assert.Empty(t, otherHits)

	// Forget removes entity + embedding; a second forget is not-found.
	deleted, err := adapter.Forget(ctx, "u1", person.EntityID)
	require.NoError(t, err)
	assert.True(t, deleted)
	deleted, err = adapter.Forget(ctx, "u1", person.EntityID)
	require.NoError(t, err)
	assert.False(t, deleted)

	hits, err = adapter.Search(ctx, "u1", "sarah", "person", 5)
	require.NoError(t, err)
	assert.Empty(t, hits, "forgotten entities must not surface in search")
}

func TestMemoryAdapterPlanUpsert(t *testing.T) {
	ctx := context.Background()
	adapter := newAdapterUnderTest(t)

	plan, err := adapter.UpsertPlan(ctx, "u1", "", "Spring garden", []string{"buy seeds", "prep beds"})
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, "plan", plan.Type)
	require.NotEmpty(t, plan.EntityID)

	// Update in place keeps the id; steps are replaced.
	updated, err := adapter.UpsertPlan(ctx, "u1", plan.EntityID, "Spring garden", []string{"buy seeds", "plant"})
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, plan.EntityID, updated.EntityID)

	// Unknown plan id is the seam's not-found (nil, nil), not an error.
	ghost, err := adapter.UpsertPlan(ctx, "u1", "no-such-plan", "t", []string{"s"})
	require.NoError(t, err)
	assert.Nil(t, ghost)
}
