package tools

// ghost_run_start / ghost_run_status — start a coding run on a chosen computer
// and repository, and read it back.
//
// ghost_run_start goes through ghost-cli's schedule launch path (POST
// /schedule with run_now), the same route cmd/codeupdate-dispatch uses, with
// the same LaunchRequest (internal/codeupdate/dispatch.go) and the same prompt
// assembly (codeupdate.BuildPrompt). What it deliberately does NOT do:
//
//   - no Opus preprocessing: the owner's words are sent as spoken, so the
//     read-back at confirmation IS what the agent receives;
//   - no progress token: that block needs the CODEUPD# token row and the
//     progress endpoint cmd/codeupdate-dispatch owns, so BuildPrompt omits it
//     when Token is empty (prompt.go) and the run reports through the normal
//     ghost summary email instead.
//
// Authority: the tool is OwnerOnly and SideEffecting. Without confirm=true it
// returns confirmation_required with the exact proposal AND a server-issued
// proposalId bound to those arguments (ghost_confirm.go); the registry
// releases the idempotency claim (idempotencyReleasable), so the confirmed
// retry with a fresh key proceeds. The confirmed call must present that
// proposalId with the same arguments: a lone confirm=true, from any surface,
// never launches. A redelivered confirmed call is a duplicate, not a second
// launch.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JeremyProffittOrg/live-ninja/internal/codeupdate"
	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
)

const ghostRunNoteNotRewritten = "not requested — launched with your original wording"

func ghostRunStartDefinition() *Definition {
	return &Definition{
		Name:          "ghost_run_start",
		OwnerOnly:     true,
		SideEffecting: true,
		Description: "Start a coding-agent run on one of the owner's ghost computers in one of their " +
			"repositories, ONLY when the owner explicitly asks. First call without confirm: the " +
			"tool checks the repository and computer exist and returns confirmation_required with " +
			"the exact proposal in error.details.proposed and a proposalId. Read it back — " +
			"repository, computer, agent, and whether it may deploy — and only after the owner says " +
			"yes call again with the SAME arguments, confirm=true, that proposalId and a NEW " +
			"idempotencyKey. confirm=true without the proposalId, or with changed arguments, is " +
			"refused and launches nothing. deploy defaults to true (the owner's standing " +
			"decision: confirmed work ships); set deploy=false when they say to hold it or not " +
			"push. Instructions are sent as spoken, without rewriting. A launched run is not a " +
			"finished one: use ghost_run_status to check it.",
		Params: []ParamSpec{
			{Name: "repo", Type: "string", Required: true, MinLen: 3, MaxLen: 140,
				Description: "The exact 'owner/name' from code_update_repos. Never invent one."},
			{Name: "instructions", Type: "string", Required: true, MinLen: 10, MaxLen: codeupdate.MaxInstructionChars,
				Description: "What the owner wants done, in their own words plus any detail they gave. This is the whole brief."},
			{Name: "node", Type: "string", MaxLen: 128,
				Description: "Computer name from ghost_nodes. Defaults to " + codeupdate.DefaultNode + "."},
			{Name: "agent", Type: "string", Enum: codeupdate.SupportedCLIs,
				Description: "Which coding CLI to run: claude (default) or codex. Only change this if the owner names one."},
			{Name: "model", Type: "string", MaxLen: 128, Description: "Optional model override for the coding session."},
			{Name: "effort", Type: "string", MaxLen: 32, Description: "Optional reasoning-effort override for the coding session."},
			{Name: "deploy", Type: "boolean",
				Description: "Whether the run may push (a push to main is a production deploy). Default true; false when the owner says to hold or not push."},
			{Name: "confirm", Type: "boolean",
				Description: "True only after you read the exact proposal back and the owner said yes. Send it with the proposalId from that reply and a new idempotencyKey."},
			ghostProposalParam(),
		},
		Handler: handleGhostRunStart,
	}
}

func handleGhostRunStart(ctx context.Context, d *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if terr := requireGhost(d); terr != nil {
		return nil, terr
	}
	repo, _ := args["repo"].(string)
	repo = strings.TrimSpace(repo)
	owner, name, found := strings.Cut(repo, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") || len(strings.Fields(repo)) != 1 {
		return nil, toolErrf(CodeInvalidArgs, "repo must be the exact owner/name from code_update_repos")
	}
	instructions, _ := args["instructions"].(string)
	instructions = strings.TrimSpace(instructions)
	if n := utf8.RuneCountInString(instructions); n < 10 || n > codeupdate.MaxInstructionChars {
		return nil, toolErrf(CodeInvalidArgs, "instructions must contain 10 to %d characters", codeupdate.MaxInstructionChars)
	}
	agent := codeupdate.DefaultCLI
	if v, ok := args["agent"].(string); ok && v != "" {
		agent = v
	}
	if !codeupdate.ValidCLI(agent) {
		return nil, toolErrf(CodeInvalidArgs, "agent must be one of: %s", strings.Join(codeupdate.SupportedCLIs, ", "))
	}
	nodeArg := codeupdate.DefaultNode
	if v, ok := args["node"].(string); ok && strings.TrimSpace(v) != "" {
		nodeArg = strings.TrimSpace(v)
	}
	deploy := true
	if v, ok := args["deploy"].(bool); ok {
		deploy = v
	}
	model, _ := args["model"].(string)
	effort, _ := args["effort"].(string)
	model, effort = strings.TrimSpace(model), strings.TrimSpace(effort)

	// The repository must be one ghost-cli's launcher credential can reach.
	repos, err := d.Ghost.ListRepos(ctx)
	if err != nil {
		return nil, ghostFleetError(err)
	}
	matched := ""
	for _, r := range repos {
		if r.Repo == repo {
			matched = r.Repo
			break
		}
	}
	if matched == "" {
		for _, r := range repos {
			if strings.EqualFold(r.Repo, repo) {
				matched = r.Repo
				break
			}
		}
	}
	if matched == "" {
		return nil, toolErrf(CodeNotFound, "%q is not one of the launchable repositories; call code_update_repos to find the exact name", repo)
	}
	repo = matched

	node, _, terr := ghostRequireNode(ctx, d, inv, nodeArg)
	if terr != nil {
		return nil, terr
	}

	proposed := map[string]any{
		"repo": repo, "node": node.NodeID, "nodeOnline": ghostNodeOnline(node),
		"agent": agent, "deploy": deploy, "instructions": instructions, "preprocess": false,
	}
	if model != "" {
		proposed["model"] = model
	}
	if effort != "" {
		proposed["effort"] = effort
	}
	// The confirmation is bound to exactly these values (ghost_confirm.go);
	// nodeOnline is informational and may change between the two calls.
	binding := map[string]any{
		"repo": repo, "node": node.NodeID, "agent": agent, "deploy": deploy,
		"instructions": instructions, "model": model, "effort": effort,
	}
	if confirmed, _ := args["confirm"].(bool); !confirmed {
		return nil, ghostIssueProposal(ctx, d, inv, "ghost_run_start",
			"Nothing was started. Read this exact run back to the owner — repository, computer, "+
				"agent and whether it may deploy — and only after they say yes call ghost_run_start "+
				"again with the same arguments, confirm=true, this proposalId and a new idempotencyKey.",
			proposed, binding)
	}
	if terr := ghostConsumeProposal(ctx, d, inv, "ghost_run_start", args, binding); terr != nil {
		return nil, terr
	}

	requestID := ghostRunRequestID(d.Now(), inv)
	prompt := codeupdate.BuildPrompt(codeupdate.PromptInput{
		Repo: repo, OutputFile: codeupdate.DefaultOutputFile, Deploy: deploy,
	}, codeupdate.BuildInstructionPrompt(repo, instructions))

	res, err := d.Ghost.Launch(ctx, ghost.LaunchRequest{
		EventID:    codeupdate.EventID(repo),
		Node:       node.NodeID,
		Repo:       repo,
		CLI:        agent,
		Model:      model,
		Effort:     effort,
		Prompt:     prompt,
		OutputFile: codeupdate.DefaultOutputFile,
		Deploy:     deploy,
	}, requestID)
	if err != nil {
		if errors.Is(err, ghost.ErrConflict) {
			return nil, toolErrf(CodeAlreadyExists, "a run is already in progress on %s; wait for it to finish or check ghost_run_status", repo)
		}
		return nil, ghostFleetError(err)
	}

	out := map[string]any{
		"status":    "launched",
		"requestId": requestID,
		"eventId":   res.EventID,
		"runId":     res.Run.RunID,
		"runStatus": res.Run.Status,
		"commandId": res.Run.CommandID,
		"node":      node.NodeID,
		"repo":      repo,
		"agent":     agent,
		"deploy":    deploy,
		"note":      "launched means queued to the computer, not finished; check ghost_run_status before reporting progress",
	}
	if d.CodeUpdate != nil {
		rec := codeupdate.Record{
			RequestID: requestID, UserID: inv.UserID, Status: codeupdate.StatusLaunched,
			Repo: repo, Node: node.NodeID, CLI: agent, Model: model, Deploy: deploy,
			Instructions: instructions, Rewritten: false, RewriteNote: ghostRunNoteNotRewritten,
			EventID: res.EventID, RunID: res.Run.RunID,
		}
		if perr := d.CodeUpdate.Put(ctx, rec); perr != nil {
			d.Log.Error("tools: ghost_run_start record write failed", "error", perr.Error())
			out["recordSaved"] = false
		} else {
			out["recordSaved"] = true
		}
	}
	return out, nil
}

// ghostRunRequestID mints the CODEUPD# request id in UUIDv7 SHAPE: the first
// 48 bits are the Unix millisecond, the version and variant bits are set, and
// the remaining bits come from a hash of the user and the idempotency key the
// registry already claimed (so the id is key-bound, not random). Every other
// CODEUPD# row is a UUIDv7 (codeupdate.Store.Latest: "lexical order on the
// sort key IS time order"), and only an id with the same millisecond prefix
// layout keeps that true — a date-string prefix would sort after every UUIDv7
// forever and make Latest return a ghost run over any newer one.
func ghostRunRequestID(now time.Time, inv Invocation) string {
	sum := sha256.Sum256([]byte("live-ninja/ghost-run\x00" + inv.UserID + "\x00" + inv.IdempotencyKey))
	var b [16]byte
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(now.UnixMilli())&0xFFFFFFFFFFFF)
	copy(b[0:6], ms[2:8])
	copy(b[6:], sum[:10])
	b[6] = 0x70 | (b[6] & 0x0F) // version 7
	b[8] = 0x80 | (b[8] & 0x3F) // RFC 4122 variant
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16]))
}

// ---------------------------------------------------------------------------
// ghost_run_status
// ---------------------------------------------------------------------------

func ghostRunStatusDefinition() *Definition {
	return &Definition{
		Name:      "ghost_run_status",
		OwnerOnly: true,
		Description: "Check a run started with ghost_run_start. Pass the eventId and runId it returned, " +
			"or its requestId, or nothing for the owner's most recent run; the ids are optional, so never ask the owner for them. Reports the run status " +
			"ghost records (for example queued, running, done, failed) and its summary when the " +
			"session left one. Do not call a run finished unless the status says so.",
		Params: []ParamSpec{
			{Name: "requestId", Type: "string", MaxLen: 128, Description: "The requestId from ghost_run_start."},
			{Name: "eventId", Type: "string", MaxLen: 128, Description: "The eventId from ghost_run_start (with runId)."},
			{Name: "runId", Type: "string", MaxLen: 128, Description: "The runId from ghost_run_start (with eventId)."},
		},
		Handler: handleGhostRunStatus,
	}
}

func handleGhostRunStatus(ctx context.Context, d *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if terr := requireGhost(d); terr != nil {
		return nil, terr
	}
	eventID, _ := args["eventId"].(string)
	runID, _ := args["runId"].(string)
	requestID, _ := args["requestId"].(string)
	eventID, runID, requestID = strings.TrimSpace(eventID), strings.TrimSpace(runID), strings.TrimSpace(requestID)

	out := map[string]any{}
	if eventID == "" || runID == "" {
		if d.CodeUpdate == nil {
			return nil, toolErrf(CodeInvalidArgs, "give the eventId and runId that ghost_run_start returned")
		}
		var (
			rec codeupdate.Record
			err error
		)
		if requestID != "" {
			rec, err = d.CodeUpdate.Get(ctx, inv.UserID, requestID)
		} else {
			rec, err = d.CodeUpdate.Latest(ctx, inv.UserID)
		}
		switch {
		case errors.Is(err, codeupdate.ErrNotFound):
			return nil, toolErrf(CodeNotFound, "no run was found for this account")
		case err != nil:
			d.Log.Error("tools: ghost_run_status read failed", "error", err.Error())
			return nil, toolErrf(CodeUpstreamError, "could not read the run record")
		}
		out["requestId"] = rec.RequestID
		out["recordStatus"] = rec.Status
		out["repo"] = rec.Repo
		out["node"] = rec.Node
		out["agent"] = rec.CLI
		out["deploy"] = rec.Deploy
		out["createdAt"] = rec.CreatedAt
		if rec.Error != "" {
			out["error"] = rec.Error
		}
		if rec.EventID == "" || rec.RunID == "" {
			out["note"] = "this request has not been launched on ghost yet; there is no run status to read"
			return out, nil
		}
		eventID, runID = rec.EventID, rec.RunID
	}

	ev, run, err := d.Ghost.FindRun(ctx, eventID, runID, ghostCorrelation(inv))
	if err != nil {
		if errors.Is(err, ghost.ErrNotFound) {
			return nil, toolErrf(CodeNotFound, "ghost has no run %s on event %s", runID, eventID)
		}
		return nil, ghostFleetError(err)
	}
	out["eventId"] = ev.EventID
	out["runId"] = run.RunID
	out["status"] = run.Status
	out["ts"] = run.Ts
	out["trigger"] = run.Trigger
	if run.Node != "" {
		out["node"] = run.Node
	} else if _, ok := out["node"]; !ok {
		out["node"] = ev.Node
	}
	if _, ok := out["repo"]; !ok {
		out["repo"] = ev.Repo
	}
	if _, ok := out["agent"]; !ok && ev.CLI != "" {
		out["agent"] = ev.CLI
	}
	if run.Summary != "" {
		out["summary"] = run.Summary
	}
	out["lastRunStatus"] = ev.LastRunStatus
	out["lastRunTs"] = ev.LastRunTs
	out["note"] = "status is ghost's recorded run state; only a terminal status means the run ended"
	return out, nil
}
