package webapp

import (
	"context"
	"errors"

	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/gofiber/fiber/v2"
)

// The Lambda integration has one pinned Ghost principal, not a per-member
// identity mapping. Only a freshly verified Live Ninja owner may use it.
func RegisterGhostWorkRoutes(app *fiber.App, deps *Deps) {
	RegisterGhostWorkAPI(app, deps.Ghost, func(ctx context.Context, uid string) error {
		if deps.Store == nil {
			return ghost.ErrNotConfigured
		}
		user, e := deps.Store.GetUserConsistent(ctx, uid)
		if e != nil {
			return e
		}
		if user == nil || user.Status != store.UserStatusActive || user.Role != store.RoleOwner {
			return ghost.ErrNotAuthorized
		}
		return nil
	})
}

func RegisterGhostWorkAPI(app *fiber.App, client *ghost.Client, authorize func(context.Context, string) error) {
	api := app.Group("/api/v1/ghost-work", func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "no-store")
		return c.Next()
	}, RequireAuth(), func(c *fiber.Ctx) error {
		if Scope(c) != "" {
			return errorJSON(c, 403, "insufficient_scope", "Use a personal signed-in owner session to browse Ghost work.")
		}
		if Surface(c) != store.SurfaceWeb && Surface(c) != store.SurfaceAndroid {
			return errorJSON(c, 403, "trusted_surface_required", "Use the signed-in web or Android interface.")
		}
		if authorize == nil {
			return errorJSON(c, 503, "authorization_unavailable", "Your current owner access could not be verified.")
		}
		if e := authorize(c.UserContext(), UserID(c)); e != nil {
			if errors.Is(e, ghost.ErrNotAuthorized) {
				return errorJSON(c, 403, "owner_only", "An active owner account is required. Clear previously displayed Ghost content.")
			}
			return errorJSON(c, 503, "authorization_unavailable", "Your current owner access could not be verified.")
		}
		if !client.Ready() {
			return ghostWorkError(c, ghost.ErrNotConfigured)
		}
		return c.Next()
	})
	api.Get("/nodes", func(c *fiber.Ctx) error {
		nodes, e := client.Nodes(c.UserContext(), TxID(c))
		if e != nil {
			return ghostWorkError(c, e)
		}
		if nodes == nil {
			nodes = []ghost.Node{}
		}
		return c.JSON(fiber.Map{"nodes": nodes, "source": "ghost", "binding": "explicit_provider_selection", "historyAvailability": "check_per_node"})
	})
	api.Get("/jobs", func(c *fiber.Ctx) error {
		events, e := client.ScheduledWork(c.UserContext(), TxID(c))
		if e != nil {
			return ghostWorkError(c, e)
		}
		return c.JSON(fiber.Map{"events": events, "source": "ghost", "runHistoryLimit": 10, "providerSessionBindingAvailable": false, "coverage": "authorized_nodes_only"})
	})
	api.Get("/sessions", func(c *fiber.Ctx) error {
		page, e := client.HistorySessions(c.UserContext(), c.Query("node_id"), c.Query("cursor"), TxID(c))
		if e != nil {
			return ghostWorkError(c, e)
		}
		return c.JSON(page)
	})
	api.Get("/events", func(c *fiber.Ctx) error {
		page, e := client.HistoryEvents(c.UserContext(), c.Query("node_id"), c.Query("session_id"), c.Query("cursor"), TxID(c))
		if e != nil {
			return ghostWorkError(c, e)
		}
		return c.JSON(page)
	})
}

func ghostWorkError(c *fiber.Ctx, e error) error {
	switch {
	case errors.Is(e, ghost.ErrNotAuthorized):
		status := 403
		var upstream *ghost.HTTPError
		if errors.As(e, &upstream) && upstream.StatusCode == 401 {
			status = 401
		}
		return errorJSON(c, status, "provider_access_denied", "Ghost access was denied. Clear displayed provider content and stop requests.")
	case errors.Is(e, ghost.ErrInvalidRequest):
		return errorJSON(c, 400, "invalid_request", "Check the exact node, provider session and cursor.")
	case errors.Is(e, ghost.ErrConflict):
		return errorJSON(c, 409, "history_rescan_required", "The retained object changed. Restart the history scan and deduplicate by event ID.")
	case errors.Is(e, ghost.ErrHistoryGone):
		return errorJSON(c, 410, "history_expired", "Retained history expired or is missing. Restart the scan; missing bytes cannot be recovered here.")
	case errors.Is(e, ghost.ErrHistoryTooLarge):
		return errorJSON(c, 413, "history_object_too_large", "A retained object exceeds the provider's supported size; history is incomplete.")
	case errors.Is(e, ghost.ErrHistoryMalformed):
		return errorJSON(c, 422, "history_malformed", "A retained object cannot be decoded; history is incomplete.")
	case errors.Is(e, ghost.ErrQuota):
		return errorJSON(c, 429, "provider_rate_limited", "Ghost is rate limited. Retry this page later.")
	case errors.Is(e, ghost.ErrNotFound):
		return errorJSON(c, 404, "provider_not_found", "The requested provider resource was not found.")
	case errors.Is(e, ghost.ErrNotConfigured):
		return errorJSON(c, 503, "provider_not_configured", "Ghost is not configured for this deployment.")
	case errors.Is(e, ghost.ErrTransportUnavailable):
		return errorJSON(c, 503, "provider_transport_unavailable", "Ghost did not accept the internal read. Its history integration may not yet be deployed.")
	default:
		return errorJSON(c, 503, "provider_unavailable", "Ghost could not return this page. Retry; do not treat this as complete history.")
	}
}
