// Memory Layer routes (M10, FR-MEM-01/02/04/05/07/09): the authenticated
// /api/v1 surface behind the memory browser + guide manager UIs
// (web/static/js/memory.mjs, Android MemoryDtos.kt) —
//
//   - GET    /api/v1/entities            — list entities (?type= optional,
//     limit/cursor paginated; Query-only via store.ListEntities).
//   - POST   /api/v1/entities            — create an entity.
//   - GET    /api/v1/entities/:id        — fetch one entity (?type= makes
//     it one GetItem; bare id probes the six type keys — still key
//     lookups, never a Scan).
//   - PUT    /api/v1/entities/:id        — create/replace an entity.
//   - POST   /api/v1/memory              — upsert ({entityId?, type, name,
//     attrs, relations?}) — the shape memory.mjs posts (contracts/api.md
//     "write a typed memory item"); same core as the memory_write tool.
//   - DELETE /api/v1/memory/:id          — "forget": deletes ENT# AND its
//     EMB# embedding (both stores, FR-MEM-05).
//   - DELETE /api/v1/entities/:id        — alias for forget.
//   - POST   /api/v1/memory/search       — semantic recall via
//     memory.Service.Search (embed query → bounded single-partition
//     brute-force cosine → ranked entities).
//   - GET    /api/v1/guides              — list Guide Entities (the store
//     seeds the default "AI is an emerging technology" guide on a user's
//     first list, FR-MEM-09).
//   - POST   /api/v1/guides              — create a guide (server uuid).
//   - PUT    /api/v1/guides/:id          — create/edit a guide (version
//     bumps atomically server-side).
//   - DELETE /api/v1/guides/:id          — delete a guide.
//
// This file is deliberately a thin HTTP layer: every item shape, key
// format, and Query/GetItem access pattern lives in internal/store
// (entities.go) and the search/write/forget cores in internal/memory —
// one canonical implementation shared with the tool registry
// (internal/tools/memory.go), so the voice tools and this REST surface
// can never diverge.
//
// Guide *injection* into realtime sessions is the broker's job
// (store.ListEnabledGuides at session mint); these routes only manage the
// items. Identity always re-derives from the auth Locals (anti-confused-
// deputy, same posture as api_routes.go) and every route is fail-closed
// via RequireAuth.
package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/JeremyProffittOrg/live-ninja/internal/memory"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// resourceIDPattern constrains client-supplied ids (entity/guide/topic).
// "#" is structurally forbidden — it is the sort-key delimiter.
var resourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,63}$`)

func apiNotFound(c *fiber.Ctx) error {
	return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not_found"})
}

func memNotConfigured(c *fiber.Ctx) error {
	return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
		"error": "not_configured", "message": "the memory layer is not configured",
	})
}

func queryLimit(c *fiber.Ctx, def, max int) int {
	n := c.QueryInt("limit", def)
	if n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// ---- wire projections (id + alias field so the lenient web/Android
// normalizers resolve the same key either way) ----

func entityJSON(e *store.Entity) fiber.Map {
	attrs := e.Attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	relations := e.Relations
	if relations == nil {
		relations = []store.Relation{}
	}
	return fiber.Map{
		"id":        e.EntityID,
		"entityId":  e.EntityID,
		"type":      e.Type,
		"name":      e.Name,
		"attrs":     attrs,
		"relations": relations,
		"embedded":  e.Embedded,
		"updatedAt": e.UpdatedAt,
	}
}

func guideJSON(g *store.Guide) fiber.Map {
	return fiber.Map{
		"id":        g.GuideID,
		"guideId":   g.GuideID,
		"title":     g.Title,
		"text":      g.Text,
		"enabled":   g.Enabled,
		"priority":  g.Priority,
		"version":   g.Version,
		"updatedAt": g.UpdatedAt,
	}
}

// ---- registrar ----

// RegisterMemoryRoutes mounts the M10 memory + guide API. svc is the
// memory core (embedder-backed); nil degrades the embedding-dependent
// routes (search, writes) to 503 not_configured while the store-only
// routes (list/get/forget/guides) stay live — same graceful-degradation
// pattern as the M9 deliverables routes.
func RegisterMemoryRoutes(app *fiber.App, deps *Deps, svc *memory.Service) {
	api := app.Group("/api/v1", RequireAuth())

	api.Get("/entities", handleListEntities(deps))
	api.Post("/entities", handleWriteEntity(deps, svc, entityIDFromBody))
	api.Get("/entities/:id", handleGetEntity(deps))
	api.Put("/entities/:id", handleWriteEntity(deps, svc, entityIDFromParam))
	api.Delete("/entities/:id", handleForgetEntity(deps))

	api.Post("/memory", handleWriteEntity(deps, svc, entityIDFromBody)) // memory.mjs upsert path
	api.Delete("/memory/:id", handleForgetEntity(deps))                 // contracts/api.md "forget" path
	api.Post("/memory/search", handleMemorySearch(deps, svc))

	api.Get("/guides", handleListGuides(deps))
	api.Post("/guides", handleWriteGuide(deps, true))
	api.Put("/guides/:id", handleWriteGuide(deps, false))
	api.Delete("/guides/:id", handleDeleteGuide(deps))
}

// ---- GET /api/v1/entities ----

func handleListEntities(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		entityType := c.Query("type")
		if entityType != "" && !store.ValidEntityType(entityType) {
			return apiBadRequest(c, "type must be one of "+strings.Join(store.EntityTypes, ", "))
		}
		limit := queryLimit(c, 100, 500)

		// store.ListEntities walks the whole (bounded, ≤2000-item) ENT#
		// range in sk order; the limit/cursor page is cut here so the wire
		// response stays small. cursor = the last row's "<type>#<id>".
		all, err := deps.Store.ListEntities(c.Context(), userID, entityType)
		if err != nil {
			return apiInternalError(c, deps, "list entities", err)
		}

		start := 0
		if cur := c.Query("cursor"); cur != "" {
			found := false
			for i := range all {
				if all[i].Type+"#"+all[i].EntityID == cur {
					start = i + 1
					found = true
					break
				}
			}
			if !found {
				// Stale cursor (entity forgotten between pages) — restart
				// rather than 400: the UI just re-lists.
				start = 0
			}
		}

		end := start + limit
		if end > len(all) {
			end = len(all)
		}
		items := make([]fiber.Map, 0, end-start)
		for i := start; i < end; i++ {
			items = append(items, entityJSON(&all[i]))
		}

		resp := fiber.Map{"items": items, "types": store.EntityTypes}
		if end < len(all) {
			resp["nextCursor"] = all[end-1].Type + "#" + all[end-1].EntityID
		}
		return c.JSON(resp)
	}
}

// ---- GET /api/v1/entities/:id ----

func handleGetEntity(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		id := c.Params("id")
		if !resourceIDPattern.MatchString(id) {
			return apiBadRequest(c, "entity id is invalid")
		}
		entityType := c.Query("type")
		if entityType != "" && !store.ValidEntityType(entityType) {
			return apiBadRequest(c, "type must be one of "+strings.Join(store.EntityTypes, ", "))
		}

		var (
			e   *store.Entity
			err error
		)
		if entityType != "" {
			e, err = deps.Store.GetEntity(c.Context(), userID, entityType, id)
		} else {
			e, err = deps.Store.GetEntityByID(c.Context(), userID, id)
		}
		if err != nil {
			return apiInternalError(c, deps, "get entity", err)
		}
		if e == nil {
			return apiNotFound(c)
		}
		return c.JSON(entityJSON(e))
	}
}

// ---- entity writes (POST /entities, PUT /entities/:id, POST /memory) ----

type entityWriteBody struct {
	EntityID  string           `json:"entityId"`
	Type      string           `json:"type"`
	Name      string           `json:"name"`
	Attrs     map[string]any   `json:"attrs"`
	Relations []store.Relation `json:"relations"`
}

func validateEntityBody(b *entityWriteBody) string {
	if !store.ValidEntityType(b.Type) {
		return "type must be one of " + strings.Join(store.EntityTypes, ", ")
	}
	if strings.TrimSpace(b.Name) == "" || len(b.Name) > 200 {
		return "name must be a non-empty string of at most 200 characters"
	}
	if len(b.Relations) > 50 {
		return "at most 50 relations are allowed"
	}
	for _, r := range b.Relations {
		if strings.TrimSpace(r.Type) == "" || len(r.Type) > 64 {
			return "every relation needs a type of at most 64 characters"
		}
		if !resourceIDPattern.MatchString(r.TargetID) {
			return "every relation needs a valid targetId"
		}
	}
	if b.Attrs != nil {
		raw, err := json.Marshal(b.Attrs)
		if err != nil || len(raw) > 8*1024 {
			return "attrs must be a JSON object of at most 8KB"
		}
	}
	return ""
}

// entityIDFromParam / entityIDFromBody pick where the target id comes
// from for the three write mounts ("" = mint or upsert-by-name).
func entityIDFromParam(c *fiber.Ctx, _ *entityWriteBody) string { return c.Params("id") }
func entityIDFromBody(_ *fiber.Ctx, b *entityWriteBody) string  { return b.EntityID }

func handleWriteEntity(deps *Deps, svc *memory.Service, pickID func(*fiber.Ctx, *entityWriteBody) string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		if svc == nil {
			return memNotConfigured(c)
		}

		var body entityWriteBody
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if msg := validateEntityBody(&body); msg != "" {
			return apiBadRequest(c, msg)
		}

		id := strings.TrimSpace(pickID(c, &body))
		if id != "" && !resourceIDPattern.MatchString(id) {
			return apiBadRequest(c, "entity id is invalid")
		}

		// An explicit-id write that changes the type moves the item to a
		// new sort key — remove the old ENT# item so it can't linger under
		// the stale type (its EMB# is keyed by id alone and gets rewritten).
		created := id == ""
		if id != "" {
			existing, err := deps.Store.GetEntityByID(c.Context(), userID, id)
			if err != nil {
				return apiInternalError(c, deps, "resolve entity for write", err)
			}
			created = existing == nil
			if existing != nil && existing.Type != body.Type {
				if derr := deps.Store.DeleteEntity(c.Context(), userID, existing.Type, existing.EntityID); derr != nil && !errors.Is(derr, store.ErrNotFound) {
					return apiInternalError(c, deps, "delete re-typed entity", derr)
				}
			}
		}

		e := &store.Entity{
			EntityID:  id,
			Type:      body.Type,
			Name:      strings.TrimSpace(body.Name),
			Attrs:     body.Attrs,
			Relations: body.Relations,
		}
		written, err := svc.WriteEntity(c.Context(), userID, e)
		if err != nil && !errors.Is(err, memory.ErrEmbedFailed) {
			return apiInternalError(c, deps, "write entity", err)
		}

		resp := entityJSON(written)
		if errors.Is(err, memory.ErrEmbedFailed) {
			// Saved but not yet semantically searchable — honest partial
			// success (a later write re-embeds), surfaced, never silent.
			deps.Log.Warn("memory: entity saved but embedding failed",
				slog.String("error", err.Error()), slog.String("entityId", written.EntityID))
			resp["warning"] = "saved, but semantic indexing failed; it will re-index on the next edit"
		}
		status := fiber.StatusOK
		if created {
			status = fiber.StatusCreated
		}
		return c.Status(status).JSON(resp)
	}
}

// ---- DELETE /api/v1/entities/:id + /api/v1/memory/:id ("forget") ----

// forgetEntity mirrors memory.Service.Forget but needs only the store, so
// forget keeps working even when the embedder (and thus svc) is absent:
// EMB# is deleted first so a partial failure can never leave an orphan
// vector resurfacing "forgotten" facts in search (FR-MEM-05).
func forgetEntity(ctx context.Context, deps *Deps, userID, entityID string) (*store.Entity, error) {
	ent, err := deps.Store.GetEntityByID(ctx, userID, entityID)
	if err != nil || ent == nil {
		return nil, err
	}
	if err := deps.Store.DeleteEmbedding(ctx, userID, entityID); err != nil {
		return nil, err
	}
	if err := deps.Store.DeleteEntity(ctx, userID, ent.Type, entityID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return ent, nil
}

func handleForgetEntity(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		id := c.Params("id")
		if !resourceIDPattern.MatchString(id) {
			return apiBadRequest(c, "entity id is invalid")
		}

		ent, err := forgetEntity(c.Context(), deps, userID, id)
		if err != nil {
			return apiInternalError(c, deps, "forget entity", err)
		}
		if ent == nil {
			return apiNotFound(c)
		}
		return c.JSON(fiber.Map{"ok": true})
	}
}

// ---- POST /api/v1/memory/search ----

func handleMemorySearch(deps *Deps, svc *memory.Service) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		if svc == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "not_configured", "message": "semantic search is not configured (no embedder)",
			})
		}

		var body struct {
			Query string   `json:"query"`
			Types []string `json:"types"`
			Limit int      `json:"limit"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if strings.TrimSpace(body.Query) == "" || len(body.Query) > 1000 {
			return apiBadRequest(c, "query must be a non-empty string of at most 1000 characters")
		}
		for _, t := range body.Types {
			if !store.ValidEntityType(t) {
				return apiBadRequest(c, "types entries must be one of "+strings.Join(store.EntityTypes, ", "))
			}
		}

		// Ask the core for the max page and cut type filter + limit here
		// (Search itself has no type facet — filtering after ranking keeps
		// scores global rather than per-type).
		results, err := svc.Search(c.Context(), userID, body.Query, memory.MaxTopK)
		if err != nil {
			deps.Log.Error("memory: search failed", slog.String("error", err.Error()), slog.String("userId", userID))
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
				"error": "search_failed", "message": "Semantic search failed; try again.",
			})
		}

		limit := body.Limit
		if limit < 1 {
			limit = memory.DefaultTopK
		}
		if limit > memory.MaxTopK {
			limit = memory.MaxTopK
		}
		typeAllowed := func(t string) bool {
			if len(body.Types) == 0 {
				return true
			}
			for _, want := range body.Types {
				if t == want {
					return true
				}
			}
			return false
		}

		hits := make([]fiber.Map, 0, limit)
		for i := range results {
			if len(hits) >= limit {
				break
			}
			if !typeAllowed(results[i].Entity.Type) {
				continue
			}
			hit := entityJSON(&results[i].Entity)
			hit["score"] = results[i].Score
			hits = append(hits, hit)
		}
		return c.JSON(fiber.Map{"hits": hits, "model": memory.EmbedModelID})
	}
}

// ---- GET /api/v1/guides ----

func handleListGuides(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		// ListGuides seeds the default "AI is an emerging technology"
		// guide on a user's very first list (FR-MEM-09) and returns
		// priority-ascending — exactly the broker's injection order.
		guides, err := deps.Store.ListGuides(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "list guides", err)
		}
		items := make([]fiber.Map, 0, len(guides))
		for i := range guides {
			items = append(items, guideJSON(&guides[i]))
		}
		return c.JSON(fiber.Map{"items": items})
	}
}

// ---- POST /api/v1/guides + PUT /api/v1/guides/:id ----

type guideWriteBody struct {
	Title    string `json:"title"`
	Text     string `json:"text"`
	Body     string `json:"body"` // contracts/api.md alias for text
	Enabled  *bool  `json:"enabled"`
	Priority *int   `json:"priority"`
}

func (b *guideWriteBody) text() string {
	if strings.TrimSpace(b.Text) != "" {
		return b.Text
	}
	return b.Body
}

func validateGuideBody(b *guideWriteBody) string {
	if strings.TrimSpace(b.Title) == "" || len(b.Title) > 120 {
		return "title must be a non-empty string of at most 120 characters"
	}
	txt := b.text()
	if strings.TrimSpace(txt) == "" || len([]rune(txt)) > 4000 {
		return "text must be a non-empty string of at most 4000 characters"
	}
	if b.Priority != nil && (*b.Priority < 0 || *b.Priority > 1000) {
		return "priority must be between 0 and 1000"
	}
	return ""
}

func handleWriteGuide(deps *Deps, create bool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		id := uuid.NewString()
		if !create {
			id = c.Params("id")
			if !resourceIDPattern.MatchString(id) {
				return apiBadRequest(c, "guide id is invalid")
			}
		}

		var body guideWriteBody
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if msg := validateGuideBody(&body); msg != "" {
			return apiBadRequest(c, msg)
		}

		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		priority := 100
		if body.Priority != nil {
			priority = *body.Priority
		}

		// UpsertGuide bumps version atomically (ADD) and returns the
		// stored row; PUT on a fresh id creates it at version 1, matching
		// the contract's "PUT creates or edits".
		stored, err := deps.Store.UpsertGuide(c.Context(), &store.Guide{
			UserID:   userID,
			GuideID:  id,
			Title:    strings.TrimSpace(body.Title),
			Text:     body.text(),
			Enabled:  enabled,
			Priority: priority,
		})
		if err != nil {
			return apiInternalError(c, deps, "upsert guide", err)
		}

		status := fiber.StatusOK
		if create {
			status = fiber.StatusCreated
		}
		return c.Status(status).JSON(guideJSON(stored))
	}
}

// ---- DELETE /api/v1/guides/:id ----

func handleDeleteGuide(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		id := c.Params("id")
		if !resourceIDPattern.MatchString(id) {
			return apiBadRequest(c, "guide id is invalid")
		}

		if err := deps.Store.DeleteGuide(c.Context(), userID, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apiNotFound(c)
			}
			return apiInternalError(c, deps, "delete guide", err)
		}
		return c.JSON(fiber.Map{"ok": true})
	}
}
