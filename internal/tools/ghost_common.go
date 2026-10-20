package tools

// Shared plumbing for the ghost_* tools (ghost-job-control): the voice view of
// the owner's ghost-cli fleet — computers, their coding-agent sessions, and the
// runs launched on them.
//
// Everything here reads through internal/ghost, which reaches ghost-cli over
// the IAM-authenticated internal invoke. Two facts shape every tool:
//
//   - ghost-cli's history is "retained_only": it holds what the node's
//     consented uploader sent, and nothing else. An empty listing is not an
//     empty conversation and a missing session is not proof it never ran, so
//     every output says so (coverage, partial, note) rather than implying
//     completeness.
//   - There is no join from a scheduled run to a provider session id
//     (ghost.ScheduledEvent comment). Scheduled runs are reported beside
//     sessions, never fused with them.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
)

// ghostActiveWindow is how recent a session's last retained upload must be for
// the fleet tools to call it "active". ghost-cli reports no live session state
// over the internal transport; the newest retained object's LastModified is
// the only activity signal there is, and uploads trail the terminal by up to a
// batch interval, so the window is generous.
const ghostActiveWindow = 30 * time.Minute

// Activity labels. "unknown" means ghost-cli reported no last-modified time
// for the session, never that nothing happened.
const (
	ghostActivityActive  = "active"
	ghostActivityIdle    = "idle"
	ghostActivityUnknown = "unknown"
)

// ghostCoverage is the only coverage ghost-cli's history serves. Echoed on
// every output so the model never presents the archive as complete.
const ghostCoverage = "retained_only"

// ghostSessionLookupPages bounds how many /history/sessions pages a tool scans
// when it needs to find one session by id. Each page collapses up to 1000 S3
// keys, so five pages cover thousands of sessions on one node.
const ghostSessionLookupPages = 5

func requireGhost(d *Deps) *ToolError {
	if d == nil || d.Ghost == nil || !d.Ghost.Ready() {
		return toolErrf(CodeNotConfigured, "the fleet (ghost) integration is not configured")
	}
	return nil
}

// ghostFleetError maps a ghost client error onto the tool-router vocabulary
// with wording for the fleet tools (ghostToolError's is code-update specific).
func ghostFleetError(err error) *ToolError {
	switch {
	case errors.Is(err, ghost.ErrNotConfigured):
		return toolErrf(CodeNotConfigured, "the fleet (ghost) integration is not configured")
	case errors.Is(err, ghost.ErrNotAuthorized):
		return toolErrf(CodeForbidden, "the fleet service refused the request; Live Ninja is not authorized for that")
	case errors.Is(err, ghost.ErrInvalidRequest):
		return toolErrf(CodeInvalidArgs, "the fleet service rejected the request as invalid")
	case errors.Is(err, ghost.ErrNotFound):
		return toolErrf(CodeNotFound, "the fleet service has no record of that")
	case errors.Is(err, ghost.ErrConflict):
		return toolErrf(CodeAlreadyExists, "the fleet service reports that is already in progress")
	case errors.Is(err, ghost.ErrQuota):
		// A 429 is a definite refusal: nothing was launched or published, so
		// the code must be one idempotencyReleasable allows, or the IDEMP#
		// marker would hold a never-attempted call for 24 h.
		return toolErrf(CodeNotConfigured, "the fleet service quota is used up for the moment; try again in a few minutes")
	case errors.Is(err, ghost.ErrRouteNotAllowed):
		// ghost-cli's internal-invoke allowlist refused the route before its
		// router ran (ghost.ErrRouteNotAllowed): coded here, not enabled there.
		return toolErrf(CodeNotConfigured, "the fleet service does not accept that request from Live Ninja yet")
	case errors.Is(err, ghost.ErrUnavailable):
		return toolErrf(CodeNotConfigured, "the fleet service is deployed but not able to serve that right now")
	case errors.Is(err, ghost.ErrHistoryGone):
		return toolErrf(CodeNotFound, "that retained history has expired or was removed")
	case errors.Is(err, ghost.ErrHistoryTooLarge), errors.Is(err, ghost.ErrHistoryMalformed):
		return toolErrf(CodeUpstreamError, "that retained history could not be read")
	default:
		return toolErrf(CodeUpstreamError, "could not reach the fleet service")
	}
}

// ghostCorrelation is the correlation id sent with every fleet call so the
// ghost-cli audit trail can be tied back to this tool invocation.
func ghostCorrelation(inv Invocation) string {
	if inv.TxID != "" {
		return inv.TxID
	}
	return inv.CallID
}

// ghostNodeOnline folds ghost-cli's presence into one boolean: a node is
// online when the broker says it is connected or the presence rollup is
// "live". "unknown" (missed heartbeats, LWT not yet fired) is reported as
// offline for the purpose of "can I talk to it now".
func ghostNodeOnline(n ghost.Node) bool {
	return n.Connected || strings.EqualFold(n.State, "live")
}

// ghostFleet lists the nodes, sorted by id for stable paging and stable voice
// output.
func ghostFleet(ctx context.Context, d *Deps, inv Invocation) ([]ghost.Node, *ToolError) {
	nodes, err := d.Ghost.Nodes(ctx, ghostCorrelation(inv))
	if err != nil {
		return nil, ghostFleetError(err)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	return nodes, nil
}

// ghostFindNode resolves a spoken/typed node name against the fleet. The
// match is case-insensitive because the IoT thing names are uppercase
// ("OFFICEPC") and nobody says them that way; the returned Node carries the
// exact id to send upstream.
func ghostFindNode(nodes []ghost.Node, name string) (ghost.Node, bool) {
	name = strings.TrimSpace(name)
	for _, n := range nodes {
		if n.NodeID == name {
			return n, true
		}
	}
	for _, n := range nodes {
		if strings.EqualFold(n.NodeID, name) {
			return n, true
		}
	}
	return ghost.Node{}, false
}

// ghostRequireNode validates the shape of a node argument and resolves it
// against the fleet, so "that machine does not exist" is told apart from
// "that machine is offline" before anything is sent to it.
func ghostRequireNode(ctx context.Context, d *Deps, inv Invocation, name string) (ghost.Node, []ghost.Node, *ToolError) {
	name = strings.TrimSpace(name)
	if !ghost.ValidNodeID(name) {
		return ghost.Node{}, nil, toolErrf(CodeInvalidArgs, "node must be a computer name as ghost_nodes lists it")
	}
	nodes, terr := ghostFleet(ctx, d, inv)
	if terr != nil {
		return ghost.Node{}, nil, terr
	}
	n, ok := ghostFindNode(nodes, name)
	if !ok {
		return ghost.Node{}, nodes, toolErrf(CodeNotFound, "no computer named %q is in the fleet; call ghost_nodes to list them", name)
	}
	return n, nodes, nil
}

// ghostActivity classifies a session by its last retained upload time.
func ghostActivity(now time.Time, updatedAt string) (activity string, lastActivity string) {
	ts, ok := ghostParseTime(updatedAt)
	if !ok {
		return ghostActivityUnknown, ""
	}
	if now.Sub(ts) <= ghostActiveWindow {
		return ghostActivityActive, ts.UTC().Format(time.RFC3339)
	}
	return ghostActivityIdle, ts.UTC().Format(time.RFC3339)
}

func ghostParseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

// ghostSessionView is the per-session row ghost_sessions and ghost_nodes share.
func ghostSessionView(now time.Time, node string, s ghost.HistorySession) map[string]any {
	activity, last := ghostActivity(now, s.UpdatedAt)
	out := map[string]any{
		"node":      node,
		"sessionId": s.SessionID,
		// The archive partitions by UPLOAD date; it is not the start time.
		"uploadDate": s.Date,
		"activity":   activity,
	}
	if last != "" {
		out["lastActivity"] = last
	}
	return out
}

// ghostLookupSession scans a node's retained session listing for one id.
// found=false with scanned=true means every page was read and the id was not
// there; scanned=false means the page bound stopped the scan first, so the
// absence is not proven.
func ghostLookupSession(ctx context.Context, d *Deps, inv Invocation, node, sessionID string) (s ghost.HistorySession, found bool, scanned bool, terr *ToolError) {
	corr := ghostCorrelation(inv)
	cursor := ""
	for page := 0; page < ghostSessionLookupPages; page++ {
		p, err := d.Ghost.HistorySessions(ctx, node, cursor, corr)
		if err != nil {
			return ghost.HistorySession{}, false, false, ghostFleetError(err)
		}
		for _, candidate := range p.Sessions {
			if candidate.SessionID == sessionID {
				return candidate, true, true, nil
			}
		}
		if p.NextCursor == "" {
			return ghost.HistorySession{}, false, true, nil
		}
		cursor = p.NextCursor
	}
	return ghost.HistorySession{}, false, false, nil
}

// ---------------------------------------------------------------------------
// Opaque paging cursors
// ---------------------------------------------------------------------------

// ghostCursor is the ghost_sessions paging state. It wraps ghost-cli's own
// opaque cursor (C) with the node being paged (N), the offset into the
// upstream page already handed out (I) and whether the walk spans the whole
// fleet (All). It is base64url JSON on the wire: the model treats it as an
// opaque token, and the decoder validates every field so a tampered cursor is
// refused rather than steering the walk.
type ghostCursor struct {
	V   int    `json:"v"`
	All bool   `json:"all"`
	N   string `json:"n"`
	C   string `json:"c,omitempty"`
	I   int    `json:"i,omitempty"`
}

const ghostCursorVersion = 1

func encodeGhostCursor(c ghostCursor) string {
	c.V = ghostCursorVersion
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeGhostCursor(s string) (ghostCursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return ghostCursor{}, false
	}
	var c ghostCursor
	if json.Unmarshal(raw, &c) != nil || c.V != ghostCursorVersion || !ghost.ValidNodeID(c.N) ||
		c.I < 0 || c.I > 100000 || len(c.C) > 2048 || strings.ContainsAny(c.C, "\x00\r\n") {
		return ghostCursor{}, false
	}
	return c, true
}

// clampText bounds untrusted session text for a voice reply, on a rune
// boundary, marking the cut.
func clampText(s string, n int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return strings.TrimSpace(string(runes[:n])) + "…"
}
