package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func newTestService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st := store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja-test")
	svc, err := NewService(st)
	require.NoError(t, err)
	return svc, st
}

func TestWriteEntityUpsertAndValidation(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestService(t)

	e, err := svc.WriteEntity(ctx, "u1", &store.Entity{
		Type: store.EntityTypePerson, Name: "Sarah",
		Attrs: map[string]any{"relation": "sister"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, e.EntityID)

	got, err := st.GetEntity(ctx, "u1", store.EntityTypePerson, e.EntityID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Sarah", got.Name)

	e2, err := svc.WriteEntity(ctx, "u1", &store.Entity{
		Type: store.EntityTypePerson, Name: "Sarah",
		Attrs: map[string]any{"relation": "sister", "city": "Boone"},
	})
	require.NoError(t, err)
	assert.Equal(t, e.EntityID, e2.EntityID, "same type+name updates in place")

	_, err = svc.WriteEntity(ctx, "u1", &store.Entity{Type: "alien", Name: "X"})
	assert.ErrorIs(t, err, store.ErrInvalidEntityType)
	_, err = svc.WriteEntity(ctx, "u1", &store.Entity{Type: store.EntityTypeInfo, Name: "X", EntityID: "bad#id"})
	assert.Error(t, err)
}

func TestSearchMatchesNameAndAttrs(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)

	sarah, err := svc.WriteEntity(ctx, "u1", &store.Entity{
		Type: store.EntityTypePerson, Name: "Sarah",
		Attrs: map[string]any{"relation": "sister"},
	})
	require.NoError(t, err)
	_, err = svc.WriteEntity(ctx, "u1", &store.Entity{
		Type: store.EntityTypePlace, Name: "Lake house",
		Attrs: map[string]any{"city": "Boone"},
	})
	require.NoError(t, err)

	results, err := svc.Search(ctx, "u1", "when is sarah's birthday", 2)
	require.NoError(t, err)
	require.NotEmpty(t, results)
	assert.Equal(t, sarah.EntityID, results[0].Entity.EntityID)
	assert.Equal(t, 1.0, results[0].Score)

	results, err = svc.Search(ctx, "u1", "boone", 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "Lake house", results[0].Entity.Name)

	results, err = svc.Search(ctx, "u2", "sarah", 5)
	require.NoError(t, err)
	assert.Empty(t, results)

	_, err = svc.Forget(ctx, "u1", sarah.EntityID)
	require.NoError(t, err)
	results, err = svc.Search(ctx, "u1", "sarah", 5)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestPlanUpsert(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)

	plan, err := svc.PlanUpsert(ctx, "u1", "", "Kitchen", []string{"measure", "order"})
	require.NoError(t, err)
	require.NotEmpty(t, plan.EntityID)
	assert.Equal(t, store.EntityTypePlan, plan.Type)

	updated, err := svc.PlanUpsert(ctx, "u1", plan.EntityID, "Kitchen", []string{"measure", "order", "install"})
	require.NoError(t, err)
	assert.Equal(t, plan.EntityID, updated.EntityID)

	_, err = svc.PlanUpsert(ctx, "u1", "missing", "Nope", []string{"x"})
	assert.ErrorIs(t, err, store.ErrNotFound)
}
