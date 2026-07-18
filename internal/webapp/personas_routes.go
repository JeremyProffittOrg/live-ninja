// Persona platform routes (personas feature): the full grouped persona
// list, custom-persona CRUD, and sharing. Registered from
// RegisterAPIRoutes (api_routes.go) under the same RequireAuth'd /api/v1
// group; CSRF on the mutating verbs is enforced by the globally-mounted
// CSRFProtect, same as every other /api/v1 route.
//
//	GET    /api/v1/personas            — grouped list: builtin / mine / shared
//	POST   /api/v1/personas            — create custom (or duplicate via copyOf)
//	GET    /api/v1/personas/:id        — one persona (instructions only for mine)
//	PUT    /api/v1/personas/:id        — edit own custom persona
//	DELETE /api/v1/personas/:id        — delete own custom persona
//	POST   /api/v1/personas/:id/share  — {shared: bool} toggle + catalog mirror
//
// Anti-injection contract (internal/realtime/personas.go): instruction
// text is only ever returned for the CALLER'S OWN personas (they authored
// it); built-in and shared-persona instructions never leave the server —
// duplication (copyOf) copies them server-side. At session time clients
// still send only a persona ID; qualifyPersonaRef (below) turns it into a
// server-composed ref the broker re-verifies against live DynamoDB state
// at mint.
package webapp

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// Validation bounds for custom personas. Instructions match the settings
// document's 4000-char persona.systemInstructions cap.
const (
	personaNameMax         = 80
	personaDescriptionMax  = 200
	personaInstructionsMax = 4000
	// maxCustomPersonas caps the per-user custom set so ListUserPersonas
	// stays a small single-partition read forever.
	maxCustomPersonas = 100
)

// registerPersonaRoutes mounts the persona surface on the RequireAuth'd
// /api/v1 group (called from RegisterAPIRoutes).
func registerPersonaRoutes(api fiber.Router, deps *Deps) {
	api.Get("/personas", handleListPersonaLibrary(deps))
	api.Post("/personas", handleCreatePersona(deps))
	api.Get("/personas/:id", handleGetPersonaByID(deps))
	api.Put("/personas/:id", handleUpdatePersona(deps))
	api.Delete("/personas/:id", handleDeletePersona(deps))
	api.Post("/personas/:id/share", handleSharePersona(deps))
}

// ---- wire shapes ----

func builtinPersonaJSON(p realtime.Persona) fiber.Map {
	// Never include Instructions/Style — built-in instruction text stays
	// server-side (catalog.go's standing rule).
	return fiber.Map{
		"id":          p.ID,
		"name":        p.Name,
		"description": p.Description,
		"voice":       p.Voice,
		"builtin":     true,
	}
}

func ownPersonaJSON(p *store.UserPersona) fiber.Map {
	return fiber.Map{
		"id":           p.PersonaID,
		"name":         p.Name,
		"description":  p.Description,
		"instructions": p.Instructions, // the caller authored these
		"voice":        p.Voice,
		"shared":       p.Shared,
		"createdAt":    p.CreatedAt,
		"updatedAt":    p.UpdatedAt,
	}
}

func sharedPersonaJSON(p *store.CatalogPersona) fiber.Map {
	// No instructions: another user's text never leaves the server; "copy
	// to mine" (POST /personas {copyOf}) copies it server-side.
	owner := p.OwnerName
	if owner == "" {
		owner = "another user"
	}
	return fiber.Map{
		"id":          p.PersonaID,
		"name":        p.Name,
		"description": p.Description,
		"voice":       p.Voice,
		"owner":       owner,
		"shared":      true,
	}
}

// ---- GET /api/v1/personas ----

func handleListPersonaLibrary(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		builtin := make([]fiber.Map, 0, 16)
		for _, p := range realtime.BuiltinPersonas() {
			builtin = append(builtin, builtinPersonaJSON(p))
		}

		own, err := deps.Store.ListUserPersonas(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "list personas", err)
		}
		mine := make([]fiber.Map, 0, len(own))
		for i := range own {
			mine = append(mine, ownPersonaJSON(&own[i]))
		}

		catalog, err := deps.Store.ListSharedPersonas(c.Context())
		if err != nil {
			return apiInternalError(c, deps, "list shared personas", err)
		}
		shared := make([]fiber.Map, 0, len(catalog))
		for i := range catalog {
			if catalog[i].OwnerID == userID {
				continue // already in "mine" (with its shared badge)
			}
			shared = append(shared, sharedPersonaJSON(&catalog[i]))
		}

		return c.JSON(fiber.Map{"builtin": builtin, "mine": mine, "shared": shared})
	}
}

// ---- POST /api/v1/personas (create / duplicate) ----

type personaBody struct {
	Name         *string `json:"name"`
	Description  *string `json:"description"`
	Instructions *string `json:"instructions"`
	Voice        *string `json:"voice"`
	CopyOf       string  `json:"copyOf,omitempty"`
}

// validatePersonaBody bounds-checks the assembled persona fields.
// Returns "" when valid.
func validatePersonaBody(name, description, instructions, voice string) string {
	if strings.TrimSpace(name) == "" || len([]rune(name)) > personaNameMax {
		return "name is required (at most 80 characters)"
	}
	if len([]rune(description)) > personaDescriptionMax {
		return "description must be at most 200 characters"
	}
	if strings.TrimSpace(instructions) == "" || len([]rune(instructions)) > personaInstructionsMax {
		return "instructions are required (at most 4000 characters)"
	}
	if voice != "" && !realtime.IsRealtimeVoice(voice) {
		return "voice must be a supported realtime voice (or empty)"
	}
	return ""
}

func handleCreatePersona(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		var body personaBody
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}

		// Seed from the duplication source first (server-side copy — the
		// only way built-in/shared instruction text reaches a user's own
		// persona), then let explicit body fields override.
		var name, description, instructions, voice string
		if src := strings.TrimSpace(body.CopyOf); src != "" {
			seed, errMsg := personaCopySeed(c.Context(), deps, userID, src)
			if errMsg != "" {
				return apiBadRequest(c, errMsg)
			}
			name, description, instructions, voice = seed.name, seed.description, seed.instructions, seed.voice
		}
		if body.Name != nil {
			name = *body.Name
		}
		if body.Description != nil {
			description = *body.Description
		}
		if body.Instructions != nil {
			instructions = *body.Instructions
		}
		if body.Voice != nil {
			voice = *body.Voice
		}
		name = strings.TrimSpace(name)
		description = strings.TrimSpace(description)
		instructions = strings.TrimSpace(instructions)
		voice = strings.TrimSpace(voice)

		if msg := validatePersonaBody(name, description, instructions, voice); msg != "" {
			return apiBadRequest(c, msg)
		}

		existing, err := deps.Store.ListUserPersonas(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "list personas for cap", err)
		}
		if len(existing) >= maxCustomPersonas {
			return apiBadRequest(c, "custom persona limit reached — delete one first")
		}

		id, err := store.NewPersonaID()
		if err != nil {
			return apiInternalError(c, deps, "generate persona id", err)
		}
		p := &store.UserPersona{
			PersonaID:    id,
			Name:         name,
			Description:  description,
			Instructions: instructions,
			Voice:        voice,
		}
		if err := deps.Store.CreateUserPersona(c.Context(), userID, p); err != nil {
			return apiInternalError(c, deps, "create persona", err)
		}
		return c.Status(fiber.StatusCreated).JSON(ownPersonaJSON(p))
	}
}

type personaSeed struct {
	name, description, instructions, voice string
}

// personaCopySeed resolves a duplication source in the same order the
// mint resolves personas: built-in -> the caller's own -> shared catalog.
// The returned error string is a client-facing 400 message ("" = ok).
func personaCopySeed(ctx context.Context, deps *Deps, userID, srcID string) (personaSeed, string) {
	if realtime.IsBuiltinPersona(srcID) {
		for _, b := range realtime.BuiltinPersonas() {
			if b.ID != srcID {
				continue
			}
			style := b.Style
			if style == "" {
				// The default persona's style is the operational core
				// itself; seed the copy with an editable sketch instead of
				// leaking the full core prompt.
				style = "Fast, warm, and practical. Friendly, concise, and personal — " +
					"the standard Live Ninja delivery. Edit this to make it your own."
			}
			return personaSeed{
				name:         b.Name + " (copy)",
				description:  b.Description,
				instructions: style,
				voice:        b.Voice,
			}, ""
		}
	}
	if own, err := deps.Store.GetUserPersona(ctx, userID, srcID); err == nil && own != nil {
		return personaSeed{
			name:         own.Name + " (copy)",
			description:  own.Description,
			instructions: own.Instructions,
			voice:        own.Voice,
		}, ""
	}
	if cp, err := deps.Store.GetCatalogPersona(ctx, srcID); err == nil && cp != nil && cp.Shared {
		return personaSeed{
			name:         cp.Name + " (copy)",
			description:  cp.Description,
			instructions: cp.Instructions,
			voice:        cp.Voice,
		}, ""
	}
	return personaSeed{}, "copyOf does not name a built-in, your own, or a shared persona"
}

// ---- GET /api/v1/personas/:id ----

func handleGetPersonaByID(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, id := UserID(c), strings.TrimSpace(c.Params("id"))
		if id == "" {
			return apiBadRequest(c, "persona id is required")
		}
		if realtime.IsBuiltinPersona(id) {
			for _, b := range realtime.BuiltinPersonas() {
				if b.ID == id {
					return c.JSON(builtinPersonaJSON(b))
				}
			}
		}
		if own, err := deps.Store.GetUserPersona(c.Context(), userID, id); err == nil && own != nil {
			return c.JSON(ownPersonaJSON(own))
		} else if err != nil && !isPersonaKeyError(err) {
			return apiInternalError(c, deps, "get persona", err)
		}
		cp, err := deps.Store.GetCatalogPersona(c.Context(), id)
		if err != nil {
			return apiInternalError(c, deps, "get catalog persona", err)
		}
		if cp != nil && cp.Shared {
			return c.JSON(sharedPersonaJSON(cp))
		}
		return errorJSON(c, fiber.StatusNotFound, "not_found", "Persona not found.")
	}
}

// isPersonaKeyError reports whether err is the store's client-input
// validation (an id containing '#'/':' can never exist — treat as absent,
// not as an infrastructure failure).
func isPersonaKeyError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "personaID must not contain")
}

// ---- PUT /api/v1/personas/:id ----

func handleUpdatePersona(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, id := UserID(c), strings.TrimSpace(c.Params("id"))
		if id == "" {
			return apiBadRequest(c, "persona id is required")
		}
		if realtime.IsBuiltinPersona(id) {
			return errorJSON(c, fiber.StatusForbidden, "builtin_readonly",
				"Built-in personas can't be edited — duplicate it to make your own copy.")
		}

		var body personaBody
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}

		p, err := deps.Store.GetUserPersona(c.Context(), userID, id)
		if err != nil {
			if isPersonaKeyError(err) {
				return errorJSON(c, fiber.StatusNotFound, "not_found", "Persona not found.")
			}
			return apiInternalError(c, deps, "get persona", err)
		}
		if p == nil {
			// Same shape whether absent or another user's — never let a
			// caller probe shared personas via PUT.
			return errorJSON(c, fiber.StatusNotFound, "not_found", "Persona not found.")
		}

		if body.Name != nil {
			p.Name = strings.TrimSpace(*body.Name)
		}
		if body.Description != nil {
			p.Description = strings.TrimSpace(*body.Description)
		}
		if body.Instructions != nil {
			p.Instructions = strings.TrimSpace(*body.Instructions)
		}
		if body.Voice != nil {
			p.Voice = strings.TrimSpace(*body.Voice)
		}
		if msg := validatePersonaBody(p.Name, p.Description, p.Instructions, p.Voice); msg != "" {
			return apiBadRequest(c, msg)
		}

		if err := deps.Store.UpdateUserPersona(c.Context(), userID, personaOwnerName(c, deps, userID), p); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errorJSON(c, fiber.StatusNotFound, "not_found", "Persona not found.")
			}
			return apiInternalError(c, deps, "update persona", err)
		}
		return c.JSON(ownPersonaJSON(p))
	}
}

// ---- DELETE /api/v1/personas/:id ----

func handleDeletePersona(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, id := UserID(c), strings.TrimSpace(c.Params("id"))
		if id == "" {
			return apiBadRequest(c, "persona id is required")
		}
		if realtime.IsBuiltinPersona(id) {
			return errorJSON(c, fiber.StatusForbidden, "builtin_readonly",
				"Built-in personas can't be deleted.")
		}

		p, err := deps.Store.GetUserPersona(c.Context(), userID, id)
		if err != nil {
			if isPersonaKeyError(err) {
				return errorJSON(c, fiber.StatusNotFound, "not_found", "Persona not found.")
			}
			return apiInternalError(c, deps, "get persona for delete", err)
		}
		if p == nil {
			return errorJSON(c, fiber.StatusNotFound, "not_found", "Persona not found.")
		}

		if err := deps.Store.DeleteUserPersona(c.Context(), userID, id); err != nil {
			return apiInternalError(c, deps, "delete persona", err)
		}
		return c.SendStatus(fiber.StatusNoContent)
	}
}

// ---- POST /api/v1/personas/:id/share ----

func handleSharePersona(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, id := UserID(c), strings.TrimSpace(c.Params("id"))
		if id == "" {
			return apiBadRequest(c, "persona id is required")
		}
		if realtime.IsBuiltinPersona(id) {
			return errorJSON(c, fiber.StatusForbidden, "builtin_readonly",
				"Built-in personas are already available to everyone.")
		}

		var body struct {
			Shared *bool `json:"shared"`
		}
		if err := c.BodyParser(&body); err != nil || body.Shared == nil {
			return apiBadRequest(c, "shared (boolean) is required")
		}

		p, err := deps.Store.SetUserPersonaShared(c.Context(), userID,
			personaOwnerName(c, deps, userID), id, *body.Shared)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) || isPersonaKeyError(err) {
				return errorJSON(c, fiber.StatusNotFound, "not_found", "Persona not found.")
			}
			return apiInternalError(c, deps, "share persona", err)
		}
		return c.JSON(ownPersonaJSON(p))
	}
}

// personaOwnerName resolves the display name stamped on shared-catalog
// mirrors (attribution). Best-effort: a profile-read failure degrades to
// an empty name (rendered as "another user"), never a failed share.
func personaOwnerName(c *fiber.Ctx, deps *Deps, userID string) string {
	u, err := deps.Store.GetUser(c.Context(), userID)
	if err != nil || u == nil {
		if err != nil {
			deps.Log.Warn("api: owner name lookup for persona attribution failed",
				slog.String("error", err.Error()), slog.String("userId", userID))
		}
		return ""
	}
	if strings.TrimSpace(u.Name) != "" {
		return u.Name
	}
	return u.Email
}

// ---- mint-side persona qualification ----

// qualifyPersonaRef turns a client/settings persona ID into what the
// broker resolves at mint, enforcing the resolution order
// built-in -> the user's own -> shared catalog:
//
//   - built-in IDs (and the settings-page "custom" preset, and empty)
//     pass through untouched;
//   - IDs containing ':' are REJECTED to "default" — refs are composed
//     here from the verified auth context, never accepted from a client
//     (anti-injection: a client must not be able to aim the broker at
//     another user's partition);
//   - an ID matching one of the caller's own personas becomes
//     "user:<uid>:<id>"; a shared-catalog ID becomes "shared:<id>". The
//     broker re-checks both against live DynamoDB state at mint
//     (internal/realtime/personas_store.go);
//   - anything else passes through and resolves to the default persona
//     broker-side (stale-client tolerance).
//
// Lookups are best-effort: a store error passes the raw ID through (the
// broker then falls back to default) — a persona lookup must never take
// voice down.
func qualifyPersonaRef(ctx context.Context, deps *Deps, userID, id string) string {
	id = strings.TrimSpace(id)
	if id == "" || id == "custom" || realtime.IsBuiltinPersona(id) {
		return id
	}
	if strings.ContainsAny(id, ":#") {
		return "default"
	}
	if p, err := deps.Store.GetUserPersona(ctx, userID, id); err == nil && p != nil {
		return realtime.UserPersonaRef(userID, id)
	} else if err != nil {
		deps.Log.Warn("api: own-persona lookup for mint failed; passing raw id",
			slog.String("error", err.Error()), slog.String("userId", userID))
		return id
	}
	if cp, err := deps.Store.GetCatalogPersona(ctx, id); err == nil && cp != nil && cp.Shared {
		return realtime.SharedPersonaRef(id)
	} else if err != nil {
		deps.Log.Warn("api: shared-persona lookup for mint failed; passing raw id",
			slog.String("error", err.Error()), slog.String("userId", userID))
	}
	return id
}
