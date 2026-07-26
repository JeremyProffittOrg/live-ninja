package tools

// profile_suggest — the assistant's write door onto the Base Knowledge profile
// (M16 Knowledge Refinement Loop, plan.md WS-2).
//
// M15 made the profile the always-injected half of what the model knows (see
// internal/realtime/baseknowledge.go): it is in every session's instructions,
// and it is where get_weather's coordinates and the scheduler's timezone come
// from. That is exactly why the model may not write it. A wrong home location
// does not fail loudly — it silently poisons every weather and time answer
// afterwards, and nobody can tell from the conversation that it happened.
//
// So this tool PROPOSES. It writes a PROFSUGG# row (the same shape M17's RCA
// analyzer files, store.ProfileSuggestion) and returns a result that tells the
// model, in words, that nothing changed yet. The one exception is the locked
// auto-apply policy: units and an explicitly-spoken always-known fact may land
// immediately, because those are the two fields whose worst case is visible and
// trivially reversible — and even those come back with an undo the owner sees
// in Settings. Everything else waits for a human.
//
// The refusal is NOT implemented here. store.AutoApplyProfileSuggestion owns
// it, so a client that hand-crafts POST /api/v1/tools/invoke with
// autoApply=true on a location gets the same ErrProfileFieldProtected the model
// would (TestProfileSuggestRefusesAutoApplyForProtectedFields).

import (
	"context"
	"errors"
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

const (
	// maxSuggestReasonChars bounds the "why" the model attaches. It is shown
	// verbatim in the Settings drawer, so it has to be a sentence, not an essay.
	maxSuggestReasonChars = 300

	// maxPendingSuggestions caps how many undecided rows one user can
	// accumulate from this tool. Without it a model in a loop turns the
	// approval queue into a wall the owner will never read — and the queue's
	// whole value is that every row in it is worth a decision.
	maxPendingSuggestions = 12

	// suggestionScanLimit bounds the single-partition Query behind the
	// duplicate check and the pending cap. Comfortably above
	// maxPendingSuggestions plus the analyzer's own daily ceiling.
	suggestionScanLimit = 60
)

func profileSuggestDefinition() *Definition {
	return &Definition{
		Name: "profile_suggest",
		Description: "Propose a change to the user's always-known profile — the BASE KNOWLEDGE facts " +
			"listed in your instructions (name, pronouns, home or work location, preferred units, " +
			"email, locale, or a new always-true fact). Use it when the user states a STABLE fact " +
			"that should hold in every future conversation (\"I moved to Charlotte\", \"call me Jem\", " +
			"\"I prefer Celsius\"); use memory_write instead for episodic details tied to a moment. " +
			"This only QUEUES the change for the account owner to confirm in Settings — it does not " +
			"change the profile, so never tell the user the change is in effect unless the result " +
			"says applied:true.",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "field", Type: "string", Required: true, Enum: store.SuggestibleProfileFields,
				Description: "Which profile field to change. Use profile.notes[] to ADD a new " +
					"always-true fact (it never replaces the existing ones)."},
			{Name: "value", Type: "string", Required: true, MinLen: 1, MaxLen: store.MaxProfileNoteChars,
				Description: "The proposed value. For profile.units it must be \"imperial\" or " +
					"\"metric\"; for a location, the place name as the user said it (the owner picks " +
					"the exact place in Settings, which is what attaches real coordinates)."},
			{Name: "reason", Type: "string", Required: true, MinLen: 1, MaxLen: maxSuggestReasonChars,
				Description: "Why you are proposing this, in one sentence — ideally quoting what the " +
					"user said. The owner reads this to decide, so a vague reason gets rejected."},
			{Name: "autoApply", Type: "boolean",
				Description: "Set true ONLY when the user just explicitly told you to change this " +
					"(\"remember that I prefer metric\") AND the field is profile.units or " +
					"profile.notes[]. Any other field is queued for confirmation regardless of this " +
					"flag — names, emails and locations always require the owner in Settings."},
		},
		Handler: handleProfileSuggest,
	}
}

func handleProfileSuggest(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	field := args["field"].(string)
	reason := strings.TrimSpace(args["reason"].(string))
	autoApplyAsked, _ := args["autoApply"].(bool)
	deviceAutoApplyRefused := autoApplyAsked && inv.DeviceID != ""
	if deviceAutoApplyRefused {
		// AutoApplyProfileSuggestion is intentionally account-global. A
		// device-effective conversation must not silently write through to
		// every host, so leave this proposal pending for an explicit scoped
		// Settings choice.
		autoApplyAsked = false
	}

	// One validator for the value, shared with the apply path, so a proposal
	// the owner later approves can never be one the settings PUT rejects.
	value, err := store.NormalizeProfileSuggestionValue(field, args["value"].(string))
	if err != nil {
		return nil, toolErrf(CodeInvalidArgs, "%s", err.Error())
	}

	// Existing rows serve two purposes here: don't file the same proposal
	// twice, and don't let the queue grow past what a human will read.
	pending, terr := unresolvedSuggestions(ctx, deps, inv.UserID)
	if terr != nil {
		return nil, terr
	}
	for _, sg := range pending {
		if sg.Field == field && sg.ProposedValue == value {
			// Already waiting. Reporting this as success (rather than an
			// error) is deliberate: the assistant re-hearing a fact across
			// turns is normal, and an error here would make it apologise for
			// something that is already correctly queued.
			return map[string]any{
				"status":        "already_suggested",
				"suggestionId":  sg.SuggID,
				"field":         field,
				"proposedValue": value,
				"applied":       false,
				"note": "Already suggested and still waiting for confirmation in " +
					"Settings → About you. Nothing has changed yet.",
			}, nil
		}
	}
	if len(pending) >= maxPendingSuggestions {
		return nil, toolErrf(CodeForbidden,
			"there are already %d profile suggestions waiting to be reviewed in Settings; "+
				"ask the user to approve or dismiss those before suggesting more", len(pending))
	}

	profile := deps.profileForInvocation(ctx, inv)
	rec := &store.ProfileSuggestion{
		SuggID:        randHex(6), // 12 lowercase hex chars, server-generated — never model output
		Status:        store.SuggestionStatusPending,
		Field:         field,
		CurrentValue:  store.CurrentProfileValue(profile, field),
		ProposedValue: value,
		Reason:        reason,
		Source:        "assistant",
		SourceRef:     inv.SessionID,
	}

	// The auto-apply attempt. store.AutoApplyProfileSuggestion is the policy
	// gate AND the versioned write, so nothing here can route around it.
	applied := false
	autoApplyRefused := false
	previous := ""
	if autoApplyAsked {
		prev, changed, _, aerr := deps.Store.AutoApplyProfileSuggestion(ctx, inv.UserID, field, value)
		switch {
		case aerr == nil:
			applied = true
			previous = prev
			rec.Status = store.SuggestionStatusApproved
			rec.AutoApplied = true
			rec.CurrentValue = prev
			if !changed {
				// The profile already said this. There is nothing to undo and
				// nothing worth putting in the owner's queue.
				return map[string]any{
					"status":        "already_true",
					"field":         field,
					"proposedValue": value,
					"applied":       true,
					"note":          "The profile already had this, so nothing changed.",
				}, nil
			}
		case errors.Is(aerr, store.ErrProfileFieldProtected):
			// The locked policy, refused server-side. Queue it instead — the
			// proposal is still worth the owner's attention, it just cannot
			// take effect on the assistant's word.
			autoApplyRefused = true
		case errors.Is(aerr, store.ErrProfileNotesFull):
			return nil, toolErrf(CodeForbidden,
				"the always-known facts list is full (%d of %d); ask the user to remove one in "+
					"Settings → About you first", store.MaxProfileNotes, store.MaxProfileNotes)
		default:
			deps.Log.Error("tools: profile_suggest auto-apply failed",
				"field", field, "error", aerr.Error())
			return nil, toolErrf(CodeUpstreamError, "failed to update the profile")
		}
	}

	if err := deps.Store.PutProfileSuggestion(ctx, inv.UserID, rec); err != nil {
		// An applied change with no queue row is still a real, correct change
		// — the owner just loses the undo button. Say so rather than reporting
		// a failure the model would then "retry" into a second apply.
		if applied {
			deps.Log.Error("tools: profile_suggest applied but could not queue the undo row",
				"field", field, "error", err.Error())
			return map[string]any{
				"status":        "applied",
				"field":         field,
				"proposedValue": value,
				"applied":       true,
				"note": "Applied to the profile. It could not be added to the Settings review list, " +
					"so say that it can be corrected in Settings → About you if it is wrong.",
			}, nil
		}
		deps.Log.Error("tools: profile_suggest queue write failed", "field", field, "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "failed to queue the suggestion")
	}

	if applied {
		return map[string]any{
			"status":        "applied",
			"suggestionId":  rec.SuggID,
			"field":         field,
			"previousValue": previous,
			"proposedValue": value,
			"applied":       true,
			"note": "Applied now, and it can be undone from Settings → About you. " +
				"You may tell the user it is set.",
		}, nil
	}

	out := map[string]any{
		"status":        "suggested",
		"suggestionId":  rec.SuggID,
		"field":         field,
		"currentValue":  rec.CurrentValue,
		"proposedValue": value,
		"applied":       false,
		"note": "Suggested — Jeremy will confirm it in Settings → About you. NOTHING has changed " +
			"yet: tell the user you have suggested it for confirmation, and do not say it is saved " +
			"or in effect.",
	}
	if autoApplyRefused {
		out["autoApplyRefused"] = true
		out["note"] = "Suggested, but it cannot be applied automatically: " +
			store.ProfileFieldLabel(field) + " always needs confirmation in Settings → About you, " +
			"because a wrong value there would quietly affect every future answer. Tell the user " +
			"you have suggested it and that they need to confirm it."
	}
	if deviceAutoApplyRefused {
		out["autoApplyRefused"] = true
		out["note"] = "Suggested, but it was not applied automatically because this conversation " +
			"uses device-specific About you settings. Review it in Settings and choose this device, " +
			"selected devices, or all devices."
	}
	if store.LocationProfileField(field) {
		out["note"] = "Suggested — it is confirmed in Settings → About you by picking the exact " +
			"place from the search results, which is what stores real coordinates. Nothing has " +
			"changed yet; do not say the location is set."
	}
	return out, nil
}

// unresolvedSuggestions returns the user's rows that still await a decision:
// pending proposals plus auto-applied changes not yet kept or undone. A
// single-partition Query, bounded — never a Scan.
func unresolvedSuggestions(ctx context.Context, deps *Deps, userID string) ([]store.ProfileSuggestion, *ToolError) {
	all, err := deps.Store.ListProfileSuggestions(ctx, userID, suggestionScanLimit)
	if err != nil {
		deps.Log.Error("tools: profile_suggest queue read failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "failed to read the existing suggestions")
	}
	out := make([]store.ProfileSuggestion, 0, len(all))
	for _, sg := range all {
		if sg.ResolvedAt == "" {
			out = append(out, sg)
		}
	}
	return out, nil
}
