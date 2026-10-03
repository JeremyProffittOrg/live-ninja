package tools

// The voice-driven code-update tools (code_update_repos / code_update_start /
// code_update_status).
//
// The interaction they are shaped for is the one the owner actually has:
//
//	"update an application"        → code_update_repos()      the 20 most recent
//	"live ninja"                   → matched locally, or code_update_repos(query)
//	"<what to change>"             → read back, then code_update_start(...)
//	"how's that going?"            → code_update_status()
//
// code_update_start only returns a review proposal. There is no trusted,
// user-bound launch approval connected to this tool yet, so model arguments
// cannot launch a coding agent. Existing records remain readable by status.

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/JeremyProffittOrg/live-ninja/internal/codeupdate"
	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
)

// defaultRepoLimit is how many repositories a bare code_update_repos returns.
// Twenty is the owner's number: enough that the app they mean is almost always
// in it, few enough that the model can hold them while they talk.
const defaultRepoLimit = 20

// ---------------------------------------------------------------------------
// code_update_repos
// ---------------------------------------------------------------------------

func codeUpdateReposDefinition() *Definition {
	return &Definition{
		Name:      "code_update_repos",
		OwnerOnly: true,
		Description: "List the user's GitHub repositories so they can pick which application to " +
			"update. With no query this returns the 20 most recently worked-on repositories — " +
			"start here when the user asks to update an application. Pass query to search the " +
			"user's FULL repository list when the app they name is not among those 20, or when " +
			"you need to check which of several similar names they mean.",
		Params: []ParamSpec{
			{Name: "query", Type: "string", MaxLen: 100,
				Description: "Optional: the application name the user said, e.g. 'live ninja'. " +
					"Searches every repository, not just the recent ones."},
			{Name: "limit", Type: "integer", Min: floatPtr(1), Max: floatPtr(50),
				Description: "How many to return (default 20)."},
		},
		Handler: handleCodeUpdateRepos,
	}
}

func handleCodeUpdateRepos(ctx context.Context, deps *Deps, _ Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Ghost == nil || !deps.Ghost.Ready() {
		return nil, toolErrf(CodeNotConfigured, "the code-update integration is not configured")
	}

	repos, err := deps.Ghost.ListRepos(ctx)
	if err != nil {
		return nil, ghostToolError(err)
	}

	// registry.go's validateArgs coerces an "integer" param to a Go int before the
	// handler runs, so this MUST assert int — asserting float64 (what raw JSON
	// would give) silently never matches and pins the limit at 20 forever. Every
	// other handler in this package reads integers this way; this one was the
	// outlier, and a unit test that called the handler directly with a float64
	// hid it.
	limit := defaultRepoLimit
	if v, ok := args["limit"].(int); ok && v > 0 {
		limit = v
	}

	// `matched` is the FULL candidate set, deliberately not pre-trimmed to the
	// limit: `truncated` has to mean "there were more matches than I showed you",
	// which is what tells the model to ask the owner to narrow it rather than
	// assume the list was exhaustive. Slicing before counting made the flag
	// permanently false on the search path.
	query, _ := args["query"].(string)
	searched := strings.TrimSpace(query) != ""
	matched := repos
	if searched {
		ranked := ghost.Rank(repos, query)
		matched = ghost.Candidates(ranked, len(ranked))
	}

	shown := matched
	if len(shown) > limit {
		shown = shown[:limit]
	}
	out := make([]map[string]any, 0, len(shown))
	for _, r := range shown {
		out = append(out, map[string]any{"repo": r.Repo, "name": r.Name, "owner": r.Owner})
	}

	return map[string]any{
		"repos":     out,
		"total":     len(repos),
		"matched":   len(matched),
		"searched":  searched,
		"truncated": len(matched) > len(out),
	}, nil
}

// ---------------------------------------------------------------------------
// code_update_start
// ---------------------------------------------------------------------------

func codeUpdateStartDefinition() *Definition {
	return &Definition{
		Name:      "code_update_start",
		OwnerOnly: true,
		Description: "Prepare a coding-job proposal for the account owner to review. This tool " +
			"does not queue or start work: trusted user-bound launch approval is not connected " +
			"yet. No model argument, including confirm=true, authorizes execution or deployment. " +
			"Return the proposed repository, computer and instructions, and explain that nothing " +
			"was started. Use code_update_status only to inspect existing runs.",
		Params: []ParamSpec{
			{Name: "repo", Type: "string", Required: true, MinLen: 3, MaxLen: 140,
				Description: "The exact 'owner/name' from code_update_repos. Never invent one."},
			{Name: "instructions", Type: "string", Required: true, MinLen: 10,
				MaxLen: codeupdate.MaxInstructionChars,
				Description: "What the user wants changed, in their own words plus any detail " +
					"they gave. Be specific and complete — this is the whole brief."},
			{Name: "agent", Type: "string", Enum: codeupdate.SupportedCLIs,
				Description: "Which coding CLI to run: claude (default) or codex. Only change " +
					"this if the user asks for it by name."},
			{Name: "node", Type: "string", MaxLen: 128,
				Description: "Proposed computer. Defaults to the office PC; no connection or availability is verified."},
			{Name: "preprocess", Type: "boolean",
				Description: "Propose refining the instructions before a future approved launch. " +
					"Defaults to true; no refinement is performed by this proposal."},
			{Name: "deploy", Type: "boolean",
				Description: "Legacy argument. Deployment cannot be authorized here. " +
					"Every proposal has deploy=false and nothing is queued or started."},
			{Name: "model", Type: "string", MaxLen: 128,
				Description: "Optional model override for the coding session."},
			{Name: "effort", Type: "string", MaxLen: 32,
				Description: "Optional reasoning-effort override for the coding session."},
			{Name: "confirm", Type: "boolean",
				Description: "Legacy argument, ignored. Model-supplied confirmation cannot " +
					"authorize a coding agent launch."},
		},
		Handler: handleCodeUpdateStart,
	}
}

// This handler deliberately has no dependency access. Restoring a launch path
// requires a separately reviewed, user-bound approval contract, not a new flag
// or an environment-variable escape hatch. The normal user audit is retained.
func handleCodeUpdateStart(_ context.Context, _ *Deps, _ Invocation, args map[string]any) (map[string]any, *ToolError) {
	repo, _ := args["repo"].(string)
	repo = strings.TrimSpace(repo)
	owner, name, found := strings.Cut(repo, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") || len(strings.Fields(repo)) != 1 {
		return nil, toolErrf(CodeInvalidArgs, "repo must be the exact owner/name from code_update_repos")
	}
	instructions, _ := args["instructions"].(string)
	instructions = strings.TrimSpace(instructions)
	if utf8.RuneCountInString(instructions) < 10 || utf8.RuneCountInString(instructions) > codeupdate.MaxInstructionChars {
		return nil, toolErrf(CodeInvalidArgs, "instructions must contain 10 to %d characters", codeupdate.MaxInstructionChars)
	}
	agent := codeupdate.DefaultCLI
	if value, ok := args["agent"].(string); ok && value != "" {
		agent = value
	}
	if !codeupdate.ValidCLI(agent) {
		return nil, toolErrf(CodeInvalidArgs, "agent must be one of: %s", strings.Join(codeupdate.SupportedCLIs, ", "))
	}
	node := codeupdate.DefaultNode
	if value, ok := args["node"].(string); ok && strings.TrimSpace(value) != "" {
		node = strings.TrimSpace(value)
	}
	preprocess := true
	if value, ok := args["preprocess"].(bool); ok {
		preprocess = value
	}
	proposed := map[string]any{
		"repo": repo, "node": node, "instructions": instructions,
		"agent": agent, "preprocess": preprocess, "deploy": false,
	}
	for _, key := range []string{"model", "effort"} {
		if value, ok := args[key].(string); ok && strings.TrimSpace(value) != "" {
			proposed[key] = strings.TrimSpace(value)
		}
	}
	return nil, &ToolError{
		Code: CodeConfirmationRequired,
		Message: "No coding job was queued or started. This tool only prepares a proposal. " +
			"Trusted user-bound launch approval is not connected yet; confirm=true cannot authorize " +
			"execution. Deployment is unavailable through this tool. Do not report this proposal as running.",
		Details: map[string]any{
			"operation": "propose_code_update", "executionAvailable": false,
			"launchApproval": "not_connected", "repositoryVerified": false, "nodeVerified": false,
			"proposed": proposed,
		},
	}
}

// ---------------------------------------------------------------------------
// code_update_status
// ---------------------------------------------------------------------------

func codeUpdateStatusDefinition() *Definition {
	return &Definition{
		Name:      "code_update_status",
		OwnerOnly: true,
		Description: "Check how a code update is going. With no arguments this reports the most " +
			"recent one. Use when the user asks whether their update has started, what it is " +
			"doing, or whether it finished.",
		Params: []ParamSpec{
			{Name: "requestId", Type: "string", MaxLen: 64,
				Description: "Optional: an existing coding run requestId. Omit for the most recent. Proposals have no requestId."},
		},
		Handler: handleCodeUpdateStatus,
	}
}

func handleCodeUpdateStatus(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.CodeUpdate == nil {
		return nil, toolErrf(CodeNotConfigured, "the code-update integration is not configured")
	}

	var (
		rec codeupdate.Record
		err error
	)
	if id, _ := args["requestId"].(string); strings.TrimSpace(id) != "" {
		rec, err = deps.CodeUpdate.Get(ctx, inv.UserID, strings.TrimSpace(id))
	} else {
		rec, err = deps.CodeUpdate.Latest(ctx, inv.UserID)
	}
	switch {
	case errors.Is(err, codeupdate.ErrNotFound):
		return nil, toolErrf(CodeNotFound, "no code update was found for this account")
	case err != nil:
		deps.Log.Error("tools: code_update_status read failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "could not read the update status")
	}

	out := map[string]any{
		"requestId": rec.RequestID,
		"status":    rec.Status,
		"repo":      rec.Repo,
		"node":      rec.Node,
		"agent":     rec.CLI,
		"deploy":    rec.Deploy,
		"rewritten": rec.Rewritten,
		"createdAt": rec.CreatedAt,
		"updatedAt": rec.UpdatedAt,
	}
	if rec.RewriteNote != "" {
		out["rewriteNote"] = rec.RewriteNote
	}
	if rec.Error != "" {
		out["error"] = rec.Error
	}

	// Once it is launched the interesting answer lives on ghost-cli's side: is
	// the session still running, and did it leave a summary?
	if rec.Status == codeupdate.StatusLaunched && rec.RunID != "" {
		out["runId"] = rec.RunID
		if deps.Ghost != nil && deps.Ghost.Ready() {
			if _, run, ferr := deps.Ghost.FindRun(ctx, rec.EventID, rec.RunID, rec.RequestID); ferr == nil {
				out["runStatus"] = run.Status
				if run.Summary != "" {
					out["summary"] = run.Summary
				}
			}
		}
	}
	return out, nil
}

// ghostToolError maps a ghost client error onto the tool-router vocabulary,
// with wording a voice model can read aloud without further translation.
func ghostToolError(err error) *ToolError {
	switch {
	case errors.Is(err, ghost.ErrNotConfigured):
		return toolErrf(CodeNotConfigured, "the code-update integration is not configured")
	case errors.Is(err, ghost.ErrNotAuthorized):
		return toolErrf(CodeForbidden,
			"the fleet service refused the request; Live Ninja is not authorized to launch there yet")
	case errors.Is(err, ghost.ErrQuota):
		return toolErrf(CodeUpstreamError,
			"the prompt-refinement quota is used up for the moment; try again in a few minutes")
	case errors.Is(err, ghost.ErrUnavailable):
		return toolErrf(CodeNotConfigured, "the fleet service is not able to launch sessions right now")
	case errors.Is(err, ghost.ErrNotFound):
		return toolErrf(CodeNotFound, "the fleet service has no record of that")
	default:
		return toolErrf(CodeUpstreamError, "could not reach the fleet service")
	}
}
