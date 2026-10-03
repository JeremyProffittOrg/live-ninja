package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/codeapproval"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/gofiber/fiber/v2"
)

func RegisterCodeApprovalRoutes(app *fiber.App, deps *Deps) {
	RegisterCodeApprovalAPI(app, deps.CodeApprovals, func(ctx context.Context, uid string) error {
		if deps.Store == nil {
			return codeapproval.ErrUnavailable
		}
		user, e := deps.Store.GetUserConsistent(ctx, uid)
		if e != nil {
			return e
		}
		if user == nil || user.Status != store.UserStatusActive || user.Role != store.RoleOwner {
			return codeapproval.ErrForbidden
		}
		return nil
	})
}

// RegisterCodeApprovalAPI is shared with the explicitly marked local preview.
// Callers install verified identity first. This registrar itself enforces CSRF,
// personal scope, a signed-in human surface and mandatory fresh ownership.
func RegisterCodeApprovalAPI(app *fiber.App, svc *codeapproval.Service, authorize func(context.Context, string) error) {
	api := app.Group("/api/v1/code-approvals", RequireAuth(), CSRFProtect(), func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "no-store")
		if Scope(c) != "" {
			return errorJSON(c, 403, "insufficient_scope", "Use a personal signed-in session to review coding work.")
		}
		if Surface(c) != store.SurfaceWeb && Surface(c) != store.SurfaceAndroid {
			return errorJSON(c, 403, "trusted_surface_required", "Use the signed-in web or Android interface.")
		}
		if svc == nil || authorize == nil {
			return errorJSON(c, 503, "not_configured", "Coding review storage is not configured.")
		}
		if e := authorize(c.UserContext(), UserID(c)); e != nil {
			if errors.Is(e, codeapproval.ErrForbidden) {
				return errorJSON(c, 403, "owner_only", "An active owner account is required.")
			}
			return errorJSON(c, 503, "authorization_unavailable", "Your current access could not be verified.")
		}
		return c.Next()
	})
	api.Get("/options", func(c *fiber.Ctx) error {
		options, e := svc.Options(c.UserContext())
		if e != nil {
			return codeApprovalError(c, e)
		}
		return c.JSON(options)
	})
	api.Get("/", func(c *fiber.Ctx) error {
		page, e := svc.List(c.UserContext(), UserID(c), queryLimit(c, 30, 50), c.Query("cursor"))
		if e != nil {
			return codeApprovalError(c, e)
		}
		return c.JSON(fiber.Map{"intents": page.Intents, "nextCursor": page.NextCursor, "capabilities": svc.Capabilities()})
	})
	api.Post("/prepare", func(c *fiber.Ctx) error {
		var body struct {
			codeapproval.Input
			RequestID string `json:"requestId"`
		}
		if ok, e := decodeCodeApproval(c, &body); !ok {
			return e
		}
		intent, e := svc.Prepare(c.UserContext(), UserID(c), body.Input, body.RequestID)
		if e != nil {
			return codeApprovalError(c, e)
		}
		return c.Status(201).JSON(fiber.Map{"intent": intent, "capabilities": svc.Capabilities()})
	})
	api.Get("/:id", func(c *fiber.Ctx) error {
		intent, e := svc.Get(c.UserContext(), UserID(c), c.Params("id"))
		if e != nil {
			return codeApprovalError(c, e)
		}
		return c.JSON(fiber.Map{"intent": intent, "capabilities": svc.Capabilities()})
	})
	api.Post("/:id/approve", func(c *fiber.Ctx) error {
		// The only mutable input is retry identity and the exact version reviewed.
		// Repository, machine, instructions, deployment and hash cannot be replaced.
		var body struct {
			ExpectedVersion    int64  `json:"expectedVersion"`
			ExpectedActionHash string `json:"expectedActionHash"`
			RequestID          string `json:"requestId"`
		}
		if ok, e := decodeCodeApproval(c, &body); !ok {
			return e
		}
		intent, e := svc.Approve(c.UserContext(), UserID(c), c.Params("id"), body.ExpectedVersion, body.ExpectedActionHash, body.RequestID)
		if e != nil {
			return codeApprovalError(c, e)
		}
		return c.JSON(fiber.Map{"intent": intent, "capabilities": svc.Capabilities()})
	})
}
func decodeCodeApproval(c *fiber.Ctx, value any) (bool, error) {
	if len(c.Body()) > 48<<10 {
		return false, errorJSON(c, 413, "request_too_large", "Coding review requests must be under 48 KiB.")
	}
	decoder := json.NewDecoder(strings.NewReader(string(c.Body())))
	decoder.DisallowUnknownFields()
	if e := decoder.Decode(value); e != nil {
		return false, apiBadRequest(c, "Invalid coding review request; check the fields.")
	}
	var extra any
	if e := decoder.Decode(&extra); e != io.EOF {
		return false, apiBadRequest(c, "Send one JSON request.")
	}
	return true, nil
}
func codeApprovalError(c *fiber.Ctx, e error) error {
	switch {
	case errors.Is(e, codeapproval.ErrValidation):
		return errorJSON(c, 400, "invalid_request", e.Error())
	case errors.Is(e, codeapproval.ErrNotFound):
		return errorJSON(c, 404, "not_found", "Coding review not found.")
	case errors.Is(e, codeapproval.ErrConflict):
		return errorJSON(c, 409, "version_conflict", "This intent changed or was already reviewed. Refresh it before continuing.")
	case errors.Is(e, codeapproval.ErrExpired):
		return errorJSON(c, 409, "approval_expired", "This review expired. Prepare and review a new intent.")
	case errors.Is(e, codeapproval.ErrForbidden):
		return errorJSON(c, 403, "owner_only", "An active owner account is required.")
	case errors.Is(e, codeapproval.ErrUnavailable):
		return errorJSON(c, 503, "verification_unavailable", "The fleet catalog could not be verified. Nothing was prepared or started.")
	case errors.Is(e, codeapproval.ErrCorrupt):
		return errorJSON(c, 503, "recovery_required", "This saved review requires operator recovery; no execution is permitted.")
	default:
		return errorJSON(c, 503, "storage_unavailable", "The review receipt could not be confirmed. Retry with the same requestId.")
	}
}
