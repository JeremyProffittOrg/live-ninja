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

// ghostSessionWalkPages bounds how many /history/sessions pages one walk of a
// node's retained listing reads. Upstream lists S3 keys in ascending upload
// order, about 1000 per page, so the NEWEST sessions are on the LAST page: a
// walk has to reach the end to see today's work. OFFICEPC carried about 16
// pages on 2026-10-24; 40 leaves headroom while keeping a voice turn bounded.
// When the bound stops a walk, every caller reports partial rather than
// implying the listing was complete.
const ghostSessionWalkPages = 40

// ghostSessionLookupPages is the same bound under the name ghost_command.go
// quotes in its "not found within N pages" message.
const ghostSessionLookupPages = ghostSessionWalkPages

// The walk is also bounded in time, because the tools run inside a 30 s
// budget shared with model calls (Alexa worker, web tool Lambda) and a full
// OFFICEPC walk measured about 4.4 s on 2026-10-24. Before each page after
// the first, the walk stops when ghostSessionWalkBudget has elapsed (by
// d.Now, so tests inject the clock) or when the context deadline is closer
// than ghostSessionWalkReserve, and returns what it read as partial.
const (
	ghostSessionWalkBudget  = 12 * time.Second
	ghostSessionWalkReserve = 8 * time.Second
)

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

// ghostSessionListing is one node's retained session listing after a full
// walk: one row per session id (a session that uploaded on several dates is
// listed under each upload partition upstream), newest activity first.
type ghostSessionListing struct {
	Sessions []ghost.HistorySession
	Pages    int
	// Partial is true only when a bound (ghostSessionWalkPages, or the time
	// budget) stopped the walk before upstream returned an empty next_cursor.
	// PartialReason then says which, for the tool's partialReasons.
	Partial       bool
	PartialReason string
}

// ghostWalkSessions reads a node's whole retained session listing, following
// only the opaque next_cursor upstream returned (never a forged one), up to
// ghostSessionWalkPages pages. Duplicate session ids keep the row with the
// newest UpdatedAt. Sessions are ordered by UpdatedAt, newest first; sessions
// without a timestamp go last, by id, so the order is stable.
func ghostWalkSessions(ctx context.Context, d *Deps, corr, node string) (ghostSessionListing, error) {
	var listing ghostSessionListing
	index := map[string]int{}
	cursor := ""
	started := d.Now()
	for {
		if listing.Pages > 0 && ghostWalkOutOfTime(ctx, d, started) {
			listing.Partial = true
			listing.PartialReason = "stopped after " + itoa(listing.Pages) + " pages to stay within the time limit while reading " +
				"retained sessions on " + node + "; the newest sessions may be on later pages"
			break
		}
		page, err := d.Ghost.HistorySessions(ctx, node, cursor, corr)
		if err != nil {
			return ghostSessionListing{}, err
		}
		listing.Pages++
		for _, s := range page.Sessions {
			i, seen := index[s.SessionID]
			if !seen {
				index[s.SessionID] = len(listing.Sessions)
				listing.Sessions = append(listing.Sessions, s)
				continue
			}
			if ghostNewerUpload(s, listing.Sessions[i]) {
				listing.Sessions[i] = s
			}
		}
		if page.NextCursor == "" {
			break
		}
		if listing.Pages >= ghostSessionWalkPages {
			listing.Partial = true
			listing.PartialReason = "only the first " + itoa(ghostSessionWalkPages) + " pages of retained sessions on " + node +
				" were read; the newest sessions may be on later pages"
			break
		}
		cursor = page.NextCursor
	}
	sort.SliceStable(listing.Sessions, func(i, j int) bool {
		a, aok := ghostParseTime(listing.Sessions[i].UpdatedAt)
		b, bok := ghostParseTime(listing.Sessions[j].UpdatedAt)
		switch {
		case aok && bok:
			if !a.Equal(b) {
				return a.After(b)
			}
		case aok != bok:
			return aok
		}
		return listing.Sessions[i].SessionID < listing.Sessions[j].SessionID
	})
	return listing, nil
}

// ghostNewerUpload reports whether candidate carries newer activity than
// current: a parseable time beats none, and a later time beats an earlier one.
func ghostNewerUpload(candidate, current ghost.HistorySession) bool {
	c, cok := ghostParseTime(candidate.UpdatedAt)
	k, kok := ghostParseTime(current.UpdatedAt)
	switch {
	case !cok:
		return false
	case !kok:
		return true
	default:
		return c.After(k)
	}
}

// ghostWalkOutOfTime reports whether a session walk that began at started
// must stop paging: the walk budget is spent, or the caller's deadline is too
// close to risk another upstream call.
func ghostWalkOutOfTime(ctx context.Context, d *Deps, started time.Time) bool {
	if d.Now().Sub(started) >= ghostSessionWalkBudget {
		return true
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < ghostSessionWalkReserve {
		return true
	}
	return false
}

// ghostLookupSession finds one session id in a node's retained listing.
// found=false with scanned=true means the whole listing was read and the id
// was not there; scanned=false means the walk bound stopped the scan first,
// so the absence is not proven.
func ghostLookupSession(ctx context.Context, d *Deps, inv Invocation, node, sessionID string) (s ghost.HistorySession, found bool, scanned bool, terr *ToolError) {
	listing, err := ghostWalkSessions(ctx, d, ghostCorrelation(inv), node)
	if err != nil {
		return ghost.HistorySession{}, false, false, ghostFleetError(err)
	}
	for _, candidate := range listing.Sessions {
		if candidate.SessionID == sessionID {
			return candidate, true, true, nil
		}
	}
	return ghost.HistorySession{}, false, !listing.Partial, nil
}

// ---------------------------------------------------------------------------
// Opaque paging cursors
// ---------------------------------------------------------------------------

// ghostCursor is the ghost_sessions paging state: the offset (I) into the
// newest-first listing already handed out, the computer it was issued for (N,
// empty for the whole fleet), whether it spans the fleet (All) and the
// sinceMinutes filter it was issued with (S). It never carries an upstream
// cursor: every call walks the upstream listing from its start, following
// only the cursors upstream itself returns. It is base64url JSON on the wire:
// the model treats it as an opaque token, and the decoder validates every
// field so a tampered cursor is refused rather than steering the walk.
// Version 2 replaced the upstream-order cursor; a version 1 cursor is refused.
type ghostCursor struct {
	V   int    `json:"v"`
	All bool   `json:"all"`
	N   string `json:"n,omitempty"`
	I   int    `json:"i,omitempty"`
	S   int    `json:"s,omitempty"`
}

const ghostCursorVersion = 2

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
	if json.Unmarshal(raw, &c) != nil || c.V != ghostCursorVersion ||
		(c.All && c.N != "") || (!c.All && !ghost.ValidNodeID(c.N)) ||
		c.I < 0 || c.I > 100000 || c.S < 0 || c.S > ghostSessionsMaxSince {
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
