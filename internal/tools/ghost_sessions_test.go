package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	ghostSessionD = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	ghostSessionE = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
)

func ghostScheduleBody() string {
	b, _ := json.Marshal(map[string]any{"events": []map[string]any{
		{"event_id": "voice-update-live-ninja", "node": "OFFICEPC", "repo": "JeremyProffittOrg/live-ninja", "cli": "claude", "enabled": false,
			"last_run_id": "run-7", "last_run_status": "running", "last_run_ts": "2026-10-19T11:50:00Z", "runs": []map[string]any{}},
		{"event_id": "nightly-laptop", "node": "LAPTOP", "repo": "JeremyProffittOrg/ghost-cli", "cli": "codex", "enabled": true, "runs": []map[string]any{}},
	}})
	return string(b)
}

func TestGhostSessionsPagesThroughUpstreamPages(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"), ghostNode("LAPTOP", "live", true, "1.1.60"))).
		static("GET", "/schedule", 200, ghostScheduleBody()).
		on("GET", "/history/sessions", func(ev ghostEvent) (int, string) {
			require.Equal(t, "OFFICEPC", ev.Query["node_id"])
			switch ev.Query["cursor"] {
			case "":
				return 200, ghostSessionsBody("OFFICEPC", "up2",
					ghostSession(ghostSessionA, 5*time.Minute),
					ghostSession(ghostSessionB, 90*time.Minute),
					ghostSession(ghostSessionC, -1))
			case "up2":
				return 200, ghostSessionsBody("OFFICEPC", "",
					ghostSession(ghostSessionD, 2*time.Minute),
					ghostSession(ghostSessionE, 3*time.Hour))
			}
			t.Fatalf("unexpected upstream cursor %q", ev.Query["cursor"])
			return 500, ""
		})
	deps, _ := ghostDeps(f)
	r := newTestRegistry(t, deps)
	call := func(cursor string) map[string]any {
		args := map[string]any{"node": "OFFICEPC", "limit": float64(2)}
		if cursor != "" {
			args["cursor"] = cursor
		}
		res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_sessions", "", args))
		require.True(t, res.OK, "%+v", res.Error)
		return res.Output
	}
	ids := func(out map[string]any) []string {
		var got []string
		for _, s := range out["sessions"].([]map[string]any) {
			got = append(got, s["sessionId"].(string))
		}
		return got
	}

	// Page 1: two of the three sessions on the first upstream page.
	p1 := call("")
	require.Equal(t, []string{ghostSessionA, ghostSessionB}, ids(p1))
	require.NotEmpty(t, p1["nextCursor"])
	require.Equal(t, "node", p1["scope"])
	require.Equal(t, "OFFICEPC", p1["node"])
	require.Equal(t, "retained_only", p1["coverage"])
	first := p1["sessions"].([]map[string]any)
	require.Equal(t, ghostActivityActive, first[0]["activity"])
	require.Equal(t, ghostNow.Add(-5*time.Minute).Format(time.RFC3339), first[0]["lastActivity"])
	require.Equal(t, ghostActivityIdle, first[1]["activity"])
	require.Equal(t, "2026-10-19", first[0]["uploadDate"])
	// The first page carries the scheduled-run sidebar, scoped to this node.
	runs := p1["scheduledRuns"].([]map[string]any)
	require.Len(t, runs, 1)
	require.Equal(t, "JeremyProffittOrg/live-ninja", runs[0]["repo"])
	require.Equal(t, "running", runs[0]["lastRunStatus"])
	require.Equal(t, true, p1["scheduledRunsAvailable"])

	// Page 2: the rest of upstream page one, then into upstream page two.
	p2 := call(p1["nextCursor"].(string))
	require.Equal(t, []string{ghostSessionC, ghostSessionD}, ids(p2))
	require.NotEmpty(t, p2["nextCursor"])
	require.NotContains(t, p2, "scheduledRuns", "the sidebar is first-page only")
	third := p2["sessions"].([]map[string]any)[0]
	require.Equal(t, ghostActivityUnknown, third["activity"])
	require.NotContains(t, third, "lastActivity")

	// Page 3: the tail, and the end.
	p3 := call(p2["nextCursor"].(string))
	require.Equal(t, []string{ghostSessionE}, ids(p3))
	require.Equal(t, "", p3["nextCursor"])

	// The upstream cursor was carried through verbatim, once per upstream page.
	var cursors []string
	for _, ev := range f.callsTo("GET", "/history/sessions") {
		cursors = append(cursors, ev.Query["cursor"])
	}
	require.Equal(t, []string{"", "", "up2", "up2"}, cursors)
}

func TestGhostSessionsWalksTheWholeFleet(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"), ghostNode("LAPTOP", "offline", false, "1.1.60"))).
		static("GET", "/schedule", 200, ghostScheduleBody()).
		on("GET", "/history/sessions", func(ev ghostEvent) (int, string) {
			switch ev.Query["node_id"] {
			case "LAPTOP":
				return 200, ghostSessionsBody("LAPTOP", "", ghostSession(ghostSessionA, 4*time.Hour))
			case "OFFICEPC":
				return 200, ghostSessionsBody("OFFICEPC", "", ghostSession(ghostSessionB, time.Minute))
			}
			return 404, `{}`
		})
	deps, _ := ghostDeps(f)
	out, terr := handleGhostSessions(context.Background(), deps, ghostOwnerInvocation("ghost_sessions", "", nil), map[string]any{})
	require.Nil(t, terr)
	require.Equal(t, "fleet", out["scope"])
	sessions := out["sessions"].([]map[string]any)
	require.Len(t, sessions, 2)
	require.Equal(t, "LAPTOP", sessions[0]["node"])
	require.Equal(t, ghostSessionA, sessions[0]["sessionId"])
	require.Equal(t, "OFFICEPC", sessions[1]["node"])
	require.Equal(t, "", out["nextCursor"])
	require.Len(t, out["scheduledRuns"], 2, "fleet scope lists every node's scheduled runs")
}

func TestGhostSessionsUnknownNode(t *testing.T) {
	f := newGhostFake().static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60")))
	deps, _ := ghostDeps(f)
	res := newTestRegistry(t, deps).Invoke(context.Background(),
		ghostOwnerInvocation("ghost_sessions", "", map[string]any{"node": "BASEMENT"}))
	terr := requireToolError(t, res, CodeNotFound)
	require.Contains(t, terr.Message, "BASEMENT")
	require.Empty(t, f.callsTo("GET", "/history/sessions"))
}

func TestGhostSessionsUpstreamError(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		static("GET", "/history/sessions", 500, `{"error":"boom"}`)
	deps, _ := ghostDeps(f)
	res := newTestRegistry(t, deps).Invoke(context.Background(),
		ghostOwnerInvocation("ghost_sessions", "", map[string]any{"node": "OFFICEPC"}))
	requireToolError(t, res, CodeUpstreamError)

	// A fleet listing failure is an upstream error too.
	f2 := newGhostFake().static("GET", "/nodes", 502, `{}`)
	deps2, _ := ghostDeps(f2)
	res = newTestRegistry(t, deps2).Invoke(context.Background(), ghostOwnerInvocation("ghost_sessions", "", map[string]any{}))
	requireToolError(t, res, CodeUpstreamError)
}

func TestGhostSessionsRejectsForeignCursors(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"), ghostNode("LAPTOP", "live", true, "1.1.60"))).
		static("GET", "/history/sessions", 200, ghostSessionsBody("OFFICEPC", ""))
	deps, _ := ghostDeps(f)
	inv := ghostOwnerInvocation("ghost_sessions", "", nil)
	for name, args := range map[string]map[string]any{
		"garbage":       {"node": "OFFICEPC", "cursor": "not-a-cursor!"},
		"scope changed": {"cursor": encodeGhostCursor(ghostCursor{N: "OFFICEPC"})},
		"other node":    {"node": "LAPTOP", "cursor": encodeGhostCursor(ghostCursor{N: "OFFICEPC"})},
	} {
		_, terr := handleGhostSessions(context.Background(), deps, inv, args)
		require.NotNil(t, terr, name)
		require.Equal(t, CodeInvalidArgs, terr.Code, name)
	}
	require.Empty(t, f.callsTo("GET", "/history/sessions"))
}

func TestGhostSessionsScheduledRunsFailureDoesNotFailTheListing(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		static("GET", "/schedule", 503, `{}`).
		static("GET", "/history/sessions", 200, ghostSessionsBody("OFFICEPC", "", ghostSession(ghostSessionA, time.Minute)))
	deps, _ := ghostDeps(f)
	out, terr := handleGhostSessions(context.Background(), deps, ghostOwnerInvocation("ghost_sessions", "", nil), map[string]any{"node": "OFFICEPC"})
	require.Nil(t, terr)
	require.Len(t, out["sessions"], 1)
	require.Equal(t, false, out["scheduledRunsAvailable"])
	require.Len(t, out["scheduledRuns"], 0)
}
