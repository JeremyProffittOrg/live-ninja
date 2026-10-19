package tools

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// PhoneScope is a call-bound capability, never a general owner login. Only
// the IAM-invoked realtime broker mints it, after it proves the canonical
// active owner for an admitted owner call.
const PhoneScope = "phone"

// Owner decision 2026-10-19 ("delivery action, live call can aporove
// directly"): an authenticated owner phone call may run account actions
// itself. The phone has no review UI, so tools marked PhoneDirect execute
// after the owner's spoken confirmation instead of returning an app-review
// proposal. Excluded on purpose: device-local controls (camera, volume,
// microphone/session), device_control, call_message (it places a new
// outgoing call), code_update_start (no launch path exists on any surface),
// profile_suggest (Settings review), the long-running web tools, and file,
// deliverable and code-update tools that a phone call cannot show.
//
// Each phone tool carries a short phone description. A call's model budget
// is fixed (cmd/phone-trial-bridge), and every response re-sends the tool
// schema, so the phone manifest stays compact.
var phoneTools = map[string]string{
	// Read.
	"memory_search":        "Search the owner's saved memory by meaning. Use before answering about personal facts.",
	"entity_get":           "Read one memory entity by ID.",
	"knowledge_search":     "Search the owner's knowledge store: loaded documents, sessions, repositories, email and pages.",
	"knowledge_recent":     "List the newest items from one knowledge source.",
	"recall_note":          "Read the owner's saved notes, newest first.",
	"rule_list":            "List the owner's saved rules.",
	"rule_load":            "Read one saved rule.",
	"job_list":             "List the owner's saved Jobs.",
	"job_status":           "Read one saved job with its runs and history.",
	"get_weather":          "Current weather and a short forecast. Omit location for where the owner is.",
	"phone_message_status": "Check an owner phone-message request by requestId.",
	// Account actions that already execute directly on every surface.
	"memory_write":         "Save or update a lasting memory entity when the owner asks.",
	"forget":               "Permanently delete one memory entity when the owner asks.",
	"remember_note":        "Save a note for the owner.",
	"set_timer":            "Set a one-shot timer of up to 24 hours.",
	"set_reminder":         "Set an email reminder for a date and time.",
	"set_current_location": "Record where the owner is now, or home=true when back home.",
	"send_email":           "Send an email; without 'to' it goes to the owner. Another address needs the owner's yes and confirmExternal=true.",
	// Review-proposal tools that execute directly on an owner phone call.
	"job_create":  "Create a reminder or review job.",
	"job_start":   "Start a saved job now.",
	"job_pause":   "Pause a saved job.",
	"job_resume":  "Resume a paused job.",
	"job_cancel":  "Cancel a saved job.",
	"job_retry":   "Retry one failed or cancelled run.",
	"job_command": "Add the owner's note to a saved job.",
	"rule_save":   "Create or change one saved rule.",
	"rule_delete": "Delete one saved rule.",
}

// PhoneToolAllowed reports whether an owner phone session may call name.
func PhoneToolAllowed(name string) bool { _, ok := phoneTools[name]; return ok }

const phoneDirectSuffix = " On this call it applies directly: read the exact change back, and only after the owner says yes call again with confirm=true."

// CatalogManifestForPhone renders the phone tool set in catalog order with
// the compact phone descriptions. Parameter schemas are the enforced ones;
// only their descriptions are shortened to the first sentence.
func CatalogManifestForPhone() []map[string]any {
	defs := make([]*Definition, 0, len(phoneTools))
	for _, def := range definitions() {
		text, ok := phoneTools[def.Name]
		if !ok || def.DeviceLocal {
			continue
		}
		short := *def
		short.Description = text
		if def.PhoneDirect {
			short.Description += phoneDirectSuffix
		}
		short.Params = make([]ParamSpec, len(def.Params))
		for i, p := range def.Params {
			if p.Name == "confirm" && def.PhoneDirect {
				p.Description = "True only after the owner said yes to this exact change on this call."
			} else {
				p.Description = firstSentence(p.Description)
			}
			short.Params[i] = p
		}
		defs = append(defs, &short)
	}
	return renderManifest(defs)
}

func firstSentence(text string) string {
	for i := 0; i+1 < len(text); i++ {
		if text[i] == '.' && text[i+1] == ' ' && !strings.HasSuffix(text[:i], "e.g") && !strings.HasSuffix(text[:i], "i.e") {
			text = text[:i+1]
			break
		}
	}
	if runes := []rune(text); len(runes) > 120 {
		cut := strings.LastIndex(string(runes[:120]), " ")
		if cut < 60 {
			return string(runes[:120])
		}
		text = string(runes[:120])[:cut]
	}
	return text
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
