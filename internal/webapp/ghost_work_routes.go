package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/gofiber/fiber/v2"
)

// ghostWorkPageBodyLimit bounds a POST archive-page read. A valid body holds a
// node (<=128), a session ID and a cursor (<=2048 bytes, validated again by
// ghost.historyQuery); 8 KiB leaves room for JSON escaping without accepting
// arbitrarily large bodies.
const ghostWorkPageBodyLimit = 8 << 10

var errGhostWorkPageTooLarge = errors.New("ghost work: page request too large")

// ghostWorkPageRequest is the strict JSON body of a POST page read. These are
// read-only archive queries: POST only keeps the opaque cursor out of the
// request line; nothing is created, changed or queued.
type ghostWorkPageRequest struct {
	NodeID    string
	SessionID string
	Cursor    string
}

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
	// GET page reads remain for Android and other existing clients.
	api.Get("/sessions", func(c *fiber.Ctx) error {
		return ghostWorkSessions(c, client, c.Query("node_id"), c.Query("cursor"))
	})
	api.Get("/events", func(c *fiber.Ctx) error {
		return ghostWorkEvents(c, client, c.Query("node_id"), c.Query("session_id"), c.Query("cursor"))
	})
	// POST page reads carry the opaque cursor in a bounded JSON body instead of
	// the request line. Same owner/scope/surface checks; CSRF is enforced by the
	// global CSRFProtect middleware for cookie-bearing sessions.
	api.Post("/sessions", func(c *fiber.Ctx) error {
		req, e := decodeGhostWorkPage(c)
		if e == nil && req.SessionID != "" {
			e = ghost.ErrInvalidRequest
		}
		if e != nil {
			return ghostWorkPageError(c, e)
		}
		return ghostWorkSessions(c, client, req.NodeID, req.Cursor)
	})
	api.Post("/events", func(c *fiber.Ctx) error {
		req, e := decodeGhostWorkPage(c)
		if e != nil {
			return ghostWorkPageError(c, e)
		}
		return ghostWorkEvents(c, client, req.NodeID, req.SessionID, req.Cursor)
	})
}

func ghostWorkSessions(c *fiber.Ctx, client *ghost.Client, node, cursor string) error {
	page, e := client.HistorySessions(c.UserContext(), node, cursor, TxID(c))
	if e != nil {
		return ghostWorkError(c, e)
	}
	return c.JSON(page)
}

func ghostWorkEvents(c *fiber.Ctx, client *ghost.Client, node, session, cursor string) error {
	page, e := client.HistoryEvents(c.UserContext(), node, session, cursor, TxID(c))
	if e != nil {
		return ghostWorkError(c, e)
	}
	return c.JSON(page)
}

// decodeGhostWorkPage accepts exactly one JSON object of string fields
// node_id/session_id/cursor. Unknown fields, nulls, non-strings, trailing data,
// encoded or non-JSON bodies, and bodies over the limit are rejected.
func decodeGhostWorkPage(c *fiber.Ctx) (ghostWorkPageRequest, error) {
	var out ghostWorkPageRequest
	raw := c.Request().Body()
	if len(raw) > ghostWorkPageBodyLimit {
		return out, errGhostWorkPageTooLarge
	}
	mediaType, _, _ := strings.Cut(c.Get(fiber.HeaderContentType), ";")
	if !strings.EqualFold(strings.TrimSpace(mediaType), fiber.MIMEApplicationJSON) || c.Get(fiber.HeaderContentEncoding) != "" {
		return out, ghost.ErrInvalidRequest
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return out, ghost.ErrInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var fields map[string]json.RawMessage
	if decoder.Decode(&fields) != nil {
		return out, ghost.ErrInvalidRequest
	}
	if _, e := decoder.Token(); !errors.Is(e, io.EOF) {
		return out, ghost.ErrInvalidRequest
	}
	for name, value := range fields {
		var target *string
		switch name {
		case "node_id":
			target = &out.NodeID
		case "session_id":
			target = &out.SessionID
		case "cursor":
			target = &out.Cursor
		default:
			return ghostWorkPageRequest{}, ghost.ErrInvalidRequest
		}
		v := bytes.TrimSpace(value)
		if len(v) == 0 || v[0] != '"' || json.Unmarshal(v, target) != nil {
			return ghostWorkPageRequest{}, ghost.ErrInvalidRequest
		}
	}
	return out, nil
}

func ghostWorkPageError(c *fiber.Ctx, e error) error {
	if errors.Is(e, errGhostWorkPageTooLarge) {
		return errorJSON(c, fiber.StatusRequestEntityTooLarge, "request_too_large", "This archive page request is larger than Live Ninja accepts. Reload the page and retry.")
	}
	return ghostWorkError(c, e)
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
