package tools

// ghost_sessions — "what is running on the office PC?" / "what sessions are
// there anywhere?"
//
// Built over ghost-cli's retained session discovery (GET /history/sessions).
// Upstream lists S3 keys in ascending upload order, about 1000 per page, so
// its first page is the OLDEST work on a computer. This tool therefore walks
// each computer's whole listing (ghostWalkSessions, bounded), merges the
// computers in scope, and pages over that list newest activity first.
// Scheduled runs (GET /schedule) are reported BESIDE the sessions on the
// first page, never joined to them: ghost-cli has no authoritative session
// binding for a run.

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
)

const (
	ghostSessionsDefaultLimit = 20
	// ghostSessionsMaxSince bounds the optional sinceMinutes filter (7 days).
	ghostSessionsMaxSince = 7 * 24 * 60
	// ghostMaxScheduledRuns bounds the sidebar.
	ghostMaxScheduledRuns = 20
)

func ghostSessionsDefinition() *Definition {
	return &Definition{
		Name:      "ghost_sessions",
		OwnerOnly: true,
		Description: "List the coding-agent sessions ghost has retained for one computer, or for every " +
			"computer when node is omitted, NEWEST ACTIVITY FIRST, with upload date, last activity and " +
			"whether each is active (activity in the last 30 minutes), idle, or unknown. Use " +
			"sinceMinutes to keep only sessions with an upload in that recent window. Results are " +
			"paged: follow nextCursor with cursor. partial=true means a computer's listing was longer " +
			"than the read bound and its newest sessions may be missing. The first page also lists " +
			"the scheduled runs on those computers (repository and last run status) separately — a " +
			"run and a session are not the same record and are never matched to each other. " +
			"Coverage is retained uploads only: an empty list does not prove nothing ran.",
		Params: []ParamSpec{
			{Name: "node", Type: "string", MaxLen: 128,
				Description: "Optional: one computer name from ghost_nodes. Omit to list every computer."},
			{Name: "cursor", Type: "string", MaxLen: 4096,
				Description: "The nextCursor from an earlier ghost_sessions result, with the same node and sinceMinutes."},
			{Name: "limit", Type: "integer", Min: floatPtr(1), Max: floatPtr(50),
				Description: "Sessions per page, default 20."},
			{Name: "sinceMinutes", Type: "integer", Min: floatPtr(1), Max: floatPtr(ghostSessionsMaxSince),
				Description: "Optional: only sessions with a retained upload in the last this-many minutes."},
		},
		Handler: handleGhostSessions,
	}
}

// ghostNodeSession is one listed session with the computer it belongs to.
type ghostNodeSession struct {
	node  string
	s     ghost.HistorySession
	ts    time.Time
	dated bool
}

func handleGhostSessions(ctx context.Context, d *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if terr := requireGhost(d); terr != nil {
		return nil, terr
	}
	limit := ghostSessionsDefaultLimit
	if v, ok := args["limit"].(int); ok && v > 0 {
		limit = v
	}
	since := 0
	if v, ok := args["sinceMinutes"].(int); ok && v > 0 {
		since = v
	}
	nodeArg, _ := args["node"].(string)
	nodeArg = strings.TrimSpace(nodeArg)
	cursorArg, _ := args["cursor"].(string)
	cursorArg = strings.TrimSpace(cursorArg)

	nodes, terr := ghostFleet(ctx, d, inv)
	if terr != nil {
		return nil, terr
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.NodeID)
	}

	// Resolve the scope, then the offset: a fresh walk, or a decoded cursor
	// that must agree with the node and sinceMinutes it was issued for.
	scope := ids
	nodeID := ""
	if nodeArg != "" {
		if !ghost.ValidNodeID(nodeArg) {
			return nil, toolErrf(CodeInvalidArgs, "node must be a computer name as ghost_nodes lists it")
		}
		n, ok := ghostFindNode(nodes, nodeArg)
		if !ok {
			return nil, toolErrf(CodeNotFound, "no computer named %q is in the fleet; call ghost_nodes to list them", nodeArg)
		}
		nodeID = n.NodeID
		scope = []string{n.NodeID}
	}
	offset := 0
	if cursorArg != "" {
		c, ok := decodeGhostCursor(cursorArg)
		if !ok {
			return nil, toolErrf(CodeInvalidArgs, "cursor is not a ghost_sessions cursor; start again without one")
		}
		if c.All != (nodeArg == "") {
			return nil, toolErrf(CodeInvalidArgs, "cursor was issued for a different node scope; start again without one")
		}
		if !c.All && c.N != nodeID {
			return nil, toolErrf(CodeInvalidArgs, "cursor was issued for a different computer; start again without one")
		}
		if c.S != since {
			return nil, toolErrf(CodeInvalidArgs, "cursor was issued for a different sinceMinutes; start again without one")
		}
		offset = c.I
	}

	now := d.Now()
	corr := ghostCorrelation(inv)
	cutoff := now.Add(-time.Duration(since) * time.Minute)
	var all []ghostNodeSession
	partialReasons := []string{}
	for _, id := range scope {
		listing, err := ghostWalkSessions(ctx, d, corr, id)
		if err != nil {
			return nil, ghostFleetError(err)
		}
		if listing.Partial {
			partialReasons = append(partialReasons, listing.PartialReason)
		}
		for _, s := range listing.Sessions {
			ts, dated := ghostParseTime(s.UpdatedAt)
			if since > 0 && (!dated || ts.Before(cutoff)) {
				continue
			}
			all = append(all, ghostNodeSession{node: id, s: s, ts: ts, dated: dated})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		switch {
		case a.dated && b.dated:
			if !a.ts.Equal(b.ts) {
				return a.ts.After(b.ts)
			}
		case a.dated != b.dated:
			return a.dated
		}
		if a.node != b.node {
			return a.node < b.node
		}
		return a.s.SessionID < b.s.SessionID
	})

	if offset > len(all) {
		// The listing shrank since the cursor was issued (retention expiry):
		// end the walk rather than invent rows.
		offset = len(all)
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	sessions := make([]map[string]any, 0, end-offset)
	for _, row := range all[offset:end] {
		sessions = append(sessions, ghostSessionView(now, row.node, row.s))
	}
	next := ""
	if end < len(all) {
		next = encodeGhostCursor(ghostCursor{All: nodeArg == "", N: nodeID, I: end, S: since})
	}
	out := map[string]any{
		"sessions":            sessions,
		"nextCursor":          next,
		"retainedSessions":    len(all),
		"coverage":            ghostCoverage,
		"activeWindowMinutes": int(ghostActiveWindow.Minutes()),
		"partial":             len(partialReasons) > 0,
		"partialReasons":      partialReasons,
		"note": "sessions are listed newest activity first; uploadDate is the archive " +
			"partition, not the start time; an empty list only means nothing was retained",
	}
	if since > 0 {
		out["sinceMinutes"] = since
	}
	if nodeArg != "" {
		out["scope"] = "node"
		out["node"] = nodeID
	} else {
		out["scope"] = "fleet"
		out["nodes"] = ids
		if len(ids) == 0 {
			out["message"] = "the fleet reports no computers at all"
		}
	}

	// First page only: the scheduled-run sidebar.
	if cursorArg == "" {
		inScope := map[string]bool{}
		for _, id := range scope {
			inScope[id] = true
		}
		runs, available := ghostScheduledRuns(ctx, d, corr, inScope)
		out["scheduledRuns"] = runs
		out["scheduledRunsAvailable"] = available
		out["scheduledRunsNote"] = "scheduled runs come from the ghost schedule and are not matched to any session above"
	}
	return out, nil
}

// ghostScheduledRuns renders the scheduled events whose node is in scope.
// Failures never fail the listing: the sidebar is context, not the answer.
func ghostScheduledRuns(ctx context.Context, d *Deps, corr string, scope map[string]bool) ([]map[string]any, bool) {
	events, err := d.Ghost.ScheduledWork(ctx, corr)
	if err != nil {
		d.Log.Warn("tools: ghost_sessions scheduled runs unavailable", "error", err.Error())
		return []map[string]any{}, false
	}
	out := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		if !scope[ev.Node] && !(ev.LastRunNode != "" && scope[ev.LastRunNode]) {
			continue
		}
		if len(out) >= ghostMaxScheduledRuns {
			break
		}
		out = append(out, ghostEventView(ev))
	}
	return out, true
}

func ghostEventView(ev ghost.ScheduledEvent) map[string]any {
	row := map[string]any{
		"eventId": ev.EventID,
		"node":    ev.Node,
		"repo":    ev.Repo,
		"agent":   ev.CLI,
		"enabled": ev.Enabled,
	}
	if ev.LastRunID != "" {
		row["lastRunId"] = ev.LastRunID
		row["lastRunStatus"] = ev.LastRunStatus
		row["lastRunTs"] = ev.LastRunTs
		if ev.LastRunNode != "" {
			row["lastRunNode"] = ev.LastRunNode
		}
	}
	return row
}
