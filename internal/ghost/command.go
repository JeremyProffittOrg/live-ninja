package ghost

// POST /command — session control for ghost-cli sessions.
//
// Canonical source: ghost-cli lambda/command/handler.go (commandRequest,
// issueResponse), params.go (buildSessionParams) and envelope.go (the type
// sets and bounds). The body is
//
//	{"type":"NUDGE","node":"OFFICEPC","params":{"session_id":"<id>","text":"..."}}
//
// and a success is 202 {"command_id":"<uuid>"}. The command is then published
// to the node's cmd topic and applied asynchronously; ghost-cli exposes no
// internal-invoke route that reads a command's eventual result, so "accepted"
// is the strongest statement this client can make.
//
// This client restricts itself to a SUBSET of what the route accepts, on
// purpose (owner decision 2026-10-19: "NUDGE (send text), ANSWER (reply to
// prompt), PAUSE / RESUME" — KILL is not allowed). KILL, LAUNCH, SAVE_LOCAL and
// LIST_SAVED are refused here before anything is sent, so a widening of the
// far-side allowlist can never widen what a voice command can do.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Command types this client will send. The literals are ghost-cli's
// CommandType values (lambda/command/envelope.go) — a CROSS-REPO CONTRACT,
// pinned by TestCommandTypesMatchGhostCLI.
const (
	CommandNudge  = "NUDGE"
	CommandAnswer = "ANSWER"
	CommandPause  = "PAUSE"
	CommandResume = "RESUME"
)

// AllowedCommandTypes is the closed set, in the order a manifest should list
// them.
var AllowedCommandTypes = []string{CommandNudge, CommandAnswer, CommandPause, CommandResume}

// MaxCommandText is ghost-cli's maxNudgeText (280), measured in BYTES there
// (len(txt)), so it is measured in bytes here too. It applies to NUDGE and
// ANSWER alike: ANSWER reuses the text param for the chosen option label.
const MaxCommandText = 280

// CommandAllowed reports whether t is one of the four session commands this
// client sends.
func CommandAllowed(t string) bool {
	for _, allowed := range AllowedCommandTypes {
		if t == allowed {
			return true
		}
	}
	return false
}

// CommandRequest is one session command. Node and SessionID name exactly one
// session on one machine; there is no fleet-wide or "most recent session" form
// (ghost-cli D42 rejects a missing session_id and so does this client).
type CommandRequest struct {
	Type      string
	Node      string
	SessionID string
	// Text is required for NUDGE and ANSWER and must be absent for PAUSE and
	// RESUME — ghost-cli would silently drop it for those, and a caller that
	// sent text with a PAUSE almost certainly meant a NUDGE.
	Text string
}

// CommandResult is the accepted command.
type CommandResult struct {
	CommandID string `json:"command_id"`
}

// commandBody is the wire shape of ghost-cli's commandRequest, with only the
// params this client ever sets.
type commandBody struct {
	Type   string        `json:"type"`
	Node   string        `json:"node"`
	Params commandParams `json:"params"`
}

type commandParams struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text,omitempty"`
}

// Validate checks the request client-side against the same rules ghost-cli
// enforces, plus this client's own type restriction, so a bad request is
// refused here with ErrInvalidRequest and never costs an upstream round trip.
func (r CommandRequest) Validate() error {
	if !CommandAllowed(r.Type) {
		return fmt.Errorf("%w: command type %q is not one of %s", ErrInvalidRequest,
			r.Type, strings.Join(AllowedCommandTypes, ", "))
	}
	if !ValidNodeID(r.Node) {
		return fmt.Errorf("%w: invalid node", ErrInvalidRequest)
	}
	if !ValidSessionID(r.SessionID) {
		return fmt.Errorf("%w: invalid session id", ErrInvalidRequest)
	}
	switch r.Type {
	case CommandNudge, CommandAnswer:
		if strings.TrimSpace(r.Text) == "" {
			return fmt.Errorf("%w: text is required for %s", ErrInvalidRequest, r.Type)
		}
		if len(r.Text) > MaxCommandText {
			return fmt.Errorf("%w: text exceeds %d bytes", ErrInvalidRequest, MaxCommandText)
		}
		if strings.ContainsAny(r.Text, "\x00\r\n") {
			return fmt.Errorf("%w: text must be a single line", ErrInvalidRequest)
		}
	default:
		if r.Text != "" {
			return fmt.Errorf("%w: %s takes no text", ErrInvalidRequest, r.Type)
		}
	}
	return nil
}

// Command sends one session command and returns the command id ghost-cli
// assigned. The caller's correlation id rides the audit trail there.
//
// Never logs the text: ghost-cli persists NUDGE/ANSWER text verbatim in its
// hash-chained audit (D52) and keeps it out of logs; this side does the same.
func (c *Client) Command(ctx context.Context, req CommandRequest, corrID string) (CommandResult, error) {
	if err := req.Validate(); err != nil {
		return CommandResult{}, err
	}
	body, err := json.Marshal(commandBody{
		Type:   req.Type,
		Node:   req.Node,
		Params: commandParams{SessionID: req.SessionID, Text: req.Text},
	})
	if err != nil {
		return CommandResult{}, fmt.Errorf("%w: marshal command: %v", ErrUpstream, err)
	}
	resp, err := c.call(ctx, "POST", "/command", string(body), nil, corrID)
	if err != nil {
		return CommandResult{}, err
	}
	var parsed CommandResult
	if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
		return CommandResult{}, fmt.Errorf("%w: decode command response: %v", ErrUpstream, err)
	}
	parsed.CommandID = strings.TrimSpace(parsed.CommandID)
	if parsed.CommandID == "" || len(parsed.CommandID) > 128 {
		return CommandResult{}, fmt.Errorf("%w: command returned no command id", ErrUpstream)
	}
	return parsed, nil
}
