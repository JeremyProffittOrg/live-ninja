package webapp

import (
	"context"
	"errors"
	"os"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/gofiber/fiber/v2"
)

type phoneBudgetService interface {
	BudgetStatus(context.Context, string) (map[string]any, *tools.ToolError)
}

func RegisterPhoneBudgetRoutes(app *fiber.App, deps *Deps) {
	var service phoneBudgetService
	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err == nil {
		phone, e := newPhoneMessages(cfg, os.Getenv("PHONE_MESSAGES_FUNCTION_ARN"))
		if e == nil && phone != nil {
			service, _ = phone.(phoneBudgetService)
		}
	}
	registerPhoneBudgetAPI(app, service, func(ctx context.Context, uid string) error {
		if deps.Store == nil {
			return errors.New("identity store unavailable")
		}
		user, err := deps.Store.GetUserConsistent(ctx, uid)
		if err != nil {
			return err
		}
		if user == nil || user.Status != store.UserStatusActive || user.Role != store.RoleOwner {
			return errAPIUserNotAllowed
		}
		return nil
	})
}

func registerPhoneBudgetAPI(app *fiber.App, service phoneBudgetService, authorize func(context.Context, string) error) {
	api := app.Group("/api/v1/phone/budget", RequireOwner(), func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "no-store")
		if Scope(c) != "" || Surface(c) != "web" {
			return errorJSON(c, 403, "human_owner_required", "Use the owner's signed-in web interface.")
		}
		if authorize == nil || service == nil {
			return errorJSON(c, 503, "not_configured", "Phone budget is unavailable.")
		}
		if err := authorize(c.UserContext(), UserID(c)); err != nil {
			if errors.Is(err, errAPIUserNotAllowed) {
				return errorJSON(c, 403, "owner_only", "An active owner account is required.")
			}
			return errorJSON(c, 503, "authorization_unavailable", "Owner access could not be verified.")
		}
		return c.Next()
	})
	api.Get("/", func(c *fiber.Ctx) error {
		result, err := service.BudgetStatus(c.UserContext(), UserID(c))
		return phoneBudgetResponse(c, result, err)
	})
	// The $5 daily approval was retired on 2026-10-19: outgoing calls use a
	// standing $10 daily allowance inside the $100 monthly cap.
	api.Post("/grant", func(c *fiber.Ctx) error {
		return errorJSON(c, 410, "approval_retired", "Daily approvals are retired. Outgoing calls use a standing $10 daily allowance within the $100 monthly cap.")
	})
}

func phoneBudgetResponse(c *fiber.Ctx, result map[string]any, err *tools.ToolError) error {
	if err == nil {
		return c.JSON(result)
	}
	status := 502
	switch err.Code {
	case tools.CodeInvalidArgs:
		status = 400
	case tools.CodeForbidden:
		status = 403
	case tools.CodeAlreadyExists, tools.CodeConfirmationRequired:
		status = 409
	case tools.CodeNotConfigured:
		status = 503
	}
	return c.Status(status).JSON(fiber.Map{"error": err.Code, "message": "Phone budget could not be read. Refresh to try again.", "details": err.Details})
}
