package webapp

// Route-level tests for the M10 memory + guide API (memory_routes.go):
// entity write/list/get/forget over a real memory.Service wired to a
// deterministic fake embedder + FakeDynamo-backed store, semantic search
// ranking, guide CRUD with the FR-MEM-09 seed, validation rejections, and
// the nil-service (embedder unavailable) degradation.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/memory"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// fakeEmbedder returns axis-aligned vectors keyed by keywords in the
// embedded text, so cosine ranking in tests is fully deterministic:
// "coffee" → x-axis, "dog" → y-axis, everything else → z-axis.
type fakeEmbedder struct{ calls int }

func (f *fakeEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	f.calls++
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "coffee"):
		return []float32{1, 0.1, 0}, nil
	case strings.Contains(lower, "dog"):
		return []float32{0.1, 1, 0}, nil
	default:
		return []float32{0, 0, 1}, nil
	}
}

// newMemoryAPIApp mounts the memory routes as user u1 over fresh fakes.
func newMemoryAPIApp(t *testing.T) (*fiber.App, *store.Store, *testutil.FakeDynamo) {
	t.Helper()
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja")
	svc, err := memory.NewService(st, &fakeEmbedder{})
	if err != nil {
		t.Fatalf("memory.NewService: %v", err)
	}
	deps := &Deps{
		Store: st,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		return c.Next()
	})
	RegisterMemoryRoutes(app, deps, svc)
	return app, st, fake
}

func TestEntityWriteListGetForget(t *testing.T) {
	app, _, fake := newMemoryAPIApp(t)

	// Create via POST /entities.
	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/entities", map[string]any{
		"type": "person", "name": "Sam the coffee friend",
		"attrs":     map[string]any{"role": "friend"},
		"relations": []map[string]string{{"type": "works_at", "targetId": "place-1"}},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%v)", resp.StatusCode, body)
	}
	id, _ := body["id"].(string)
	if id == "" || body["entityId"] != id {
		t.Fatalf("create must return id + entityId alias, got %v", body)
	}
	if body["embedded"] != true {
		t.Errorf("entity should be embedded after write, got %v", body["embedded"])
	}
	// The EMB item must exist alongside the ENT item.
	if fake.RawItem("USER#u1", "EMB#"+id) == nil {
		t.Errorf("EMB item missing after write")
	}

	// List: one item, plus the populated types enum for the UI filter.
	resp, body = doJSON(t, app, http.MethodGet, "/api/v1/entities", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list len = %d, want 1", len(items))
	}
	if types, _ := body["types"].([]any); len(types) != 6 {
		t.Errorf("types enum missing from list response: %v", body["types"])
	}
	// Type filter that matches nothing.
	_, body = doJSON(t, app, http.MethodGet, "/api/v1/entities?type=place", nil)
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Errorf("type=place should list 0 entities, got %d", len(items))
	}
	// Invalid type is rejected, never silently ignored.
	resp, _ = doJSON(t, app, http.MethodGet, "/api/v1/entities?type=alien", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid type filter status = %d, want 400", resp.StatusCode)
	}

	// Get by bare id (type probe) and 404 for unknown.
	resp, body = doJSON(t, app, http.MethodGet, "/api/v1/entities/"+id, nil)
	if resp.StatusCode != http.StatusOK || body["name"] != "Sam the coffee friend" {
		t.Fatalf("get = %d %v", resp.StatusCode, body)
	}
	resp, _ = doJSON(t, app, http.MethodGet, "/api/v1/entities/nope-123", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown entity status = %d, want 404", resp.StatusCode)
	}

	// PUT that changes the type must move the item (old sk deleted).
	resp, body = doJSON(t, app, http.MethodPut, "/api/v1/entities/"+id, map[string]any{
		"type": "info", "name": "Sam the coffee friend",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retype put status = %d (%v)", resp.StatusCode, body)
	}
	if fake.RawItem("USER#u1", "ENT#person#"+id) != nil {
		t.Errorf("old-type ENT item must be deleted on retype")
	}
	if fake.RawItem("USER#u1", "ENT#info#"+id) == nil {
		t.Errorf("new-type ENT item missing after retype")
	}

	// Forget via the contracts path: ENT + EMB both gone.
	resp, body = doJSON(t, app, http.MethodDelete, "/api/v1/memory/"+id, nil)
	if resp.StatusCode != http.StatusOK || body["ok"] != true {
		t.Fatalf("forget = %d %v", resp.StatusCode, body)
	}
	if fake.RawItem("USER#u1", "ENT#info#"+id) != nil || fake.RawItem("USER#u1", "EMB#"+id) != nil {
		t.Errorf("forget must delete both ENT and EMB items")
	}
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/memory/"+id, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second forget status = %d, want 404", resp.StatusCode)
	}
}

func TestEntityWriteValidation(t *testing.T) {
	app, _, _ := newMemoryAPIApp(t)

	cases := []map[string]any{
		{"type": "alien", "name": "x"},                       // bad type
		{"type": "person", "name": ""},                       // empty name
		{"type": "person", "name": strings.Repeat("a", 201)}, // name too long
		{"type": "person", "name": "ok", "relations": []map[string]string{{"type": "", "targetId": "t1"}}},
		{"type": "person", "name": "ok", "relations": []map[string]string{{"type": "knows", "targetId": "bad#id"}}},
	}
	for i, body := range cases {
		resp, _ := doJSON(t, app, http.MethodPost, "/api/v1/entities", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("case %d: status = %d, want 400", i, resp.StatusCode)
		}
	}
}

func TestMemoryUpsertByName(t *testing.T) {
	app, _, _ := newMemoryAPIApp(t)

	// POST /api/v1/memory without entityId twice with the same (type,name)
	// must refine one entity, not spawn a duplicate (memory_write semantics).
	_, first := doJSON(t, app, http.MethodPost, "/api/v1/memory", map[string]any{
		"type": "task", "name": "water the plants",
	})
	_, second := doJSON(t, app, http.MethodPost, "/api/v1/memory", map[string]any{
		"type": "task", "name": "water the plants", "attrs": map[string]any{"when": "friday"},
	})
	if first["id"] != second["id"] {
		t.Errorf("upsert-by-name minted a duplicate: %v vs %v", first["id"], second["id"])
	}
	_, list := doJSON(t, app, http.MethodGet, "/api/v1/entities?type=task", nil)
	if items, _ := list["items"].([]any); len(items) != 1 {
		t.Errorf("expected exactly 1 task after two upserts, got %d", len(items))
	}
}

func TestMemorySearch(t *testing.T) {
	app, _, _ := newMemoryAPIApp(t)

	_, coffee := doJSON(t, app, http.MethodPost, "/api/v1/entities", map[string]any{
		"type": "place", "name": "Blue Bottle coffee shop",
	})
	doJSON(t, app, http.MethodPost, "/api/v1/entities", map[string]any{
		"type": "person", "name": "Rex the dog",
	})

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/memory/search", map[string]any{
		"query": "where do I get coffee",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d (%v)", resp.StatusCode, body)
	}
	hits, _ := body["hits"].([]any)
	if len(hits) != 2 {
		t.Fatalf("hits len = %d, want 2", len(hits))
	}
	top, _ := hits[0].(map[string]any)
	if top["id"] != coffee["id"] {
		t.Errorf("top hit = %v, want the coffee entity %v", top["id"], coffee["id"])
	}
	if _, ok := top["score"].(float64); !ok {
		t.Errorf("hits must carry a numeric score, got %v", top["score"])
	}

	// Type facet filters the ranked list.
	_, body = doJSON(t, app, http.MethodPost, "/api/v1/memory/search", map[string]any{
		"query": "coffee", "types": []string{"person"},
	})
	hits, _ = body["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("type-filtered hits len = %d, want 1", len(hits))
	}
	if only, _ := hits[0].(map[string]any); only["type"] != "person" {
		t.Errorf("type filter leaked a %v entity", only["type"])
	}

	// Validation.
	resp, _ = doJSON(t, app, http.MethodPost, "/api/v1/memory/search", map[string]any{"query": ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty query status = %d, want 400", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodPost, "/api/v1/memory/search", map[string]any{
		"query": "x", "types": []string{"alien"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad types status = %d, want 400", resp.StatusCode)
	}
}

func TestGuidesCRUDAndSeed(t *testing.T) {
	app, _, _ := newMemoryAPIApp(t)

	// First list seeds the default guide (FR-MEM-09).
	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/guides", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list guides status = %d", resp.StatusCode)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("first list should seed exactly 1 guide, got %d", len(items))
	}
	seeded, _ := items[0].(map[string]any)
	if seeded["id"] != store.SeedGuideID {
		t.Errorf("seed guide id = %v, want %s", seeded["id"], store.SeedGuideID)
	}
	if seeded["title"] != "AI is an emerging technology" {
		t.Errorf("seed guide title = %v", seeded["title"])
	}

	// Create.
	resp, created := doJSON(t, app, http.MethodPost, "/api/v1/guides", map[string]any{
		"title": "Be brief", "text": "Answer in two sentences.", "priority": 5,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create guide status = %d (%v)", resp.StatusCode, created)
	}
	gid, _ := created["id"].(string)
	if gid == "" || created["version"] != float64(1) {
		t.Fatalf("created guide = %v", created)
	}

	// Edit bumps version; enabled toggle persists.
	resp, updated := doJSON(t, app, http.MethodPut, "/api/v1/guides/"+gid, map[string]any{
		"title": "Be brief", "text": "Answer in one sentence.", "enabled": false, "priority": 5,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update guide status = %d (%v)", resp.StatusCode, updated)
	}
	if updated["version"] != float64(2) || updated["enabled"] != false {
		t.Errorf("updated guide = %v, want version 2 enabled false", updated)
	}

	// List is priority-ascending (5 before the seed's 100).
	_, body = doJSON(t, app, http.MethodGet, "/api/v1/guides", nil)
	items, _ = body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("guide list len = %d, want 2", len(items))
	}
	if first, _ := items[0].(map[string]any); first["id"] != gid {
		t.Errorf("guides must sort priority-ascending; first = %v", first["id"])
	}

	// The alias `body` field is accepted for text (contracts/api.md name).
	resp, aliased := doJSON(t, app, http.MethodPut, "/api/v1/guides/"+gid, map[string]any{
		"title": "Be brief", "body": "Alias text.", "priority": 5,
	})
	if resp.StatusCode != http.StatusOK || aliased["text"] != "Alias text." {
		t.Errorf("body-alias put = %d %v", resp.StatusCode, aliased)
	}

	// Validation.
	resp, _ = doJSON(t, app, http.MethodPost, "/api/v1/guides", map[string]any{"title": "", "text": "x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty title status = %d, want 400", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodPost, "/api/v1/guides", map[string]any{
		"title": "t", "text": strings.Repeat("a", 4001),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("overlong text status = %d, want 400", resp.StatusCode)
	}

	// Delete + 404 on re-delete.
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/guides/"+gid, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("delete guide status = %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/guides/"+gid, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("re-delete guide status = %d, want 404", resp.StatusCode)
	}
}

func TestMemoryRoutesDegradeWithoutService(t *testing.T) {
	// nil memory.Service (no embedder): embedding-dependent routes answer
	// 503; store-only routes stay live.
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja")
	deps := &Deps{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		return c.Next()
	})
	RegisterMemoryRoutes(app, deps, nil)

	resp, _ := doJSON(t, app, http.MethodPost, "/api/v1/memory/search", map[string]any{"query": "x"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("search without service status = %d, want 503", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodPost, "/api/v1/entities", map[string]any{"type": "person", "name": "x"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("write without service status = %d, want 503", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodGet, "/api/v1/entities", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("list without service status = %d, want 200", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodGet, "/api/v1/guides", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("guides without service status = %d, want 200", resp.StatusCode)
	}
}

func TestMemoryRoutesRequireAuth(t *testing.T) {
	// No auth middleware → RequireAuth rejects with 401.
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja")
	deps := &Deps{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app := fiber.New()
	RegisterMemoryRoutes(app, deps, nil)

	resp, _ := doJSON(t, app, http.MethodGet, "/api/v1/entities", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated list status = %d, want 401", resp.StatusCode)
	}
}
