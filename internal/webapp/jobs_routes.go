package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/gofiber/fiber/v2"
)

// RegisterJobsRoutes mounts the Jobs resource behind the same verified identity,
// CSRF and fresh allow-list checks as the existing tool API. No model tool can
// approve a review checkpoint through this surface.
func RegisterJobsRoutes(app *fiber.App, deps *Deps) {
	RegisterJobsAPI(app, deps.Jobs, deps.JobsSchedulingEnabled, func(ctx context.Context, uid string) error {
		if deps.Store == nil {
			return fmt.Errorf("identity store unavailable")
		}
		return apiReauthorize(deps)(ctx, uid)
	})
}

// RegisterJobsAPI shares the real HTTP contract with the loopback-only local
// runner. Authorize is mandatory; callers must install verified auth context and
// CSRF before registering. Production uses RegisterJobsRoutes, never local auth.
func RegisterJobsAPI(app *fiber.App, svc *jobs.Service, scheduling bool, authorize func(context.Context, string) error) {
	api := app.Group("/api/v1/jobs", RequireAuth(), func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "no-store")
		if Scope(c) != "" {
			return errorJSON(c, 403, "insufficient_scope", "A personal signed-in session is required for Jobs.")
		}
		if svc == nil {
			return errorJSON(c, 503, "not_configured", "Jobs storage is not configured.")
		}
		if authorize == nil {
			return errorJSON(c, 503, "not_configured", "Jobs authorization is not configured.")
		}
		if err := authorize(c.UserContext(), UserID(c)); err != nil {
			if errors.Is(err, errAPIUserNotAllowed) || errors.Is(err, jobs.ErrForbidden) {
				return errorJSON(c, 403, "forbidden", "Your account cannot access Jobs.")
			}
			return errorJSON(c, 503, "authorization_unavailable", "Your access could not be verified. Try again.")
		}
		if c.Method() != fiber.MethodGet && c.Method() != fiber.MethodHead && Surface(c) != "web" && Surface(c) != "android" {
			return errorJSON(c, 403, "trusted_surface_required", "Use the signed-in web or Android interface to change Jobs.")
		}
		return c.Next()
	})
	registerJobsHistoryRoutes(api, svc)
	api.Get("/", func(c *fiber.Ctx) error {
		page, err := svc.List(c.UserContext(), UserID(c), queryLimit(c, 30, 50), c.Query("cursor"))
		if err != nil {
			return jobsError(c, err)
		}
		return c.JSON(fiber.Map{"jobs": page.Jobs, "nextCursor": page.NextCursor, "capabilities": jobsCapabilities(scheduling)})
	})
	api.Post("/", func(c *fiber.Ctx) error {
		var b jobRequest
		if ok, err := decodeJobRequest(c, &b); !ok {
			return err
		}
		if !scheduling && scheduledJob(b.Input) {
			return errorJSON(c, 409, "scheduler_disabled", "Background scheduling is not enabled. Save a manual job instead.")
		}
		j, err := svc.Create(c.UserContext(), UserID(c), b.Input, b.RequestID)
		if err != nil {
			return jobsError(c, err)
		}
		return c.Status(201).JSON(fiber.Map{"job": j})
	})
	api.Get("/:id", func(c *fiber.Ctx) error {
		j, err := svc.Get(c.UserContext(), UserID(c), c.Params("id"))
		if err != nil {
			return jobsError(c, err)
		}
		return c.JSON(fiber.Map{"job": j})
	})
	api.Patch("/:id", func(c *fiber.Ctx) error {
		var b jobRequest
		if ok, err := decodeJobRequest(c, &b); !ok {
			return err
		}
		if !scheduling && scheduledJob(b.Input) {
			return errorJSON(c, 409, "scheduler_disabled", "Background scheduling is not enabled.")
		}
		j, err := svc.Update(c.UserContext(), UserID(c), c.Params("id"), b.Input, b.ExpectedVersion, b.RequestID)
		if err != nil {
			return jobsError(c, err)
		}
		return c.JSON(fiber.Map{"job": j})
	})
	for _, a := range []string{"pause", "resume", "cancel"} {
		action := a
		api.Post("/:id/"+action, func(c *fiber.Ctx) error {
			var b jobRequest
			if ok, err := decodeJobRequest(c, &b); !ok {
				return err
			}
			if action == "resume" && !scheduling {
				j, e := svc.Get(c.UserContext(), UserID(c), c.Params("id"))
				if e != nil {
					return jobsError(c, e)
				}
				if j.NextRunAt != "" || j.Schedule.Kind != "once" || j.Schedule.At != "" {
					return errorJSON(c, 409, "scheduler_disabled", "Background scheduling is not enabled.")
				}
			}
			j, err := svc.Action(c.UserContext(), UserID(c), c.Params("id"), action, b.ExpectedVersion, b.RequestID)
			if err != nil {
				return jobsError(c, err)
			}
			return c.JSON(fiber.Map{"job": j})
		})
	}
	api.Get("/:id/runs", func(c *fiber.Ctx) error {
		p, e := svc.ListRuns(c.UserContext(), UserID(c), c.Params("id"), queryLimit(c, 20, 50), c.Query("cursor"))
		if e != nil {
			return jobsError(c, e)
		}
		return c.JSON(p)
	})
	api.Post("/:id/run", func(c *fiber.Ctx) error {
		var b jobRequest
		if ok, err := decodeJobRequest(c, &b); !ok {
			return err
		}
		r, e := svc.RunNow(c.UserContext(), UserID(c), c.Params("id"), b.ExpectedVersion, b.RequestID)
		if e != nil {
			return jobsError(c, e)
		}
		return jobRunResponse(c, svc, r)
	})
	for _, a := range []string{"retry", "approve", "cancel"} {
		action := a
		api.Post("/:id/runs/:runId/"+action, func(c *fiber.Ctx) error {
			var b jobRequest
			if ok, err := decodeJobRequest(c, &b); !ok {
				return err
			}
			var r *jobs.Run
			var e error
			switch action {
			case "retry":
				r, e = svc.RetryRun(c.UserContext(), UserID(c), c.Params("id"), c.Params("runId"), b.ExpectedVersion, b.RequestID)
			case "approve":
				r, e = svc.ApproveRun(c.UserContext(), UserID(c), c.Params("id"), c.Params("runId"), b.ExpectedVersion, b.RequestID)
			case "cancel":
				r, e = svc.CancelRun(c.UserContext(), UserID(c), c.Params("id"), c.Params("runId"), b.ExpectedVersion, b.RequestID)
			}
			if e != nil {
				return jobsError(c, e)
			}
			return jobRunResponse(c, svc, r)
		})
	}
}

type jobRequest struct {
	jobs.Input
	ExpectedVersion int64  `json:"expectedVersion"`
	RequestID       string `json:"requestId"`
}

func decodeJobRequest(c *fiber.Ctx, b *jobRequest) (bool, error) {
	if len(c.Body()) > 16384 {
		return false, errorJSON(c, 413, "request_too_large", "Job requests must be under 16 KiB.")
	}
	dec := json.NewDecoder(strings.NewReader(string(c.Body())))
	dec.DisallowUnknownFields()
	if err := dec.Decode(b); err != nil {
		return false, apiBadRequest(c, "Invalid Jobs request: check fields and schedule.")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return false, apiBadRequest(c, "Send one JSON request.")
	}
	if strings.TrimSpace(b.RequestID) == "" {
		return false, apiBadRequest(c, "A requestId is required for safe retries.")
	}
	return true, nil
}
func scheduledJob(in jobs.Input) bool {
	return in.Schedule.Kind != "" && in.Schedule.Kind != "once" || in.Schedule.At != ""
}
func jobRunResponse(c *fiber.Ctx, svc *jobs.Service, r *jobs.Run) error {
	j, e := svc.Get(c.UserContext(), UserID(c), c.Params("id"))
	if e != nil {
		return jobsError(c, e)
	}
	return c.JSON(fiber.Map{"run": r, "job": j})
}
func jobsError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, jobs.ErrUnsupported):
		return errorJSON(c, 501, "unsupported_command", "This command or conversation provider is not connected. A note can be saved without changing execution.")
	case errors.Is(err, jobs.ErrValidation):
		return errorJSON(c, 400, "invalid_request", err.Error())
	case errors.Is(err, jobs.ErrNotFound):
		return errorJSON(c, 404, "not_found", "Job or run not found.")
	case errors.Is(err, jobs.ErrConflict):
		return errorJSON(c, 409, "version_conflict", "This job changed. Refresh it before trying again.")
	case errors.Is(err, jobs.ErrInvalidState):
		return errorJSON(c, 409, "invalid_state", err.Error())
	case errors.Is(err, jobs.ErrForbidden):
		return errorJSON(c, 403, "forbidden", "This operation is not allowed.")
	case errors.Is(err, jobs.ErrCorrupt):
		return errorJSON(c, 503, "recovery_required", "This saved job needs operator recovery. Its data has been retained.")
	case errors.Is(err, jobs.ErrLimit):
		return errorJSON(c, 429, "limit_exceeded", err.Error())
	}
	return errorJSON(c, 503, "jobs_unavailable", "Jobs could not be saved or loaded. Your request can be retried safely.")
}
func jobsCapabilities(scheduling bool) fiber.Map {
	return fiber.Map{"reminder": true, "review": true, "scheduling": scheduling, "historyLimit": 50, "durableHistory": true, "noteCommands": true, "steering": false, "providers": []fiber.Map{
		{"id": "reminder", "label": "In-app reminders", "available": true, "reason": "Creates a durable reminder receipt in Jobs. No email is sent."},
		{"id": "review", "label": "Human review", "available": true, "reason": "Waits for your review of the saved instructions. Approval records your acknowledgement; it does not execute external work."},
		{"id": "coding", "label": "Coding agents", "available": false, "reason": "Durable isolated execution is not connected to Jobs yet."},
		{"id": "email", "label": "Email and calendar", "available": false, "reason": "No connected provider or action approval integration."},
		{"id": "browser", "label": "Browser and desktop", "available": false, "reason": "An isolated execution provider is required."},
		{"id": "documents", "label": "Document and image generation", "available": false, "reason": "Generation workers are not connected to Jobs yet."},
	}}
}
