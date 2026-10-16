package webapp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/gofiber/fiber/v2"
)

type phoneBudgetService interface {
	BudgetStatus(context.Context, string) (map[string]any, *tools.ToolError)
	BudgetGrant(context.Context, string, string, string) (map[string]any, *tools.ToolError)
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
		user, err := deps.Store.GetUser(ctx, uid)
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
	api.Post("/grant", func(c *fiber.Ctx) error {
		cookie, header := c.Cookies(CSRFCookieName), c.Get(CSRFHeaderName)
		if cookie == "" || header == "" || subtle.ConstantTimeCompare([]byte(cookie), []byte(header)) != 1 {
			return errorJSON(c, 403, "csrf_failed", "Refresh the page before authorizing a budget.")
		}
		if len(c.Body()) > 1024 {
			return apiBadRequest(c, "Budget request is too large.")
		}
		var body struct {
			RequestID   string `json:"requestId"`
			ExpectedDay string `json:"expectedDay"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(c.Body())))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			return apiBadRequest(c, "Invalid budget request.")
		}
		if decoder.Decode(new(any)) != io.EOF {
			return apiBadRequest(c, "Invalid budget request.")
		}
		if !regexp.MustCompile(`^[a-zA-Z0-9_-]{8,100}$`).MatchString(body.RequestID) {
			return apiBadRequest(c, "A valid requestId is required.")
		}
		day, err := time.Parse("2006-01-02", body.ExpectedDay)
		if err != nil || day.Format("2006-01-02") != body.ExpectedDay {
			return apiBadRequest(c, "A valid expectedDay is required.")
		}
		result, toolErr := service.BudgetGrant(c.UserContext(), UserID(c), body.RequestID, body.ExpectedDay)
		return phoneBudgetResponse(c, result, toolErr)
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
	return c.Status(status).JSON(fiber.Map{"error": err.Code, "message": "Budget request was not confirmed. Retry the same request; do not create a second grant.", "details": err.Details})
}
