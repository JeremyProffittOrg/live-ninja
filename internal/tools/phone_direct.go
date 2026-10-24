package tools

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// PhoneScope is a call-bound capability, never a general owner login. Only
// the IAM-invoked realtime broker mints it, after it proves the canonical
// active owner for an admitted owner call.
const PhoneScope = "phone"

// Owner decision 2026-10-22: phone calls have the same server tools as every
// other path. Device-local tools still require a client that executes them.
// Authorization and each tool's confirmation behavior remain in the registry.
func PhoneToolAllowed(name string) bool {
	for _, def := range definitions() {
		if def.Name == name {
			return !def.DeviceLocal
		}
	}
	return false
}

// CatalogManifestForPhone shares the server catalog, including its complete
// argument schemas and tool descriptions. No second allowlist can drift.
func CatalogManifestForPhone() []map[string]any {
	return CatalogManifestForServerExecution()
}

// phoneOwnerCall is true only for the verified phone-scoped call token. The
// registry has already refused a phone-scoped invocation by a non-owner.
func phoneOwnerCall(inv Invocation) bool {
	return inv.Scope == PhoneScope && inv.Surface == "phone"
}

// phoneConfirmParam is honored only on an authenticated owner phone call.
func phoneConfirmParam() ParamSpec {
	return ParamSpec{Name: "confirm", Type: "boolean",
		Description: "Owner phone calls only: set true after you read this exact change back and the owner said yes on this call. " +
			"Ignored everywhere else, where the signed-in app review applies."}
}

// phoneConfirmed returns nil when the owner confirmed this exact call, else
// the confirmation error. The idempotency claim is released for it, so the
// confirmed retry (a new call id) is not reported as a duplicate.
func phoneConfirmed(args map[string]any, operation string, proposed map[string]any) *ToolError {
	if confirmed, _ := args["confirm"].(bool); confirmed {
		return nil
	}
	return &ToolError{Code: CodeConfirmationRequired,
		Message: "Nothing was changed yet. Read this exact change back to the owner. Only after they say yes on this call, call the tool again with confirm=true.",
		Details: map[string]any{"operation": operation, "proposed": proposed, "confirmOnCall": true}}
}

// phoneRequestID binds the service-level request receipt to the registry's
// idempotency key, so a redelivered call cannot repeat the change.
func phoneRequestID(inv Invocation) (string, *ToolError) {
	if len(inv.IdempotencyKey) < 8 || len(inv.IdempotencyKey) > 128 {
		return "", toolErrf(CodeInvalidArgs, "idempotencyKey must contain 8 to 128 characters")
	}
	return inv.IdempotencyKey, nil
}

func phoneJobCreate(ctx context.Context, d *Deps, inv Invocation, args map[string]any, in jobs.Input, proposed map[string]any) (map[string]any, *ToolError) {
	if e := phoneConfirmed(args, "job_create", proposed); e != nil {
		return nil, e
	}
	requestID, e := phoneRequestID(inv)
	if e != nil {
		return nil, e
	}
	j, err := d.Jobs.Create(ctx, inv.UserID, in, requestID)
	if err != nil {
		return nil, jobsToolError(err)
	}
	return map[string]any{"status": "created", "job": j, "jobsUrl": "/jobs"}, nil
}

func phoneJobAction(ctx context.Context, d *Deps, inv Invocation, args map[string]any, action string, j *jobs.Job, run *jobs.Run, proposed map[string]any) (map[string]any, *ToolError) {
	if e := phoneConfirmed(args, "job_"+action, proposed); e != nil {
		return nil, e
	}
	requestID, e := phoneRequestID(inv)
	if e != nil {
		return nil, e
	}
	out := map[string]any{"status": "applied", "operation": "job_" + action, "jobsUrl": "/jobs"}
	var err error
	switch action {
	case "start":
		var started *jobs.Run
		started, err = d.Jobs.RunNow(ctx, inv.UserID, j.ID, j.Version, requestID)
		out["run"] = started
	case "pause", "resume", "cancel":
		var changed *jobs.Job
		changed, err = d.Jobs.Action(ctx, inv.UserID, j.ID, action, j.Version, requestID)
		out["job"] = changed
	case "retry":
		var retried *jobs.Run
		retried, err = d.Jobs.RetryRun(ctx, inv.UserID, j.ID, run.ID, j.Version, requestID)
		out["run"] = retried
	case "command":
		note := jobs.NoteInput{Kind: "note", Text: proposed["text"].(string)}
		if run != nil {
			note.RunID = run.ID
		}
		var entry *jobs.HistoryEntry
		var changed *jobs.Job
		entry, changed, err = d.Jobs.AddNote(ctx, inv.UserID, j.ID, note, j.Version, requestID)
		out["entry"], out["job"] = entry, changed
	default:
		return nil, toolErrf(CodeInvalidArgs, "unsupported job action %q", action)
	}
	if err != nil {
		return nil, jobsToolError(err)
	}
	return out, nil
}

// phoneRuleChange applies a confirmed rule save or delete through the same
// single-use, version-checked ApplyRuleReview the signed-in review uses.
func phoneRuleChange(ctx context.Context, d *Deps, inv Invocation, args map[string]any, operation string, proposed map[string]string) (map[string]any, *ToolError) {
	shown := make(map[string]any, len(proposed))
	for k, v := range proposed {
		shown[k] = v
	}
	if e := phoneConfirmed(args, "rule_"+operation, shown); e != nil {
		return nil, e
	}
	if _, e := phoneRequestID(inv); e != nil {
		return nil, e
	}
	name := proposed["name"]
	review := store.RuleReview{
		RequestID:  uuid.NewSHA1(uuid.NameSpaceOID, []byte("live-ninja/phone-rule\x00"+inv.UserID+"\x00"+inv.IdempotencyKey)).String(),
		ReviewedAt: d.Now().UTC().Format(time.RFC3339Nano),
		Operation:  operation,
		Proposed:   store.Rule{Name: name},
	}
	existing, err := d.Store.GetRule(ctx, inv.UserID, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		existing = nil
	case err != nil:
		return nil, toolErrf(CodeUpstreamError, "failed to read the rule; nothing was changed")
	}
	if existing != nil && existing.Version > 0 {
		review.ExpectedVersion = existing.Version
		review.ExpectedUpdatedAt = existing.UpdatedAt
		review.ExpectedRule = &store.RuleReviewSnapshot{Description: existing.Description, Body: existing.Body, Enabled: existing.Enabled}
	}
	if operation == "delete" && review.ExpectedVersion == 0 {
		return nil, toolErrf(CodeNotFound, "no saved rule named %q; call rule_list to see the saved rules", name)
	}
	if operation == "save" {
		review.Proposed.Description = proposed["description"]
		review.Proposed.Body = proposed["body"]
		review.Proposed.Enabled = existing == nil || existing.Version == 0 || existing.Enabled
	}
	result, err := d.Store.ApplyRuleReview(ctx, inv.UserID, review)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidRule):
			return nil, toolErrf(CodeInvalidArgs, "%s", err.Error())
		case errors.Is(err, store.ErrRuleLimit):
			return nil, toolErrf(CodeInvalidArgs, "Delete a rule before adding another.")
		case errors.Is(err, store.ErrRuleReviewAlreadyApplied):
			return map[string]any{"status": "duplicate", "message": "this change was already applied"}, nil
		case errors.Is(err, store.ErrRuleReviewConflict):
			return nil, toolErrf(CodeInvalidArgs, "The rule changed during the call. Load it again and confirm the current version.")
		case errors.Is(err, store.ErrRuleReviewForbidden):
			return nil, toolErrf(CodeForbidden, "This account can no longer change rules.")
		default:
			return nil, toolErrf(CodeUpstreamError, "the rule change could not be saved")
		}
	}
	return map[string]any{"status": "applied", "operation": "rule_" + operation, "result": result}, nil
}
