package webapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/gofiber/fiber/v2"
)

// RegisterRuleReviewRoutes exposes deliberate human approvals. Authorization
// comes only from the signed-in UI request, never an assistant confirm flag.
// The UI generates requestId (UUID) and reviewedAt (RFC3339) once when opening
// its review and reuses both on repeated clicks or uncertain network retries.
// Each successful request is single-use; reviews expire after 15 minutes.
// 409 already_reviewed/review_expired/review_conflict requires reloading the
// current state. A new approval requires a newly opened, deliberate review.
func RegisterRuleReviewRoutes(app *fiber.App, deps *Deps) {
	api := app.Group("/api/v1/rule-reviews", RequireAuth(), CSRFProtect())
	api.Post("/", func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "no-store")
		if Scope(c) != "" {
			return errorJSON(c, 403, "insufficient_scope", "A personal signed-in session is required to approve a rule.")
		}
		if Surface(c) != "web" && Surface(c) != "android" {
			return errorJSON(c, 403, "trusted_surface_required", "Use the signed-in web or Android interface to approve a rule.")
		}
		if deps == nil || deps.Store == nil {
			return errorJSON(c, 503, "not_configured", "Rule review is not configured.")
		}
		if err := apiReauthorize(deps)(c.UserContext(), UserID(c)); err != nil {
			if errors.Is(err, errAPIUserNotAllowed) {
				return errorJSON(c, 403, "forbidden", "Your account cannot approve rules.")
			}
			return errorJSON(c, 503, "authorization_unavailable", "Your access could not be verified. Try again.")
		}
		if len(c.Body()) > 32*1024 {
			return apiBadRequest(c, "rule review is too large")
		}
		var body struct {
			RequestID         string `json:"requestId"`
			ReviewedAt        string `json:"reviewedAt"`
			Operation         string `json:"operation"`
			ExpectedVersion   *int   `json:"expectedVersion"`
			ExpectedUpdatedAt string `json:"expectedUpdatedAt"`
			ExpectedRule      *struct {
				Description *string `json:"description"`
				Body        *string `json:"body"`
				Enabled     *bool   `json:"enabled"`
			} `json:"expectedRule"`
			Proposed struct {
				Name        string  `json:"name"`
				Description *string `json:"description"`
				Body        *string `json:"body"`
				Enabled     *bool   `json:"enabled"`
			} `json:"proposed"`
		}
		decoder := json.NewDecoder(bytes.NewReader(c.Body()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			return apiBadRequest(c, "invalid rule review JSON")
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return apiBadRequest(c, "expected one rule review object")
		}
		if body.ExpectedVersion == nil || *body.ExpectedVersion < 0 {
			return apiBadRequest(c, "expectedVersion is required and must be nonnegative")
		}
		review := store.RuleReview{RequestID: body.RequestID, ReviewedAt: body.ReviewedAt, Operation: body.Operation, ExpectedVersion: *body.ExpectedVersion, ExpectedUpdatedAt: body.ExpectedUpdatedAt,
			Proposed: store.Rule{Name: strings.TrimSpace(body.Proposed.Name)}}
		if body.ExpectedRule != nil {
			if body.ExpectedRule.Description == nil || body.ExpectedRule.Body == nil || body.ExpectedRule.Enabled == nil {
				return apiBadRequest(c, "expectedRule must include description, body and enabled")
			}
			review.ExpectedRule = &store.RuleReviewSnapshot{Description: *body.ExpectedRule.Description, Body: *body.ExpectedRule.Body, Enabled: *body.ExpectedRule.Enabled}
		}
		switch body.Operation {
		case "save":
			if body.Proposed.Description == nil || body.Proposed.Body == nil || body.Proposed.Enabled == nil {
				return apiBadRequest(c, "save requires the complete description, body and enabled value you reviewed")
			}
			review.Proposed.Description = *body.Proposed.Description
			review.Proposed.Body = *body.Proposed.Body
			review.Proposed.Enabled = *body.Proposed.Enabled
		case "delete":
			if body.Proposed.Description != nil || body.Proposed.Body != nil || body.Proposed.Enabled != nil {
				return apiBadRequest(c, "delete proposed must contain only the rule name")
			}
		default:
			return apiBadRequest(c, "operation must be save or delete")
		}
		if review.ExpectedVersion == 0 && (review.Operation != "save" || review.ExpectedUpdatedAt != "" || review.ExpectedRule != nil) {
			return apiBadRequest(c, "creation requires expectedVersion 0 and no previous snapshot")
		}
		if review.ExpectedVersion > 0 && (review.ExpectedUpdatedAt == "" || review.ExpectedRule == nil) {
			return apiBadRequest(c, "expectedUpdatedAt and expectedRule are required for an existing rule")
		}
		result, err := deps.Store.ApplyRuleReview(c.UserContext(), UserID(c), review)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrInvalidRule):
				return apiBadRequest(c, err.Error())
			case errors.Is(err, store.ErrRuleLimit):
				return errorJSON(c, 409, "rule_limit", "Delete a rule before adding another.")
			case errors.Is(err, store.ErrRuleReviewAlreadyApplied):
				return errorJSON(c, 409, "already_reviewed", "This approval was already used. Reload the current rule before making another change.")
			case errors.Is(err, store.ErrRuleReviewExpired):
				return errorJSON(c, 409, "review_expired", "This review expired or its time is invalid. Open a fresh review before approving.")
			case errors.Is(err, store.ErrRuleReviewConflict):
				return errorJSON(c, 409, "review_conflict", "This rule changed. Reload it and review the current rule before approving.")
			case errors.Is(err, store.ErrRuleReviewForbidden):
				return errorJSON(c, 403, "forbidden", "Your account can no longer approve this rule.")
			default:
				return apiInternalError(c, deps, "apply reviewed rule", err)
			}
		}
		return c.JSON(result)
	})
}
