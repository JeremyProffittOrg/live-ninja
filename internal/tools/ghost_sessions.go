package tools

// ghost_sessions — "what is running on the office PC?" / "what sessions are
// there anywhere?"
//
// Paged over ghost-cli's retained session discovery (GET /history/sessions),
// one node at a time in node-id order when no node is named. Scheduled runs
// (GET /schedule) are reported BESIDE the sessions on the first page, never
// joined to them: ghost-cli has no authoritative session binding for a run.

import (
	"context"
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
)

const (
	ghostSessionsDefaultLimit = 20
	// ghostSessionsMaxPages bounds upstream listings per call, so a node with a
	// huge archive of empty pages cannot stall a voice turn; the cursor carries
	// the walk forward on the next call.
	ghostSessionsMaxPages = 3
	// ghostMaxScheduledRuns bounds the sidebar.
	ghostMaxScheduledRuns = 20
)

func ghostSessionsDefinition() *Definition {
	return &Definition{
		Name:      "ghost_sessions",
		OwnerOnly: true,
		Description: "List the coding-agent sessions ghost has retained for one computer, or for every " +
			"computer when node is omitted, with upload date, last activity and whether each is " +
			"active (activity in the last 30 minutes), idle, or unknown. Results are paged: follow " +
			"nextCursor with cursor. The first page also lists the scheduled runs on those computers " +
			"(repository and last run status) separately — a run and a session are not the same " +
			"record and are never matched to each other. Coverage is retained uploads only: an " +
			"empty list does not prove nothing ran.",
		Params: []ParamSpec{
			{Name: "node", Type: "string", MaxLen: 128,
				Description: "Optional: one computer name from ghost_nodes. Omit to walk every computer."},
			{Name: "cursor", Type: "string", MaxLen: 4096,
				Description: "The nextCursor from an earlier ghost_sessions result."},
			{Name: "limit", Type: "integer", Min: floatPtr(1), Max: floatPtr(50),
				Description: "Sessions per page, default 20."},
		},
		Handler: handleGhostSessions,
	}
}

func handleGhostSessions(ctx context.Context, d *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if terr := requireGhost(d); terr != nil {
		return nil, terr
	}
	limit := ghostSessionsDefaultLimit
	if v, ok := args["limit"].(int); ok && v > 0 {
		limit = v
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

	// Resolve the starting state: a fresh walk, or a decoded cursor that must
	// agree with the node argument it was issued for.
	var state ghostCursor
	switch {
	case cursorArg != "":
		c, ok := decodeGhostCursor(cursorArg)
		if !ok {
			return nil, toolErrf(CodeInvalidArgs, "cursor is not a ghost_sessions cursor; start again without one")
		}
		if c.All != (nodeArg == "") {
			return nil, toolErrf(CodeInvalidArgs, "cursor was issued for a different node scope; start again without one")
		}
		if !c.All {
			n, ok := ghostFindNode(nodes, nodeArg)
			if !ok {
				return nil, toolErrf(CodeNotFound, "no computer named %q is in the fleet; call ghost_nodes to list them", nodeArg)
			}
			if c.N != n.NodeID {
				return nil, toolErrf(CodeInvalidArgs, "cursor was issued for a different computer; start again without one")
			}
		}
		state = c
	case nodeArg != "":
		n, _, terr := ghostRequireNode(ctx, d, inv, nodeArg)
		if terr != nil {
			return nil, terr
		}
		state = ghostCursor{N: n.NodeID}
	default:
		if len(ids) == 0 {
			return map[string]any{"sessions": []map[string]any{}, "nextCursor": "", "scope": "fleet",
				"coverage": ghostCoverage, "message": "the fleet reports no computers at all"}, nil
		}
		state = ghostCursor{All: true, N: ids[0]}
	}

	now := d.Now()
	corr := ghostCorrelation(inv)
	sessions := make([]map[string]any, 0, limit)
	pages := 0
	for len(sessions) < limit && pages < ghostSessionsMaxPages && state.N != "" {
		if state.All {
			// A node that left the fleet mid-walk: continue with the next in order.
			if _, ok := ghostFindNode(nodes, state.N); !ok {
				state = ghostCursor{All: true, N: nextNodeAfter(ids, state.N)}
				continue
			}
		}
		page, err := d.Ghost.HistorySessions(ctx, state.N, state.C, corr)
		if err != nil {
			return nil, ghostFleetError(err)
		}
		pages++
		if state.I > len(page.Sessions) {
			// The upstream page shrank since the cursor was issued; do not skip
			// past content that moved, just finish this page.
			state.I = len(page.Sessions)
		}
		for state.I < len(page.Sessions) && len(sessions) < limit {
			sessions = append(sessions, ghostSessionView(now, state.N, page.Sessions[state.I]))
			state.I++
		}
		if state.I < len(page.Sessions) {
			break // more in this page; the cursor keeps (N, C, I)
		}
		switch {
		case page.NextCursor != "":
			state = ghostCursor{All: state.All, N: state.N, C: page.NextCursor}
		case state.All:
			state = ghostCursor{All: true, N: nextNodeAfter(ids, state.N)}
		default:
			state = ghostCursor{}
		}
	}

	next := ""
	if state.N != "" {
		next = encodeGhostCursor(state)
	}
	out := map[string]any{
		"sessions":            sessions,
		"nextCursor":          next,
		"coverage":            ghostCoverage,
		"activeWindowMinutes": int(ghostActiveWindow.Minutes()),
		"note": "sessions are listed oldest upload partition first; uploadDate is the archive " +
			"partition, not the start time; an empty list only means nothing was retained",
	}
	if nodeArg != "" {
		out["scope"] = "node"
		out["node"] = state.N
		if out["node"] == "" {
			n, _ := ghostFindNode(nodes, nodeArg)
			out["node"] = n.NodeID
		}
	} else {
		out["scope"] = "fleet"
		out["nodes"] = ids
	}

	// First page only: the scheduled-run sidebar.
	if cursorArg == "" {
		scope := map[string]bool{}
		if nodeArg != "" {
			n, _ := ghostFindNode(nodes, nodeArg)
			scope[n.NodeID] = true
		} else {
			for _, id := range ids {
				scope[id] = true
			}
		}
		runs, available := ghostScheduledRuns(ctx, d, corr, scope)
		out["scheduledRuns"] = runs
		out["scheduledRunsAvailable"] = available
		out["scheduledRunsNote"] = "scheduled runs come from the ghost schedule and are not matched to any session above"
	}
	return out, nil
}

// nextNodeAfter returns the first id lexically after cur, or "" at the end.
func nextNodeAfter(ids []string, cur string) string {
	for _, id := range ids {
		if id > cur {
			return id
		}
	}
	return ""
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
