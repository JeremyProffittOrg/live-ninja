package tools

// ghost_nodes — "which of my computers are up?"

import (
	"context"
	"strings"
)

// ghostMaxSessionCountNodes bounds the per-node session listings one
// ghost_nodes call makes to count active sessions. Offline nodes are never
// listed (nothing can be active there), so this only bites on a fleet with
// more than eight machines online at once.
const ghostMaxSessionCountNodes = 8

func ghostNodesDefinition() *Definition {
	return &Definition{
		Name:      "ghost_nodes",
		OwnerOnly: true,
		Description: "List the owner's ghost fleet computers with online state, agent version and the " +
			"number of coding sessions active in the last 30 minutes, plus lastActivity (the newest " +
			"retained session upload on that computer). Use it when the owner asks " +
			"which computers are up, what version a machine runs, or whether anything is running " +
			"somewhere. Session counts come from retained history uploads, so a count can lag the " +
			"terminal by a few minutes; activeSessionsKnown=false means the count could not be read.",
		Params: []ParamSpec{
			{Name: "node", Type: "string", MaxLen: 128,
				Description: "Optional: one computer name to report, e.g. OFFICEPC. Omit for the whole fleet."},
		},
		Handler: handleGhostNodes,
	}
}

func handleGhostNodes(ctx context.Context, d *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if terr := requireGhost(d); terr != nil {
		return nil, terr
	}
	nodes, terr := ghostFleet(ctx, d, inv)
	if terr != nil {
		return nil, terr
	}
	if want, _ := args["node"].(string); strings.TrimSpace(want) != "" {
		n, ok := ghostFindNode(nodes, want)
		if !ok {
			return nil, toolErrf(CodeNotFound, "no computer named %q is in the fleet", strings.TrimSpace(want))
		}
		nodes = nodes[:0:0]
		nodes = append(nodes, n)
	}

	now := d.Now()
	corr := ghostCorrelation(inv)
	out := make([]map[string]any, 0, len(nodes))
	online, counted := 0, 0
	for _, n := range nodes {
		isOnline := ghostNodeOnline(n)
		row := map[string]any{
			"node":                n.NodeID,
			"online":              isOnline,
			"state":               n.State,
			"connected":           n.Connected,
			"agentVersion":        n.AgentVersion,
			"lastSeen":            n.LastSeen,
			"activeSessions":      0,
			"activeSessionsKnown": true,
		}
		if isOnline {
			online++
			switch {
			case counted >= ghostMaxSessionCountNodes:
				row["activeSessionsKnown"] = false
				row["activeSessionsNote"] = "not counted: more than " + itoa(ghostMaxSessionCountNodes) + " computers are online; ask about this one by name"
			default:
				counted++
				listing, err := ghostWalkSessions(ctx, d, corr, n.NodeID)
				if err != nil {
					d.Log.Warn("tools: ghost_nodes session count failed", "node", n.NodeID, "error", err.Error())
					row["activeSessionsKnown"] = false
					row["activeSessionsNote"] = "the retained session listing could not be read"
					break
				}
				active := 0
				for _, s := range listing.Sessions {
					if activity, _ := ghostActivity(now, s.UpdatedAt); activity == ghostActivityActive {
						active++
					}
				}
				row["activeSessions"] = active
				row["retainedSessions"] = len(listing.Sessions)
				// Newest first: the first dated session is the computer's
				// latest retained activity.
				if len(listing.Sessions) > 0 {
					if _, last := ghostActivity(now, listing.Sessions[0].UpdatedAt); last != "" {
						row["lastActivity"] = last
					}
				}
				if listing.Partial {
					row["activeSessionsPartial"] = true
					row["activeSessionsNote"] = listing.PartialReason
				}
			}
		}
		out = append(out, row)
	}

	result := map[string]any{
		"nodes":               out,
		"total":               len(out),
		"online":              online,
		"activeWindowMinutes": int(ghostActiveWindow.Minutes()),
		"coverage":            ghostCoverage,
	}
	if len(out) == 0 {
		result["message"] = "the fleet reports no computers at all"
	}
	return result, nil
}
