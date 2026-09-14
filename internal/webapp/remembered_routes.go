package webapp

import (
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
)

// agentcore-memory (plan.md memory-tools-cutover): the Memory page's
// "Learned from conversations" list. These are the records AWS extracted
// from the user's own conversations, read straight from AgentCore — nothing
// is cached in the table. Forget is the only mutation: a record is
// re-derived by consolidation, so editing it in place would not stick.

// rememberedListLimit bounds one page listing; the API pages at 100.
const rememberedListLimit = 200

func handleListRemembered(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		if !deps.AgentMemory.Admits(Role(c)) {
			// Not configured, or this account is outside the rollout mode:
			// an honest "off" so the page can say so instead of "nothing yet".
			return c.JSON(fiber.Map{"enabled": false, "items": []fiber.Map{}})
		}
		recs, err := deps.AgentMemory.ListRecords(c.Context(), userID, rememberedListLimit)
		if err != nil {
			return apiInternalError(c, deps, "list remembered records", err)
		}
		items := make([]fiber.Map, 0, len(recs))
		for _, r := range recs {
			item := fiber.Map{"id": r.ID, "text": r.Text, "namespace": r.Namespace}
			if !r.CreatedAt.IsZero() {
				item["createdAt"] = r.CreatedAt.UTC().Format(time.RFC3339)
			}
			items = append(items, item)
		}
		return c.JSON(fiber.Map{"enabled": true, "items": items})
	}
}

func handleForgetRemembered(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		if !deps.AgentMemory.Admits(Role(c)) {
			return errorJSON(c, fiber.StatusServiceUnavailable, "not_configured",
				"Learning from conversations is not switched on for this account.")
		}
		id := c.Params("id")
		ok, err := deps.AgentMemory.DeleteRecord(c.Context(), userID, id)
		if err != nil {
			return apiInternalError(c, deps, "forget remembered record", err)
		}
		if !ok {
			return errorJSON(c, fiber.StatusNotFound, "not_found", "No such remembered fact.")
		}
		deps.Log.Info("api: remembered record forgotten", slog.String("userId", userID), slog.String("recordId", id))
		return c.JSON(fiber.Map{"ok": true, "id": id})
	}
}
