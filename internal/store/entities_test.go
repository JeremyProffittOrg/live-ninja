package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVectorEncodeDecodeRoundTrip(t *testing.T) {
	vec := []float32{0.25, -1.5, 3.14159, 0, 1e-7}
	decoded, err := DecodeVector(EncodeVector(vec))
	require.NoError(t, err)
	assert.Equal(t, vec, decoded)

	_, err = DecodeVector("!!!not-base64!!!")
	assert.Error(t, err)

	// 3 bytes is not a whole number of float32s.
	_, err = DecodeVector("AAAA") // decodes to 3 bytes
	assert.Error(t, err)
}

func TestEntityCRUD(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	e := &Entity{
		UserID:   "u1",
		Type:     EntityTypePerson,
		EntityID: "e-alice",
		Name:     "Alice",
		Attrs:    map[string]any{"role": "sister"},
		Relations: []Relation{
			{Type: "lives_in", TargetID: "e-austin"},
		},
	}
	require.NoError(t, st.PutEntity(ctx, e))
	assert.NotEmpty(t, e.UpdatedAt)

	got, err := st.GetEntity(ctx, "u1", EntityTypePerson, "e-alice")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Alice", got.Name)
	assert.Equal(t, "sister", got.Attrs["role"])
	require.Len(t, got.Relations, 1)
	assert.Equal(t, "e-austin", got.Relations[0].TargetID)
	assert.False(t, got.Embedded)

	// GetEntityByID probes type prefixes without knowing the type.
	byID, err := st.GetEntityByID(ctx, "u1", "e-alice")
	require.NoError(t, err)
	require.NotNil(t, byID)
	assert.Equal(t, EntityTypePerson, byID.Type)

	byID, err = st.GetEntityByID(ctx, "u1", "e-nope")
	require.NoError(t, err)
	assert.Nil(t, byID)

	// Invalid type is rejected everywhere.
	bad := &Entity{UserID: "u1", Type: "alien", EntityID: "x", Name: "X"}
	require.ErrorIs(t, st.PutEntity(ctx, bad), ErrInvalidEntityType)
	_, err = st.GetEntity(ctx, "u1", "alien", "x")
	require.ErrorIs(t, err, ErrInvalidEntityType)
	_, err = st.ListEntities(ctx, "u1", "alien")
	require.ErrorIs(t, err, ErrInvalidEntityType)

	// Delete → gone; second delete reports ErrNotFound.
	require.NoError(t, st.DeleteEntity(ctx, "u1", EntityTypePerson, "e-alice"))
	gone, err := st.GetEntity(ctx, "u1", EntityTypePerson, "e-alice")
	require.NoError(t, err)
	assert.Nil(t, gone)
	require.ErrorIs(t, st.DeleteEntity(ctx, "u1", EntityTypePerson, "e-alice"), ErrNotFound)
}

func TestListEntitiesTypeFilterAndIsolation(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	seed := []*Entity{
		{UserID: "u1", Type: EntityTypePerson, EntityID: "p1", Name: "Alice"},
		{UserID: "u1", Type: EntityTypePerson, EntityID: "p2", Name: "Bob"},
		{UserID: "u1", Type: EntityTypePlace, EntityID: "pl1", Name: "Austin"},
		{UserID: "u2", Type: EntityTypePerson, EntityID: "p9", Name: "Mallory"},
	}
	for _, e := range seed {
		require.NoError(t, st.PutEntity(ctx, e))
	}

	all, err := st.ListEntities(ctx, "u1", "")
	require.NoError(t, err)
	assert.Len(t, all, 3)

	people, err := st.ListEntities(ctx, "u1", EntityTypePerson)
	require.NoError(t, err)
	require.Len(t, people, 2)
	for _, p := range people {
		assert.Equal(t, EntityTypePerson, p.Type)
		assert.Equal(t, "u1", p.UserID)
	}

	found, err := st.FindEntityByName(ctx, "u1", EntityTypePerson, "Bob")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, "p2", found.EntityID)

	missing, err := st.FindEntityByName(ctx, "u1", EntityTypePerson, "Mallory")
	require.NoError(t, err)
	assert.Nil(t, missing) // u2's entity never leaks into u1's partition
}

func TestMarkEntityEmbedded(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	e := &Entity{UserID: "u1", Type: EntityTypeInfo, EntityID: "i1", Name: "Fact"}
	require.NoError(t, st.PutEntity(ctx, e))

	require.NoError(t, st.MarkEntityEmbedded(ctx, "u1", EntityTypeInfo, "i1"))
	got, err := st.GetEntity(ctx, "u1", EntityTypeInfo, "i1")
	require.NoError(t, err)
	assert.True(t, got.Embedded)

	// Absent entity: conditional fails as ErrNotFound, no ghost item.
	require.ErrorIs(t, st.MarkEntityEmbedded(ctx, "u1", EntityTypeInfo, "i-gone"), ErrNotFound)
	ghost, err := st.GetEntity(ctx, "u1", EntityTypeInfo, "i-gone")
	require.NoError(t, err)
	assert.Nil(t, ghost)
}

func TestEmbeddingCRUD(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	emb := &Embedding{
		UserID:     "u1",
		EntityID:   "e1",
		EntityType: EntityTypePerson,
		Vector:     []float32{1, 0, 0.5},
		Model:      "test-model",
	}
	require.NoError(t, st.PutEmbedding(ctx, emb))
	assert.Equal(t, 3, emb.Dim)

	require.NoError(t, st.PutEmbedding(ctx, &Embedding{
		UserID: "u1", EntityID: "e2", EntityType: EntityTypePlace,
		Vector: []float32{0, 1, 0}, Model: "test-model",
	}))

	list, err := st.ListEmbeddings(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, list, 2)
	byID := map[string]Embedding{}
	for _, e := range list {
		byID[e.EntityID] = e
	}
	assert.Equal(t, []float32{1, 0, 0.5}, byID["e1"].Vector)
	assert.Equal(t, EntityTypePerson, byID["e1"].EntityType)
	assert.Equal(t, "test-model", byID["e1"].Model)

	// Deleting an embedding is idempotent (missing item is not an error —
	// forget must succeed for never-embedded entities).
	require.NoError(t, st.DeleteEmbedding(ctx, "u1", "e1"))
	require.NoError(t, st.DeleteEmbedding(ctx, "u1", "e1"))
	list, err = st.ListEmbeddings(ctx, "u1")
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// Validation.
	require.Error(t, st.PutEmbedding(ctx, &Embedding{UserID: "u1", EntityID: "x", EntityType: EntityTypeInfo}))
	require.ErrorIs(t, st.PutEmbedding(ctx, &Embedding{
		UserID: "u1", EntityID: "x", EntityType: "alien", Vector: []float32{1},
	}), ErrInvalidEntityType)
}

func TestGuidesSeedUpsertListDelete(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// First list seeds the default "AI is an emerging technology" guide.
	guides, err := st.ListGuides(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, guides, 1)
	assert.Equal(t, SeedGuideID, guides[0].GuideID)
	assert.Equal(t, "AI is an emerging technology", guides[0].Title)
	assert.True(t, guides[0].Enabled)
	assert.Equal(t, 1, guides[0].Version)

	// Upsert a new guide with lower priority — it sorts first.
	g, err := st.UpsertGuide(ctx, &Guide{
		UserID: "u1", GuideID: "house-style", Title: "House style",
		Text: "Answer briefly.", Enabled: true, Priority: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, g.Version)

	// Editing bumps version atomically.
	g, err = st.UpsertGuide(ctx, &Guide{
		UserID: "u1", GuideID: "house-style", Title: "House style",
		Text: "Answer briefly and cite dates.", Enabled: false, Priority: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, g.Version)
	assert.False(t, g.Enabled)

	guides, err = st.ListGuides(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, guides, 2)
	assert.Equal(t, "house-style", guides[0].GuideID) // priority 10 before 100
	assert.Equal(t, SeedGuideID, guides[1].GuideID)

	// Broker read: enabled only, priority asc.
	enabled, err := st.ListEnabledGuides(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, enabled, 1)
	assert.Equal(t, SeedGuideID, enabled[0].GuideID)

	// Delete → ErrNotFound on repeat.
	require.NoError(t, st.DeleteGuide(ctx, "u1", "house-style"))
	require.ErrorIs(t, st.DeleteGuide(ctx, "u1", "house-style"), ErrNotFound)

	// Seed does not re-fire while any guide remains.
	guides, err = st.ListGuides(ctx, "u1")
	require.NoError(t, err)
	assert.Len(t, guides, 1)
}
