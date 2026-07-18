package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMemory is a canned MemoryService recording the arguments it was
// called with, so tests assert the tool→service mapping without DynamoDB
// or Bedrock.
type fakeMemory struct {
	searchHits []MemoryHit
	entities   map[string]*MemoryEntity

	err error // returned by every method when set

	lastSearchQuery string
	lastSearchType  string
	lastSearchLimit int
	lastWrite       *MemoryWriteInput
	lastPlanID      string
	lastPlanTitle   string
	lastPlanSteps   []string
	forgotten       []string
}

func (f *fakeMemory) Search(ctx context.Context, userID, query, entityType string, limit int) ([]MemoryHit, error) {
	f.lastSearchQuery, f.lastSearchType, f.lastSearchLimit = query, entityType, limit
	return f.searchHits, f.err
}

func (f *fakeMemory) Write(ctx context.Context, userID string, in MemoryWriteInput) (*MemoryEntity, error) {
	f.lastWrite = &in
	if f.err != nil {
		return nil, f.err
	}
	if in.EntityID != "" {
		if e, ok := f.entities[in.EntityID]; ok {
			return e, nil
		}
		return nil, nil
	}
	attrs := make(map[string]any, len(in.Attrs))
	for k, v := range in.Attrs {
		attrs[k] = v
	}
	return &MemoryEntity{EntityID: "ent-new", Type: in.Type, Name: in.Name,
		Attrs: attrs, Relations: in.Relations, UpdatedAt: "2026-07-17T00:00:00Z"}, nil
}

func (f *fakeMemory) Get(ctx context.Context, userID, entityID string) (*MemoryEntity, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.entities[entityID], nil
}

func (f *fakeMemory) UpsertPlan(ctx context.Context, userID, planID, title string, steps []string) (*MemoryEntity, error) {
	f.lastPlanID, f.lastPlanTitle, f.lastPlanSteps = planID, title, steps
	if f.err != nil {
		return nil, f.err
	}
	id := planID
	if id == "" {
		id = "plan-new"
	} else if _, ok := f.entities[id]; !ok {
		return nil, nil
	}
	return &MemoryEntity{EntityID: id, Type: "plan", Name: title}, nil
}

func (f *fakeMemory) Forget(ctx context.Context, userID, entityID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if _, ok := f.entities[entityID]; !ok {
		return false, nil
	}
	f.forgotten = append(f.forgotten, entityID)
	return true, nil
}

func newMemoryDeps(fake *fakeMemory) *Deps {
	deps := newTestDeps()
	deps.Memory = fake
	return deps
}

func TestMemoryToolsNotConfigured(t *testing.T) {
	r := newTestRegistry(t, newTestDeps()) // deps.Memory == nil

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"memory_search", map[string]any{"query": "sister"}},
		{"entity_get", map[string]any{"entityId": "ent-1"}},
	} {
		res := r.Invoke(context.Background(), invocation(tc.tool, tc.args))
		require.False(t, res.OK, tc.tool)
		assert.Equal(t, CodeNotConfigured, res.Error.Code, tc.tool)
		assert.Equal(t, 503, res.StatusCode(), tc.tool)
	}
}

func TestMemorySearch(t *testing.T) {
	fake := &fakeMemory{searchHits: []MemoryHit{
		{Entity: MemoryEntity{EntityID: "ent-1", Type: "person", Name: "Sarah (sister)",
			Attrs:     map[string]any{"birthday": "March 3"},
			Relations: []EntityRelation{{Type: "sibling", TargetID: "ent-0"}},
			UpdatedAt: "2026-07-01T00:00:00Z"}, Score: 0.91},
	}}
	r := newTestRegistry(t, newMemoryDeps(fake))

	res := r.Invoke(context.Background(), invocation("memory_search", map[string]any{
		"query": "sister's birthday",
		"type":  "person",
	}))
	require.True(t, res.OK, "error: %+v", res.Error)

	assert.Equal(t, "sister's birthday", fake.lastSearchQuery)
	assert.Equal(t, "person", fake.lastSearchType)
	assert.Equal(t, defaultSearchLimit, fake.lastSearchLimit, "limit defaults to 5")

	assert.Equal(t, 1, res.Output["count"])
	results := res.Output["results"].([]map[string]any)
	require.Len(t, results, 1)
	assert.Equal(t, "ent-1", results[0]["entityId"])
	assert.Equal(t, 0.91, results[0]["score"])
	assert.Equal(t, map[string]any{"birthday": "March 3"}, results[0]["attrs"])
}

func TestMemorySearchRejectsBadType(t *testing.T) {
	r := newTestRegistry(t, newMemoryDeps(&fakeMemory{}))
	res := r.Invoke(context.Background(), invocation("memory_search", map[string]any{
		"query": "x", "type": "spaceship",
	}))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
}

func TestMemorySearchUpstreamError(t *testing.T) {
	r := newTestRegistry(t, newMemoryDeps(&fakeMemory{err: errors.New("bedrock down")}))
	res := r.Invoke(context.Background(), invocation("memory_search", map[string]any{"query": "x"}))
	require.False(t, res.OK)
	assert.Equal(t, CodeUpstreamError, res.Error.Code)
}

func TestMemoryWriteParsesAttrsAndRelations(t *testing.T) {
	fake := &fakeMemory{}
	r := newTestRegistry(t, newMemoryDeps(fake))

	inv := invocation("memory_write", map[string]any{
		"type":      "person",
		"name":      "  Sarah (sister)  ",
		"attrs":     []any{"birthday=March 3", " city = Austin "},
		"relations": []any{"sibling:ent-0"},
	})
	inv.IdempotencyKey = "k1"
	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK, "error: %+v", res.Error)

	require.NotNil(t, fake.lastWrite)
	assert.Equal(t, "person", fake.lastWrite.Type)
	assert.Equal(t, "Sarah (sister)", fake.lastWrite.Name)
	assert.Equal(t, map[string]string{"birthday": "March 3", "city": "Austin"}, fake.lastWrite.Attrs)
	assert.Equal(t, []EntityRelation{{Type: "sibling", TargetID: "ent-0"}}, fake.lastWrite.Relations)

	assert.Equal(t, "saved", res.Output["status"])
	assert.Equal(t, "ent-new", res.Output["entityId"])
}

func TestMemoryWriteRejectsMalformedEncodings(t *testing.T) {
	r := newTestRegistry(t, newMemoryDeps(&fakeMemory{}))

	for name, args := range map[string]map[string]any{
		"attr without equals":    {"type": "info", "name": "n", "attrs": []any{"noequals"}},
		"attr empty value":       {"type": "info", "name": "n", "attrs": []any{"key="}},
		"relation without colon": {"type": "info", "name": "n", "relations": []any{"sibling ent-0"}},
		"relation empty target":  {"type": "info", "name": "n", "relations": []any{"sibling:"}},
	} {
		t.Run(name, func(t *testing.T) {
			inv := invocation("memory_write", args)
			inv.IdempotencyKey = "k-" + name
			res := r.Invoke(context.Background(), inv)
			require.False(t, res.OK)
			assert.Equal(t, CodeInvalidArgs, res.Error.Code)
		})
	}
}

func TestMemoryWriteUnknownEntityIDNotFound(t *testing.T) {
	r := newTestRegistry(t, newMemoryDeps(&fakeMemory{entities: map[string]*MemoryEntity{}}))
	inv := invocation("memory_write", map[string]any{
		"type": "person", "name": "n", "entityId": "ent-missing",
	})
	inv.IdempotencyKey = "k2"
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeNotFound, res.Error.Code)
}

func TestEntityGet(t *testing.T) {
	fake := &fakeMemory{entities: map[string]*MemoryEntity{
		"ent-1": {EntityID: "ent-1", Type: "project", Name: "Kitchen remodel"},
	}}
	r := newTestRegistry(t, newMemoryDeps(fake))

	res := r.Invoke(context.Background(), invocation("entity_get", map[string]any{"entityId": "ent-1"}))
	require.True(t, res.OK)
	assert.Equal(t, "Kitchen remodel", res.Output["name"])

	res = r.Invoke(context.Background(), invocation("entity_get", map[string]any{"entityId": "ent-404"}))
	require.False(t, res.OK)
	assert.Equal(t, CodeNotFound, res.Error.Code)
	assert.Equal(t, 404, res.StatusCode())
}

func TestPlanUpsert(t *testing.T) {
	fake := &fakeMemory{}
	r := newTestRegistry(t, newMemoryDeps(fake))

	inv := invocation("plan_upsert", map[string]any{
		"title": "Spring garden",
		"steps": []any{"buy seeds", "  ", "prep beds"},
	})
	inv.IdempotencyKey = "p1"
	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK, "error: %+v", res.Error)

	assert.Equal(t, "", fake.lastPlanID)
	assert.Equal(t, "Spring garden", fake.lastPlanTitle)
	assert.Equal(t, []string{"buy seeds", "prep beds"}, fake.lastPlanSteps, "blank steps dropped")
	assert.Equal(t, "plan-new", res.Output["planId"])
	assert.Equal(t, 2, res.Output["stepCount"])
}

func TestPlanUpsertRequiresSteps(t *testing.T) {
	r := newTestRegistry(t, newMemoryDeps(&fakeMemory{}))
	inv := invocation("plan_upsert", map[string]any{
		"title": "Empty", "steps": []any{"   "},
	})
	inv.IdempotencyKey = "p2"
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
}

func TestForget(t *testing.T) {
	fake := &fakeMemory{entities: map[string]*MemoryEntity{
		"ent-1": {EntityID: "ent-1", Type: "info", Name: "old fact"},
	}}
	r := newTestRegistry(t, newMemoryDeps(fake))

	inv := invocation("forget", map[string]any{"entityId": "ent-1"})
	inv.IdempotencyKey = "f1"
	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "forgotten", res.Output["status"])
	assert.Equal(t, []string{"ent-1"}, fake.forgotten)

	inv = invocation("forget", map[string]any{"entityId": "ent-404"})
	inv.IdempotencyKey = "f2"
	res = r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeNotFound, res.Error.Code)
}

func TestManifestIncludesMemoryAndResearchTools(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	byName := map[string]bool{}
	for _, m := range r.Manifest() {
		byName[m["name"].(string)] = true
	}
	for _, name := range []string{"memory_search", "memory_write", "entity_get",
		"plan_upsert", "forget", "web_research"} {
		assert.True(t, byName[name], "manifest missing %s", name)
	}
}
