package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// fakeEmbedder maps exact texts to fixed vectors; unmapped texts get
// defaultVec. err (when set) fails every call — the embed-failure path.
type fakeEmbedder struct {
	vecs       map[string][]float32
	defaultVec []float32
	err        error
	calls      []string
}

func (f *fakeEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	f.calls = append(f.calls, text)
	if f.err != nil {
		return nil, f.err
	}
	if v, ok := f.vecs[text]; ok {
		return v, nil
	}
	if f.defaultVec != nil {
		return f.defaultVec, nil
	}
	return []float32{1, 0, 0}, nil
}

func newTestService(t *testing.T) (*Service, *store.Store, *fakeEmbedder, *testutil.FakeDynamo) {
	t.Helper()
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja-test")
	emb := &fakeEmbedder{vecs: map[string][]float32{}}
	svc, err := NewService(st, emb)
	require.NoError(t, err)
	return svc, st, emb, fake
}

func TestCosine(t *testing.T) {
	a := []float32{1, 0, 0}
	assert.InDelta(t, 1.0, Cosine(a, []float32{2, 0, 0}), 1e-9)   // parallel
	assert.InDelta(t, 0.0, Cosine(a, []float32{0, 3, 0}), 1e-9)   // orthogonal
	assert.InDelta(t, -1.0, Cosine(a, []float32{-1, 0, 0}), 1e-9) // opposite
	assert.Equal(t, 0.0, Cosine(a, []float32{1, 0}))              // dim mismatch
	assert.Equal(t, 0.0, Cosine(a, []float32{0, 0, 0}))           // zero vector
	assert.Equal(t, 0.0, Cosine(nil, nil))                        // empty
}

func TestWriteEntityEmbedsAndUpsertsByName(t *testing.T) {
	ctx := context.Background()
	svc, st, emb, _ := newTestService(t)
	emb.defaultVec = []float32{0.5, 0.5, 0}

	e, err := svc.WriteEntity(ctx, "u1", &store.Entity{
		Type:  store.EntityTypePerson,
		Name:  "Alice",
		Attrs: map[string]any{"role": "sister"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, e.EntityID)
	assert.True(t, e.Embedded)

	// ENT persisted with embedded=true; EMB persisted with type + model.
	got, err := st.GetEntity(ctx, "u1", store.EntityTypePerson, e.EntityID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.Embedded)

	embs, err := st.ListEmbeddings(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, embs, 1)
	assert.Equal(t, e.EntityID, embs[0].EntityID)
	assert.Equal(t, store.EntityTypePerson, embs[0].EntityType)
	assert.Equal(t, EmbedModelID, embs[0].Model)
	assert.Equal(t, []float32{0.5, 0.5, 0}, embs[0].Vector)

	// Same (type, name) again → same entityId, attrs replaced, still 1 EMB.
	e2, err := svc.WriteEntity(ctx, "u1", &store.Entity{
		Type:  store.EntityTypePerson,
		Name:  "Alice",
		Attrs: map[string]any{"role": "sister", "city": "Austin"},
	})
	require.NoError(t, err)
	assert.Equal(t, e.EntityID, e2.EntityID)

	people, err := st.ListEntities(ctx, "u1", store.EntityTypePerson)
	require.NoError(t, err)
	require.Len(t, people, 1)
	assert.Equal(t, "Austin", people[0].Attrs["city"])

	embs, err = st.ListEmbeddings(ctx, "u1")
	require.NoError(t, err)
	assert.Len(t, embs, 1)

	// Validation.
	_, err = svc.WriteEntity(ctx, "u1", &store.Entity{Type: "alien", Name: "X"})
	require.ErrorIs(t, err, store.ErrInvalidEntityType)
	_, err = svc.WriteEntity(ctx, "u1", &store.Entity{Type: store.EntityTypeInfo, Name: "X", EntityID: "bad#id"})
	require.Error(t, err)
}

func TestWriteEntityEmbedFailureIsPartial(t *testing.T) {
	ctx := context.Background()
	svc, st, emb, _ := newTestService(t)
	emb.err = errors.New("bedrock throttled")

	e, err := svc.WriteEntity(ctx, "u1", &store.Entity{
		Type: store.EntityTypeInfo, Name: "Fragile fact",
	})
	require.ErrorIs(t, err, ErrEmbedFailed)
	require.NotNil(t, e) // partial success: entity IS saved

	got, err := st.GetEntity(ctx, "u1", store.EntityTypeInfo, e.EntityID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, got.Embedded) // the gap is recorded, not hidden

	embs, err := st.ListEmbeddings(ctx, "u1")
	require.NoError(t, err)
	assert.Empty(t, embs)

	// A retry after the embedder recovers completes the index.
	emb.err = nil
	e2, err := svc.WriteEntity(ctx, "u1", &store.Entity{
		Type: store.EntityTypeInfo, Name: "Fragile fact",
	})
	require.NoError(t, err)
	assert.Equal(t, e.EntityID, e2.EntityID)
	embs, err = st.ListEmbeddings(ctx, "u1")
	require.NoError(t, err)
	assert.Len(t, embs, 1)
}

func TestSearchRanksByCosine(t *testing.T) {
	ctx := context.Background()
	svc, st, emb, _ := newTestService(t)

	seed := []struct {
		id, typ, name string
		vec           []float32
	}{
		{"e-dog", store.EntityTypeInfo, "Rex the dog", []float32{1, 0, 0}},
		{"e-cat", store.EntityTypeInfo, "Whiskers the cat", []float32{0.9, 0.1, 0}},
		{"e-tax", store.EntityTypeTask, "File taxes", []float32{0, 0, 1}},
	}
	for _, s := range seed {
		require.NoError(t, st.PutEntity(ctx, &store.Entity{
			UserID: "u1", Type: s.typ, EntityID: s.id, Name: s.name,
		}))
		require.NoError(t, st.PutEmbedding(ctx, &store.Embedding{
			UserID: "u1", EntityID: s.id, EntityType: s.typ,
			Vector: s.vec, Model: EmbedModelID,
		}))
	}
	emb.vecs["pets"] = []float32{1, 0.05, 0}

	results, err := svc.Search(ctx, "u1", "pets", 2)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "e-dog", results[0].Entity.EntityID)
	assert.Equal(t, "e-cat", results[1].Entity.EntityID)
	assert.Greater(t, results[0].Score, results[1].Score)
	assert.Equal(t, "Rex the dog", results[0].Entity.Name)

	// topK <= 0 falls back to DefaultTopK; all 3 fit.
	results, err = svc.Search(ctx, "u1", "pets", 0)
	require.NoError(t, err)
	assert.Len(t, results, 3)

	// Empty corpus for another user.
	results, err = svc.Search(ctx, "u2", "pets", 5)
	require.NoError(t, err)
	assert.Empty(t, results)

	// A vector whose entity vanished (forget raced) is skipped, not fatal.
	require.NoError(t, st.DeleteEntity(ctx, "u1", store.EntityTypeInfo, "e-dog"))
	results, err = svc.Search(ctx, "u1", "pets", 2)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "e-cat", results[0].Entity.EntityID)
}

func TestPlanUpsert(t *testing.T) {
	ctx := context.Background()
	svc, st, _, _ := newTestService(t)

	p, err := svc.PlanUpsert(ctx, "u1", "", "Ship M10", []string{"store", "memory core", "tools"})
	require.NoError(t, err)
	require.NotEmpty(t, p.EntityID)
	assert.Equal(t, store.EntityTypePlan, p.Type)
	assert.Equal(t, "Ship M10", p.Attrs["title"])

	// Update in place by id.
	p2, err := svc.PlanUpsert(ctx, "u1", p.EntityID, "Ship M10+M11", []string{"store", "memory core", "tools", "history"})
	require.NoError(t, err)
	assert.Equal(t, p.EntityID, p2.EntityID)

	plans, err := st.ListEntities(ctx, "u1", store.EntityTypePlan)
	require.NoError(t, err)
	require.Len(t, plans, 1)
	assert.Equal(t, "Ship M10+M11", plans[0].Name)
	steps, ok := plans[0].Attrs["steps"].([]any)
	require.True(t, ok)
	assert.Len(t, steps, 4)

	// Unknown planId must not silently create a divergent plan.
	_, err = svc.PlanUpsert(ctx, "u1", "no-such-plan", "Ghost", []string{"x"})
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = svc.PlanUpsert(ctx, "u1", "", "No steps", nil)
	require.Error(t, err)
}

func TestForgetPropagatesToBothStores(t *testing.T) {
	ctx := context.Background()
	svc, st, emb, _ := newTestService(t)
	emb.defaultVec = []float32{0, 1, 0}

	e, err := svc.WriteEntity(ctx, "u1", &store.Entity{
		Type: store.EntityTypePerson, Name: "Bob",
	})
	require.NoError(t, err)

	forgotten, err := svc.Forget(ctx, "u1", e.EntityID)
	require.NoError(t, err)
	assert.Equal(t, "Bob", forgotten.Name)

	got, err := st.GetEntityByID(ctx, "u1", e.EntityID)
	require.NoError(t, err)
	assert.Nil(t, got)
	embs, err := st.ListEmbeddings(ctx, "u1")
	require.NoError(t, err)
	assert.Empty(t, embs)

	// Unknown entity → ErrNotFound.
	_, err = svc.Forget(ctx, "u1", "never-existed")
	require.ErrorIs(t, err, store.ErrNotFound)

	// A never-embedded entity (embed failed earlier) still forgets cleanly.
	require.NoError(t, st.PutEntity(ctx, &store.Entity{
		UserID: "u1", Type: store.EntityTypeInfo, EntityID: "raw", Name: "Unindexed",
	}))
	forgotten, err = svc.Forget(ctx, "u1", "raw")
	require.NoError(t, err)
	assert.Equal(t, "Unindexed", forgotten.Name)
}

func TestEmbedTextDeterministic(t *testing.T) {
	e := &store.Entity{
		Type: store.EntityTypePerson,
		Name: "Alice",
		Attrs: map[string]any{
			"role":    "sister",
			"city":    "Austin",
			"hobbies": []any{"climbing", "chess"},
			"contact": map[string]any{"email": "a@example.com", "phone": "555"},
		},
		Relations: []store.Relation{{Type: "works_at", TargetID: "e-acme"}},
	}
	text := EmbedText(e)
	assert.Equal(t, text, EmbedText(e)) // stable across calls
	assert.Contains(t, text, "person: Alice")
	assert.Contains(t, text, "city: Austin")
	assert.Contains(t, text, "hobbies: climbing; chess")
	assert.Contains(t, text, "contact: email=a@example.com, phone=555")
	assert.Contains(t, text, "works_at -> e-acme")
	// Sorted attr order: city < contact < hobbies < role.
	assert.Less(t, indexOf(text, "city:"), indexOf(text, "contact:"))
	assert.Less(t, indexOf(text, "contact:"), indexOf(text, "hobbies:"))
	assert.Less(t, indexOf(text, "hobbies:"), indexOf(text, "role:"))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
