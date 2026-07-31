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
// code_update_start does NOT launch anything itself. It validates, enqueues, and
// returns in well under a second, because the Opus rewrite that follows takes
// 30–90 s and the web function's timeout is 30 s. The queue is also what makes
// the request survive the owner hanging up mid-sentence — see
// cmd/codeupdate-dispatch.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"

	"github.com/JeremyProffittOrg/live-ninja/internal/codeupdate"
	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
)

// defaultRepoLimit is how many repositories a bare code_update_repos returns.
// Twenty is the owner's number: enough that the app they mean is almost always
// in it, few enough that the model can hold them while they talk.
const defaultRepoLimit = 20

// maxCandidates bounds the disambiguation list handed back when a spoken name
// matches more than one repository. Reading six options aloud is already too
// many; four is the point where the owner re-words instead of listening.
const maxCandidates = 4

// ---------------------------------------------------------------------------
// code_update_repos
// ---------------------------------------------------------------------------

func codeUpdateReposDefinition() *Definition {
	return &Definition{
		Name: "code_update_repos",
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
		Name: "code_update_start",
		Description: "Start a coding session on one of the user's computers to update an " +
			"application. Confirm the repository AND what you are about to ask for out loud " +
			"first, then call this with confirm=true. The session runs immediately; the user " +
			"gets an email when it starts, progress emails while it works, and a summary when " +
			"it finishes.",
		SideEffecting: true,
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
				Description: "Which computer to run on. Defaults to the office PC."},
			{Name: "preprocess", Type: "boolean",
				Description: "Have Opus expand the instructions into a fuller brief before the " +
					"session starts. Defaults to TRUE — set false ONLY if the user says not to " +
					"rewrite or refine their wording."},
			{Name: "deploy", Type: "boolean",
				Description: "Allow the session to push its work, which deploys to production in " +
					"these repositories. Defaults to FALSE (commit locally only). Set true ONLY " +
					"if the user explicitly asked to deploy, ship, or release it."},
			{Name: "model", Type: "string", MaxLen: 128,
				Description: "Optional model override for the coding session."},
			{Name: "effort", Type: "string", MaxLen: 32,
				Description: "Optional reasoning-effort override for the coding session."},
			{Name: "confirm", Type: "boolean", Required: true,
				Description: "Set true only after you have told the user which repository and " +
					"what change you are about to start, and they agreed."},
		},
		Handler: handleCodeUpdateStart,
	}
}

func handleCodeUpdateStart(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Ghost == nil || !deps.Ghost.Ready() {
		return nil, toolErrf(CodeNotConfigured, "the code-update integration is not configured")
	}
	if deps.SQS == nil || deps.CodeUpdateQueueURL == "" {
		return nil, toolErrf(CodeNotConfigured, "the code-update queue is not configured")
	}

	// Starting a coding agent on the owner's machine is not something to infer
	// from an ambiguous sentence. The model must have said the repo and the
	// change out loud first; this is the same shape send_email's confirmation
	// gate uses.
	if confirmed, _ := args["confirm"].(bool); !confirmed {
		return nil, toolErrf(CodeConfirmationRequired,
			"tell the user which repository you are about to update and what you will ask for, "+
				"get their agreement, then call again with confirm=true")
	}

	repoArg, _ := args["repo"].(string)
	instructions, _ := args["instructions"].(string)

	// The repo must be one that actually exists. Resolving it against the live
	// listing is what stops a model-invented "owner/name" from reaching a
	// launch, and lets an almost-right name come back as candidates instead of
	// a flat failure.
	repos, err := deps.Ghost.ListRepos(ctx)
	if err != nil {
		return nil, ghostToolError(err)
	}
	repo, ok := ghost.Find(repos, repoArg)
	if !ok {
		if cands := ghost.Candidates(ghost.Rank(repos, repoArg), maxCandidates); len(cands) > 0 {
			names := make([]string, 0, len(cands))
			for _, c := range cands {
				names = append(names, c.Repo)
			}
			return nil, toolErrf(CodeNotFound,
				"no repository named %q; did you mean one of: %s? Ask the user which, then "+
					"call again with the exact name", repoArg, strings.Join(names, ", "))
		}
		return nil, toolErrf(CodeNotFound,
			"no repository named %q — call code_update_repos to see what is available", repoArg)
	}

	agent := codeupdate.DefaultCLI
	if v, ok := args["agent"].(string); ok && v != "" {
		agent = v
	}
	if !codeupdate.ValidCLI(agent) {
		return nil, toolErrf(CodeInvalidArgs, "agent must be one of: %s",
			strings.Join(codeupdate.SupportedCLIs, ", "))
	}

	node := codeupdate.DefaultNode
	if v, ok := args["node"].(string); ok && strings.TrimSpace(v) != "" {
		node = strings.TrimSpace(v)
	}

	// Preprocessing is ON unless the model explicitly turned it off, which is
	// what "use opus to pre-process the prompt, unless told not to" means. An
	// absent argument must therefore read as true, not as the zero value.
	preprocess := true
	if v, present := args["preprocess"]; present {
		if b, isBool := v.(bool); isBool {
			preprocess = b
		}
	}
	// Deploy is the opposite: absent means NO. A push to main is a production
	// deploy in these repositories, so it takes a spoken opt-in.
	deploy, _ := args["deploy"].(bool)

	requestID := uuid.Must(uuid.NewV7()).String()
	model, _ := args["model"].(string)
	effort, _ := args["effort"].(string)

	req := codeupdate.Request{
		Version:      codeupdate.QueueMessageVersion,
		RequestID:    requestID,
		UserID:       inv.UserID,
		SessionID:    inv.SessionID,
		Repo:         repo.Repo,
		Instructions: strings.TrimSpace(instructions),
		Node:         node,
		CLI:          agent,
		Model:        strings.TrimSpace(model),
		Effort:       strings.TrimSpace(effort),
		Preprocess:   preprocess,
		Deploy:       deploy,
		RequestedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	// The record lands BEFORE the queue message, so code_update_status can
	// always answer — a message that outran its own row would make a request the
	// owner just made look like it never happened.
	if deps.CodeUpdate != nil {
		if err := deps.CodeUpdate.Put(ctx, codeupdate.Record{
			RequestID: requestID,
			UserID:    inv.UserID,
			Status:    codeupdate.StatusQueued,
			Repo:      repo.Repo,
			Node:      node,
			CLI:       agent,
			Model:     req.Model,
			Deploy:    deploy,
			// The owner's own words, from the same trimmed value the queue
			// message carries — so the row and the request can never disagree
			// about what was asked for. See Record.Instructions for why.
			Instructions: req.Instructions,
		}); err != nil {
			deps.Log.Error("tools: code_update_start record write failed", "error", err.Error())
			return nil, toolErrf(CodeUpstreamError, "could not record the update request")
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, toolErrf(CodeUpstreamError, "could not prepare the update request")
	}
	if _, err := deps.SQS.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(deps.CodeUpdateQueueURL),
		MessageBody: aws.String(string(body)),
	}); err != nil {
		deps.Log.Error("tools: code_update_start enqueue failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "could not queue the update request")
	}

	return map[string]any{
		"status":     codeupdate.StatusQueued,
		"requestId":  requestID,
		"repo":       repo.Repo,
		"node":       node,
		"agent":      agent,
		"preprocess": preprocess,
		"deploy":     deploy,
		"note": "Queued. " + startedNote(preprocess, deploy) +
			" The user will get an email when the session starts, progress emails while it " +
			"works, and a summary when it finishes.",
	}, nil
}

// startedNote is what the model should say out loud. It states the two things
// the owner cannot see for themselves and would be surprised by.
func startedNote(preprocess, deploy bool) string {
	var parts []string
	if preprocess {
		parts = append(parts, "Opus is expanding the instructions first, so the session starts in about a minute")
	} else {
		parts = append(parts, "starting with the exact wording given")
	}
	if deploy {
		parts = append(parts, "and it IS authorized to deploy")
	} else {
		parts = append(parts, "and it will commit locally without deploying")
	}
	return strings.Join(parts, ", ") + "."
}

// ---------------------------------------------------------------------------
// code_update_status
// ---------------------------------------------------------------------------

func codeUpdateStatusDefinition() *Definition {
	return &Definition{
		Name: "code_update_status",
		Description: "Check how a code update is going. With no arguments this reports the most " +
			"recent one. Use when the user asks whether their update has started, what it is " +
			"doing, or whether it finished.",
		Params: []ParamSpec{
			{Name: "requestId", Type: "string", MaxLen: 64,
				Description: "Optional: the requestId returned by code_update_start. Omit for the most recent."},
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
