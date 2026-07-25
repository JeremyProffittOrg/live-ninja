// Deliverables HTTP surface (M9 Deliverables Store, FR-DLV-04..06) —
// the read/manage API behind the web Download Center and the Android
// Files tab (both consume these routes identically):
//
//   - GET    /api/v1/deliverables               — paginated newest-first list
//     (single-partition Query, never a Scan; ?limit=&cursor=).
//   - GET    /api/v1/deliverables/:id/download  — 302 redirect to a
//     15-minute presigned S3 GET (key prefix-checked to the caller).
//   - DELETE /api/v1/deliverables/:id           — remove object + index item.
//
// Creation/zipping/delivery are tool-router operations
// (internal/tools/deliverable.go → internal/deliv), not HTTP POSTs —
// the assistant is the producer; this surface is the consumer.
//
// RegisterDeliverablesRoutes is called from cmd/web/main.go alongside the
// other registrars, behind the same ExtractAuthContext middleware; the
// /api/v1 group here is additionally fail-closed via RequireAuth
// (defense in depth, mirroring api_routes.go). Identity comes only from
// the auth Locals — never from the request (anti-confused-deputy,
// NFR-02) — and cross-user ids 404 indistinguishably from absent ones.
package webapp

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/deliv"
)

const (
	deliverablesDefaultPageSize = 25
	deliverablesMaxPageSize     = 100
)

// RegisterDeliverablesRoutes mounts the deliverables list/download/delete
// API. When the deliverables service is not configured (no
// DELIVERABLES_BUCKET), every route answers 503 rather than half-working.
func RegisterDeliverablesRoutes(app *fiber.App, deps *Deps) {
	api := app.Group("/api/v1", RequireAuth())
	api.Post("/deliverables/upload-intents", handleCreateUploadIntent(deps))
	api.Post("/deliverables/:id/upload-complete", handleCompleteUpload(deps))
	api.Get("/deliverables", handleListDeliverables(deps))
	api.Get("/deliverables/:id/download", handleDownloadDeliverable(deps))
	api.Delete("/deliverables/:id", handleDeleteDeliverable(deps))
}

func deliverablesUnavailable(c *fiber.Ctx) error {
	return errorJSON(c, fiber.StatusServiceUnavailable, "not_configured", "The deliverables store is not configured.")
}

// ---- POST /api/v1/deliverables/upload-intents ----

func handleCreateUploadIntent(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Deliv == nil {
			return deliverablesUnavailable(c)
		}

		var body struct {
			Name        string `json:"name"`
			ContentType string `json:"contentType"`
			SizeBytes   int64  `json:"sizeBytes"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "request body must be valid JSON")
		}

		intent, err := deps.Deliv.BeginMediaUpload(
			c.Context(), UserID(c), body.Name, body.ContentType, body.SizeBytes,
		)
		if err != nil {
			switch {
			case errors.Is(err, deliv.ErrNameTaken):
				return errorJSON(c, fiber.StatusConflict, "already_exists", "A file with that name already exists.")
			case errors.Is(err, deliv.ErrBadInput):
				return apiBadRequest(c, err.Error())
			default:
				return apiInternalError(c, deps, "create media upload intent", err)
			}
		}

		d := intent.Deliverable
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{
			"deliverableId": d.DeliverableID,
			"name":          d.Name,
			"status":        d.Status,
			"contentType":   d.ContentType,
			"sizeBytes":     d.SizeBytes,
			"uploadUrl":     intent.URL,
			"expiresAt":     intent.ExpiresAt.Format(time.RFC3339),
			"headers":       intent.RequiredHeader,
		})
	}
}

// ---- POST /api/v1/deliverables/:id/upload-complete ----

func handleCompleteUpload(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Deliv == nil {
			return deliverablesUnavailable(c)
		}
		id := strings.TrimSpace(c.Params("id"))
		if id == "" {
			return apiBadRequest(c, "deliverable id is required")
		}

		d, err := deps.Deliv.CompleteMediaUpload(c.Context(), UserID(c), id)
		if err != nil {
			switch {
			case errors.Is(err, deliv.ErrNotFound):
				return errorJSON(c, fiber.StatusNotFound, "not_found", "Deliverable not found.")
			case errors.Is(err, deliv.ErrNotReady):
				return errorJSON(c, fiber.StatusConflict, "not_ready", "This upload is not pending media.")
			case errors.Is(err, deliv.ErrBadInput):
				return apiBadRequest(c, err.Error())
			default:
				return apiInternalError(c, deps, "complete media upload", err)
			}
		}
		return c.JSON(fiber.Map{
			"deliverableId": d.DeliverableID,
			"name":          d.Name,
			"status":        d.Status,
			"contentType":   d.ContentType,
			"sizeBytes":     d.SizeBytes,
			"createdAt":     d.CreatedAt,
		})
	}
}

// ---- GET /api/v1/deliverables ----

func handleListDeliverables(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Deliv == nil {
			return deliverablesUnavailable(c)
		}
		userID := UserID(c)

		limit := deliverablesDefaultPageSize
		if raw := c.Query("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > deliverablesMaxPageSize {
				return apiBadRequest(c, "limit must be an integer between 1 and 100")
			}
			limit = n
		}

		items, next, err := deps.Deliv.List(c.Context(), userID, int32(limit), c.Query("cursor"))
		if err != nil {
			if strings.Contains(err.Error(), "invalid cursor") {
				return apiBadRequest(c, "cursor is not a valid page cursor")
			}
			return apiInternalError(c, deps, "list deliverables", err)
		}

		out := make([]fiber.Map, 0, len(items))
		for _, d := range items {
			out = append(out, fiber.Map{
				"deliverableId": d.DeliverableID,
				"name":          d.Name,
				"kind":          d.Kind,
				"status":        d.Status,
				"contentType":   d.ContentType,
				"sizeBytes":     d.SizeBytes,
				"createdAt":     d.CreatedAt,
			})
		}
		resp := fiber.Map{"deliverables": out}
		if next != "" {
			resp["nextCursor"] = next
		}
		return c.JSON(resp)
	}
}

// ---- GET /api/v1/deliverables/:id/download ----

func handleDownloadDeliverable(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Deliv == nil {
			return deliverablesUnavailable(c)
		}
		userID := UserID(c)
		id := strings.TrimSpace(c.Params("id"))
		if id == "" {
			return apiBadRequest(c, "deliverable id is required")
		}

		res, err := deps.Deliv.PresignDownload(c.Context(), userID, id)
		if err != nil {
			switch {
			case errors.Is(err, deliv.ErrNotFound):
				// Absent and other-user ids answer identically.
				return errorJSON(c, fiber.StatusNotFound, "not_found", "Deliverable not found.")
			case errors.Is(err, deliv.ErrNotReady):
				return errorJSON(c, fiber.StatusConflict, "not_ready", "This deliverable is still being built (or failed to build).")
			}
			return apiInternalError(c, deps, "presign deliverable download", err)
		}

		// The Location target is a credentialed, short-lived URL — make
		// sure no intermediary or history cache holds onto the redirect.
		c.Set(fiber.HeaderCacheControl, "no-store")
		return c.Redirect(res.URL, fiber.StatusFound)
	}
}

// ---- DELETE /api/v1/deliverables/:id ----

func handleDeleteDeliverable(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Deliv == nil {
			return deliverablesUnavailable(c)
		}
		userID := UserID(c)
		id := strings.TrimSpace(c.Params("id"))
		if id == "" {
			return apiBadRequest(c, "deliverable id is required")
		}

		if err := deps.Deliv.Delete(c.Context(), userID, id); err != nil {
			if errors.Is(err, deliv.ErrNotFound) {
				return errorJSON(c, fiber.StatusNotFound, "not_found", "Deliverable not found.")
			}
			return apiInternalError(c, deps, "delete deliverable", err)
		}
		return c.SendStatus(fiber.StatusNoContent)
	}
}
