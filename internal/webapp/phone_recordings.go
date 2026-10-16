package webapp

import (
	"context"
	"errors"
	"net/url"
	"os"
	"regexp"
	"strconv"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/gofiber/fiber/v2"
)

type phoneRecordingsService interface {
	Recordings(context.Context, string, string, string, int) (map[string]any, *tools.ToolError)
	RecordingMedia(context.Context, string, string, string, string) (map[string]any, *tools.ToolError)
}

func (p *phoneMessages) Recordings(ctx context.Context, owner, source, cursor string, limit int) (map[string]any, *tools.ToolError) {
	return p.invoke(ctx, map[string]any{"operation": "owner_recordings_list", "ownerUserId": owner, "source": source, "cursor": cursor, "limit": limit})
}
func (p *phoneMessages) RecordingMedia(ctx context.Context, owner, source, sid, disposition string) (map[string]any, *tools.ToolError) {
	return p.invoke(ctx, map[string]any{"operation": "owner_recording_media", "ownerUserId": owner, "source": source, "callSid": sid, "disposition": disposition})
}
func RegisterPhoneRecordingRoutes(app *fiber.App, deps *Deps) {
	var service phoneRecordingsService
	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err == nil {
		phone, e := newPhoneMessages(cfg, os.Getenv("PHONE_MESSAGES_FUNCTION_ARN"))
		if e == nil && phone != nil {
			service, _ = phone.(phoneRecordingsService)
		}
	}
	registerPhoneRecordingAPI(app, service, func(ctx context.Context, owner string) error {
		if deps.Store == nil {
			return errors.New("identity unavailable")
		}
		u, e := deps.Store.GetUserConsistent(ctx, owner)
		if e != nil {
			return e
		}
		if u == nil || u.Status != store.UserStatusActive || u.Role != store.RoleOwner {
			return errAPIUserNotAllowed
		}
		return nil
	})
}

var recordingSID = regexp.MustCompile("^CA[0-9a-fA-F]{32}$")
var recordingSource = regexp.MustCompile("^[a-z0-9][a-z0-9-]{0,99}$")

func registerPhoneRecordingAPI(app *fiber.App, service phoneRecordingsService, authorize func(context.Context, string) error) {
	api := app.Group("/api/v1/phone/recordings", RequireOwner(), func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "no-store")
		if Scope(c) != "" {
			return errorJSON(c, 403, "owner_only", "A full owner session is required.")
		}
		if service == nil || authorize == nil {
			return errorJSON(c, 503, "not_configured", "Phone recordings are unavailable.")
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
		source := c.Query("source", "messages")
		cursor := c.Query("cursor")
		limit, err := strconv.Atoi(c.Query("limit", "25"))
		if err != nil || limit < 1 || limit > 50 || !recordingSource.MatchString(source) || len(cursor) > 100 {
			return apiBadRequest(c, "Invalid recording page.")
		}
		result, e := service.Recordings(c.UserContext(), UserID(c), source, cursor, limit)
		return phoneRecordingResponse(c, result, e)
	})
	api.Get("/:sid/media", func(c *fiber.Ctx) error {
		source := c.Query("source", "messages")
		disposition := c.Query("disposition", "inline")
		if !recordingSource.MatchString(source) || !recordingSID.MatchString(c.Params("sid")) || (disposition != "inline" && disposition != "attachment") {
			return apiBadRequest(c, "Invalid recording request.")
		}
		result, e := service.RecordingMedia(c.UserContext(), UserID(c), source, c.Params("sid"), disposition)
		if e != nil {
			return phoneRecordingResponse(c, nil, e)
		}
		raw, _ := result["url"].(string)
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || (u.Host != "live-ninja-deliverables-759775734231.s3.amazonaws.com" && u.Host != "live-ninja-deliverables-759775734231.s3.us-east-1.amazonaws.com") || u.RawQuery == "" {
			return errorJSON(c, 502, "invalid_media", "Recording link could not be verified.")
		}
		return c.JSON(result)
	})
}
func phoneRecordingResponse(c *fiber.Ctx, result map[string]any, e *tools.ToolError) error {
	if e == nil {
		return c.JSON(result)
	}
	status := 502
	switch e.Code {
	case tools.CodeInvalidArgs:
		status = 400
	case tools.CodeForbidden:
		status = 403
	case tools.CodeNotFound:
		status = 404
	case tools.CodeAlreadyExists:
		status = 409
	case tools.CodeNotConfigured:
		status = 503
	}
	return errorJSON(c, status, "recording_unavailable", "Recording is not available. Refresh the list or try again later.")
}
