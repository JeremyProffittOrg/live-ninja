package webapp

// The base-knowledge suggestion queue's HTTP surface (M16, plan.md WS-2).
//
//	GET  /api/v1/profile/suggestions            the queue + the drawer badge count
//	POST /api/v1/profile/suggestions/:id/resolve {"action":"approve"|"reject"|"keep"|"undo"}
//
// The split of responsibility between these two routes and the client is the
// whole design, and it is deliberate:
//
//   - APPROVE does not write the profile here. The client applies the proposed
//     value into the settings document it already holds and PUTs it through
//     PUT /api/v1/settings — the same versioned, schema-validated,
//     shadow-fanned-out path every other settings edit takes — and only then
//     calls this route to record the decision. Approving therefore cannot
//     bypass validateProfile, and there is no second profile-write path to keep
//     in sync with the first.
//   - UNDO does write, because the change it reverses was made by the server on
//     the assistant's behalf and the client never saw the value it replaced.
//     It goes through store.UndoProfileSuggestion, which is the same versioned
//     read-modify-write the auto-apply used and which refuses any field outside
//     the locked auto-apply allowlist.
//
// Nothing here can apply a protected field (location / name / email): approve
// routes through the settings validator, undo routes through
// store.AutoApplyableProfileField, and neither accepts a value from the request
// body at all — the proposed value is read from the stored PROFSUGG# row.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// suggestionListLimit bounds the queue read. The producers are capped
// (12 pending from the tool, 10 analyses per UTC day) and rows carry a 30-day
// TTL, so this covers the realistic worst case with room to spare while keeping
// one Query bounded.
const suggestionListLimit = 60

// suggestionResponse is one row as the drawer renders it. It carries the
// human label alongside the raw dotted field so the client never has to keep
// its own copy of the field vocabulary, and `needsPick` so the UI knows the
// difference between "Approve" and "find this place" without re-deriving the
// location rule.
type suggestionResponse struct {
	ID            string `json:"id"`
	Field         string `json:"field"`
	FieldLabel    string `json:"fieldLabel"`
	CurrentValue  string `json:"currentValue"`
	ProposedValue string `json:"proposedValue"`
	Reason        string `json:"reason"`
	Source        string `json:"source"`
	SourceRef     string `json:"sourceRef,omitempty"`
	Status        string `json:"status"`
	AutoApplied   bool   `json:"autoApplied"`
	NeedsPick     bool   `json:"needsPick"`
	CreatedAt     string `json:"createdAt"`
}

// handleListProfileSuggestions returns everything still awaiting the owner:
// pending proposals, and auto-applied changes not yet kept or undone. Resolved
// rows are filtered out server-side — they exist only as an audit trail until
// their TTL, and the drawer has nothing to do with them.
func handleListProfileSuggestions(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		rows, err := deps.Store.ListProfileSuggestions(c.Context(), UserID(c), suggestionListLimit)
		if err != nil {
			return apiInternalError(c, deps, "list profile suggestions", err)
		}

		out := make([]suggestionResponse, 0, len(rows))
		pending, autoApplied := 0, 0
		for _, sg := range rows {
			if sg.ResolvedAt != "" {
				continue
			}
			if sg.AutoApplied {
				autoApplied++
			} else {
				pending++
			}
			out = append(out, suggestionResponse{
				ID:            sg.SuggID,
				Field:         sg.Field,
				FieldLabel:    store.ProfileFieldLabel(sg.Field),
				CurrentValue:  sg.CurrentValue,
				ProposedValue: sg.ProposedValue,
				Reason:        sg.Reason,
				Source:        sg.Source,
				SourceRef:     sg.SourceRef,
				Status:        sg.Status,
				AutoApplied:   sg.AutoApplied,
				NeedsPick:     store.LocationProfileField(sg.Field),
				CreatedAt:     sg.CreatedAt,
			})
		}
		return c.JSON(fiber.Map{
			"suggestions":      out,
			"pendingCount":     pending,
			"autoAppliedCount": autoApplied,
			// The badge counts both: an auto-applied change the owner has not
			// acknowledged is exactly as much "needs your eyes" as a proposal.
			"reviewCount": pending + autoApplied,
		})
	}
}

// The four decisions a client can record. approve/reject resolve a pending
// proposal; keep/undo resolve an auto-applied one.
const (
	suggestActionApprove = "approve"
	suggestActionReject  = "reject"
	suggestActionKeep    = "keep"
	suggestActionUndo    = "undo"
)

// handleResolveProfileSuggestion records the owner's decision on one row, and
// for `undo` also reverts the profile change that was auto-applied.
func handleResolveProfileSuggestion(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		suggID := strings.TrimSpace(c.Params("id"))
		if suggID == "" || len(suggID) > 64 || strings.Contains(suggID, "#") {
			return apiBadRequest(c, "a suggestion id is required")
		}

		var body struct {
			Action string `json:"action"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		action := strings.ToLower(strings.TrimSpace(body.Action))
		if !oneOf(action, suggestActionApprove, suggestActionReject, suggestActionKeep, suggestActionUndo) {
			return apiBadRequest(c, "action must be one of approve, reject, keep, undo")
		}

		// The stored row is the only source of the field and value. Nothing
		// about what gets written comes from the request body — a client can
		// only ever say "yes" or "no" to a proposal the server already holds.
		sg, err := deps.Store.FindProfileSuggestion(c.Context(), userID, suggID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apiNotFound(c)
			}
			return apiInternalError(c, deps, "find profile suggestion", err)
		}
		if sg.ResolvedAt != "" {
			return errorJSON(c, http.StatusConflict, "already_resolved",
				"That suggestion was already handled — reload to see the current list.")
		}
		// keep/undo only make sense for something that was actually applied,
		// and approve/reject only for something that was not. Crossing them
		// would let a client "undo" a change that never happened.
		if (action == suggestActionKeep || action == suggestActionUndo) != sg.AutoApplied {
			return apiBadRequest(c,
				"approve and reject apply to pending suggestions; keep and undo apply to changes that were made automatically")
		}

		status := store.SuggestionStatusApproved
		version := int64(0)
		switch action {
		case suggestActionReject:
			status = store.SuggestionStatusRejected
		case suggestActionUndo:
			status = store.SuggestionStatusRejected
			// Reverse the auto-apply BEFORE resolving. If the resolve then
			// loses its race the profile is still correct (reverted) and the
			// row stays unresolved, which the owner sees as "still offering
			// Undo" — annoying, not wrong. Resolving first and failing the
			// revert would leave the opposite: a row that says "handled" over
			// a change nobody wanted.
			v, uerr := deps.Store.UndoProfileSuggestion(c.Context(), userID, sg.Field, sg.ProposedValue, sg.CurrentValue)
			switch {
			case uerr == nil:
				version = v
			case errors.Is(uerr, store.ErrProfileFieldProtected), errors.Is(uerr, store.ErrProfileFieldUnknown):
				// A row that claims to have auto-applied a protected field
				// cannot have been written by this server. Refuse rather than
				// guess at what to restore.
				return apiBadRequest(c, "that change cannot be undone automatically — edit the field directly below")
			case errors.Is(uerr, store.ErrVersionConflict):
				return errorJSON(c, http.StatusConflict, "version_conflict",
					"Your settings changed while undoing. Reload and try again.")
			default:
				return apiInternalError(c, deps, "undo profile suggestion", uerr)
			}
		}

		if _, err := deps.Store.ResolveProfileSuggestion(c.Context(), userID, suggID, status, time.Now()); err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				return apiNotFound(c)
			case errors.Is(err, store.ErrSuggestionResolved):
				return errorJSON(c, http.StatusConflict, "already_resolved",
					"That suggestion was already handled — reload to see the current list.")
			default:
				return apiInternalError(c, deps, "resolve profile suggestion", err)
			}
		}

		resp := fiber.Map{"id": suggID, "action": action, "status": status}
		if version > 0 {
			// The undo wrote the settings document; the client adopts this
			// version so its next PUT does not 409 against its own revert.
			resp["version"] = version
		}
		return c.JSON(resp)
	}
}
