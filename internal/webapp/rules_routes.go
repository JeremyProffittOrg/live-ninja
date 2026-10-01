// Assistant rules routes: the authenticated /api/v1 surface behind the
// "Rules" section of the Memory page (web/static/js/memory.mjs).
//
//   - GET    /api/v1/rules        — list the caller's rules, sorted by name.
//     The store seeds the default "location-and-time" rule on a user's
//     first empty list, so the page never special-cases it.
//   - POST   /api/v1/rules        — create or update a rule by name
//     ({name, description, body, enabled?}); source is always "user".
//     201 when the name is new, 200 when it replaced an existing rule.
//   - PUT    /api/v1/rules/:name  — {enabled} toggles one rule.
//   - DELETE /api/v1/rules/:name  — delete one rule.
//
// A rule is a named instruction the assistant reads on demand: only the
// name and description go into every session (the broker builds that index
// at mint); the body is fetched with the rule_load tool when a turn
// matches the description. These routes only manage the items. All item
// shape, key format, and the 50-rule cap live in internal/store/rules.go,
// shared with the rule_* voice tools, so the two surfaces cannot diverge.
// Identity always re-derives from the auth Locals, and every route is
// fail-closed via RequireAuth (mounted from RegisterMemoryRoutes).
package webapp

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// ruleNameHint is the 400 message for a bad slug. Field limits come from
// internal/store/rules.go; the store validates again on write (and its
// ErrInvalidRule also maps to 400), so the two checks cannot disagree.
const ruleNameHint = "name must be 3 to 48 lowercase letters, digits and single hyphens, like \"trip-packing\""

func ruleJSON(r *store.Rule) fiber.Map {
	return fiber.Map{
		"name":        r.Name,
		"description": r.Description,
		"body":        r.Body,
		"enabled":     r.Enabled,
		"source":      r.Source,
		"createdAt":   r.CreatedAt,
		"updatedAt":   r.UpdatedAt,
		"version":     r.Version,
	}
}

// ---- GET /api/v1/rules ----

func handleListRules(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		rules, err := deps.Store.ListRules(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "list rules", err)
		}
		items := make([]fiber.Map, 0, len(rules))
		for i := range rules {
			items = append(items, ruleJSON(&rules[i]))
		}
		return c.JSON(fiber.Map{"rules": items})
	}
}

// ---- POST /api/v1/rules ----

type ruleWriteBody struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Enabled     *bool  `json:"enabled"`
}

func validateRuleWrite(b *ruleWriteBody) string {
	if !store.ValidRuleName(b.Name) {
		return ruleNameHint
	}
	if n := utf8.RuneCountInString(b.Description); n < store.RuleDescriptionMinRunes || n > store.RuleDescriptionMaxRunes {
		return "description must be 10 to 200 characters and say when the rule applies"
	}
	if b.Body == "" || utf8.RuneCountInString(b.Body) > store.RuleBodyMaxRunes {
		return "body must be a non-empty string of at most 4000 characters"
	}
	return ""
}

func handleWriteRule(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		var body ruleWriteBody
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		body.Name = strings.TrimSpace(body.Name)
		body.Description = store.NormalizeRuleDescription(body.Description)
		body.Body = strings.TrimSpace(body.Body)
		if msg := validateRuleWrite(&body); msg != "" {
			return apiBadRequest(c, msg)
		}

		// Probe first: it decides 201 vs 200, and an edit that omits
		// "enabled" keeps the rule's current switch instead of silently
		// turning a disabled rule back on.
		existing, err := deps.Store.GetRule(c.Context(), userID, body.Name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return apiInternalError(c, deps, "get rule", err)
		}
		enabled := true
		if existing != nil {
			enabled = existing.Enabled
		}
		if body.Enabled != nil {
			enabled = *body.Enabled
		}

		stored, err := deps.Store.UpsertRule(c.Context(), store.Rule{
			UserID:      userID,
			Name:        body.Name,
			Description: body.Description,
			Body:        body.Body,
			Enabled:     enabled,
			Source:      store.RuleSourceUser,
		})
		if err != nil {
			if errors.Is(err, store.ErrRuleLimit) {
				return errorJSON(c, fiber.StatusConflict, "rule_limit",
					fmt.Sprintf("You already have %d rules. Delete one before you add another.", store.MaxRules))
			}
			if errors.Is(err, store.ErrInvalidRule) {
				return apiBadRequest(c, err.Error())
			}
			return apiInternalError(c, deps, "upsert rule", err)
		}

		status := fiber.StatusOK
		if existing == nil {
			status = fiber.StatusCreated
		}
		return c.Status(status).JSON(ruleJSON(stored))
	}
}

// ---- PUT /api/v1/rules/:name ----

type ruleToggleBody struct {
	Enabled *bool `json:"enabled"`
}

func handleToggleRule(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		name := c.Params("name")
		if !store.ValidRuleName(name) {
			return apiBadRequest(c, ruleNameHint)
		}
		var body ruleToggleBody
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if body.Enabled == nil {
			return apiBadRequest(c, "enabled must be true or false")
		}
		stored, err := deps.Store.SetRuleEnabled(c.Context(), userID, name, *body.Enabled)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apiNotFound(c)
			}
			return apiInternalError(c, deps, "set rule enabled", err)
		}
		return c.JSON(ruleJSON(stored))
	}
}

// ---- DELETE /api/v1/rules/:name ----

func handleDeleteRule(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		name := c.Params("name")
		if !store.ValidRuleName(name) {
			return apiBadRequest(c, ruleNameHint)
		}
		if err := deps.Store.DeleteRule(c.Context(), userID, name); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apiNotFound(c)
			}
			return apiInternalError(c, deps, "delete rule", err)
		}
		return c.JSON(fiber.Map{"ok": true})
	}
}
