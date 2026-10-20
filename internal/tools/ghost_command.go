package tools

// ghost_session_command — NUDGE, ANSWER, PAUSE or RESUME one session on one
// computer (owner decision 2026-10-19; KILL is not offered).
//
// The command goes to ghost-cli's POST /command through internal/ghost
// (Client.Command restricts the type set client-side). ghost-cli publishes it
// to the node, which applies it asynchronously; the route returns only the
// command id it minted, and no internal-invoke route reads the eventual
// result, so the tool reports "accepted" and says so.
//
// Confirmation is server-issued and user-bound (ghost_confirm.go): the first
// call returns the proposal with a proposalId, the confirmed call must present
// it with the same arguments, and a lone confirm=true sends nothing.
//
// Upstream dependency: POST /command is reachable only once ghost-cli's
// internal-invoke allowlist carries it (plan item ghost-command-allowlist, a
// ghost-cli change shipped through that repository's pipeline). Until then
// ghost-cli refuses the route before its router runs (ghost.ErrRouteNotAllowed)
// and this tool reports not_configured — "session commands are not enabled on
// the fleet service yet" — so the owner hears the real state and the
// idempotency claim is released for a later retry.

import (
	"context"
	"errors"
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
)

func ghostSessionCommandDefinition() *Definition {
	return &Definition{
		Name:          "ghost_session_command",
		OwnerOnly:     true,
		SideEffecting: true,
		Description: "Send one command to one coding-agent session on one of the owner's ghost computers, " +
			"ONLY when the owner explicitly asks: NUDGE types text into the session, ANSWER replies " +
			"to a prompt the session is waiting on (text is the chosen option or answer), PAUSE and " +
			"RESUME suspend and continue it. Killing a session is not available. First call without " +
			"confirm: the tool checks the computer and session exist and returns " +
			"confirmation_required with the exact proposal and a proposalId; read it back and only " +
			"after the owner says yes call again with the SAME arguments, confirm=true, that " +
			"proposalId and a NEW idempotencyKey (confirm=true without it, or with changed " +
			"arguments, is refused and sends nothing). If the fleet service does not accept " +
			"session commands from Live Ninja yet the result is not_configured and nothing was " +
			"sent; say so. The result is " +
			"'accepted' with a commandId: ghost queued it to the computer, which applies it on its " +
			"own time, so do not claim the session already acted on it.",
		Params: []ParamSpec{
			{Name: "node", Type: "string", Required: true, MaxLen: 128,
				Description: "The computer name from ghost_nodes or ghost_sessions."},
			{Name: "sessionId", Type: "string", Required: true, MaxLen: 128,
				Description: "The exact session id from ghost_sessions. Never guess or pick 'the latest' yourself."},
			{Name: "command", Type: "string", Required: true, Enum: ghost.AllowedCommandTypes,
				Description: "NUDGE (send text), ANSWER (reply to the waiting prompt), PAUSE or RESUME."},
			{Name: "text", Type: "string", MaxLen: ghost.MaxCommandText,
				Description: "Required for NUDGE and ANSWER: the exact text to type, one line, at most 280 bytes. Omit for PAUSE and RESUME."},
			{Name: "confirm", Type: "boolean",
				Description: "True only after you read the exact command back and the owner said yes. Send it with the proposalId from that reply and a new idempotencyKey."},
			ghostProposalParam(),
		},
		Handler: handleGhostSessionCommand,
	}
}

func handleGhostSessionCommand(ctx context.Context, d *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if terr := requireGhost(d); terr != nil {
		return nil, terr
	}
	nodeArg, _ := args["node"].(string)
	sessionID, _ := args["sessionId"].(string)
	command, _ := args["command"].(string)
	text, _ := args["text"].(string)
	sessionID = strings.TrimSpace(sessionID)
	text = strings.TrimSpace(text)

	if !ghost.ValidSessionID(sessionID) {
		return nil, toolErrf(CodeInvalidArgs, "sessionId must be a session id as ghost_sessions lists it")
	}
	node, _, terr := ghostRequireNode(ctx, d, inv, nodeArg)
	if terr != nil {
		return nil, terr
	}
	req := ghost.CommandRequest{Type: command, Node: node.NodeID, SessionID: sessionID, Text: text}
	if err := req.Validate(); err != nil {
		return nil, toolErrf(CodeInvalidArgs, "%s", strings.TrimPrefix(err.Error(), ghost.ErrInvalidRequest.Error()+": "))
	}

	// The session must be one ghost has retained for that computer; an id the
	// owner misheard must not be published to a node to fail there, silently.
	_, found, scanned, terr := ghostLookupSession(ctx, d, inv, node.NodeID, sessionID)
	if terr != nil {
		return nil, terr
	}
	if !found {
		if scanned {
			return nil, toolErrf(CodeNotFound, "no session %s is retained for %s; call ghost_sessions to list them", sessionID, node.NodeID)
		}
		return nil, toolErrf(CodeNotFound, "session %s was not found in the first %d pages of retained sessions on %s; call ghost_sessions to find it", sessionID, ghostSessionLookupPages, node.NodeID)
	}

	proposed := map[string]any{"node": node.NodeID, "nodeOnline": ghostNodeOnline(node), "sessionId": sessionID, "command": command}
	if text != "" {
		proposed["text"] = text
	}
	binding := map[string]any{"node": node.NodeID, "sessionId": sessionID, "command": command, "text": text}
	if confirmed, _ := args["confirm"].(bool); !confirmed {
		return nil, ghostIssueProposal(ctx, d, inv, "ghost_session_command",
			"Nothing was sent. Read this exact command back to the owner — computer, session and "+
				"what it does — and only after they say yes call ghost_session_command again with "+
				"the same arguments, confirm=true, this proposalId and a new idempotencyKey.",
			proposed, binding)
	}
	if terr := ghostConsumeProposal(ctx, d, inv, "ghost_session_command", args, binding); terr != nil {
		return nil, terr
	}

	res, err := d.Ghost.Command(ctx, req, ghostCorrelation(inv))
	if err != nil {
		switch {
		case errors.Is(err, ghost.ErrInvalidRequest):
			return nil, toolErrf(CodeInvalidArgs, "the fleet service rejected the command as invalid; nothing was sent")
		case errors.Is(err, ghost.ErrRouteNotAllowed):
			// ghost-command-allowlist has not shipped on the fleet service.
			return nil, toolErrf(CodeNotConfigured, "session commands are not enabled on the fleet service yet; nothing was sent")
		}
		return nil, ghostFleetError(err)
	}
	return map[string]any{
		"status":    "accepted",
		"result":    "accepted",
		"commandId": res.CommandID,
		"command":   command,
		"node":      node.NodeID,
		"sessionId": sessionID,
		"delivery":  "asynchronous",
		"note":      "ghost accepted the command and queued it to the computer; the session applies it on its own time and no delivery receipt is available here",
	}, nil
}
