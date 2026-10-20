package tools

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGhostNodesReportsOnlineStateAndActiveSessions(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(
			ghostNode("OFFICEPC", "live", true, "1.1.60"),
			ghostNode("LAPTOP", "offline", false, "1.1.58"),
		)).
		on("GET", "/history/sessions", func(ev ghostEvent) (int, string) {
			require.Equal(t, "OFFICEPC", ev.Query["node_id"], "only the online node is listed")
			return 200, ghostSessionsBody("OFFICEPC", "",
				ghostSession(ghostSessionA, 5*time.Minute),
				ghostSession(ghostSessionB, 2*time.Hour),
				ghostSession(ghostSessionC, -1),
			)
		})
	deps, _ := ghostDeps(f)
	inv := ghostOwnerInvocation("ghost_nodes", "", map[string]any{})
	res := newTestRegistry(t, deps).Invoke(context.Background(), inv)
	require.True(t, res.OK, "%+v", res.Error)

	require.Equal(t, 2, res.Output["total"])
	require.Equal(t, 1, res.Output["online"])
	require.Equal(t, "retained_only", res.Output["coverage"])
	nodes := res.Output["nodes"].([]map[string]any)
	require.Len(t, nodes, 2)
	// Sorted by id: LAPTOP before OFFICEPC.
	laptop, office := nodes[0], nodes[1]
	require.Equal(t, "LAPTOP", laptop["node"])
	require.Equal(t, false, laptop["online"])
	require.Equal(t, 0, laptop["activeSessions"])
	require.Equal(t, true, laptop["activeSessionsKnown"])
	require.Equal(t, "1.1.58", laptop["agentVersion"])

	require.Equal(t, "OFFICEPC", office["node"])
	require.Equal(t, true, office["online"])
	require.Equal(t, "1.1.60", office["agentVersion"])
	require.Equal(t, 1, office["activeSessions"], "only the session updated inside the 30-minute window counts")
	require.Equal(t, 3, office["retainedSessions"])
	require.Equal(t, true, office["activeSessionsKnown"])
	require.NotContains(t, office, "activeSessionsPartial")

	require.Len(t, f.callsTo("GET", "/history/sessions"), 1, "offline nodes are never listed")
}

func TestGhostNodesOfflineFleetMakesNoSessionCalls(t *testing.T) {
	f := newGhostFake().static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "offline", false, "1.1.60")))
	deps, _ := ghostDeps(f)
	out, terr := handleGhostNodes(context.Background(), deps, ghostOwnerInvocation("ghost_nodes", "", nil), map[string]any{})
	require.Nil(t, terr)
	require.Equal(t, 0, out["online"])
	nodes := out["nodes"].([]map[string]any)
	require.Len(t, nodes, 1)
	require.Equal(t, false, nodes[0]["online"])
	require.Equal(t, "offline", nodes[0]["state"])
	require.Empty(t, f.callsTo("GET", "/history/sessions"))
}

func TestGhostNodesEmptyFleet(t *testing.T) {
	f := newGhostFake().static("GET", "/nodes", 200, ghostNodesBody())
	deps, _ := ghostDeps(f)
	out, terr := handleGhostNodes(context.Background(), deps, ghostOwnerInvocation("ghost_nodes", "", nil), map[string]any{})
	require.Nil(t, terr)
	require.Equal(t, 0, out["total"])
	require.Equal(t, 0, out["online"])
	require.Len(t, out["nodes"], 0)
	require.Contains(t, out["message"], "no computers")
}

func TestGhostNodesFilterAndUnknownNode(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"), ghostNode("LAPTOP", "live", true, "1.1.60"))).
		static("GET", "/history/sessions", 200, ghostSessionsBody("OFFICEPC", ""))
	deps, _ := ghostDeps(f)
	// Case-insensitive match on the spoken name, exact id in the output.
	out, terr := handleGhostNodes(context.Background(), deps, ghostOwnerInvocation("ghost_nodes", "", nil), map[string]any{"node": "officepc"})
	require.Nil(t, terr)
	nodes := out["nodes"].([]map[string]any)
	require.Len(t, nodes, 1)
	require.Equal(t, "OFFICEPC", nodes[0]["node"])

	_, terr = handleGhostNodes(context.Background(), deps, ghostOwnerInvocation("ghost_nodes", "", nil), map[string]any{"node": "BASEMENT"})
	require.NotNil(t, terr)
	require.Equal(t, CodeNotFound, terr.Code)
}

func TestGhostNodesSessionListingFailureIsReportedNotHidden(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		static("GET", "/history/sessions", 500, `{"error":"boom"}`)
	deps, _ := ghostDeps(f)
	out, terr := handleGhostNodes(context.Background(), deps, ghostOwnerInvocation("ghost_nodes", "", nil), map[string]any{})
	require.Nil(t, terr, "the node list is still the answer")
	nodes := out["nodes"].([]map[string]any)
	require.Equal(t, false, nodes[0]["activeSessionsKnown"])
	require.Contains(t, nodes[0]["activeSessionsNote"], "could not be read")
}

func TestGhostNodesUpstreamErrorAndNotConfigured(t *testing.T) {
	f := newGhostFake().static("GET", "/nodes", 500, `{}`)
	deps, _ := ghostDeps(f)
	_, terr := handleGhostNodes(context.Background(), deps, ghostOwnerInvocation("ghost_nodes", "", nil), map[string]any{})
	require.NotNil(t, terr)
	require.Equal(t, CodeUpstreamError, terr.Code)

	_, terr = handleGhostNodes(context.Background(), newTestDeps(), ghostOwnerInvocation("ghost_nodes", "", nil), map[string]any{})
	require.NotNil(t, terr)
	require.Equal(t, CodeNotConfigured, terr.Code)
}

// Every ghost tool is owner-only; a member never reaches the handler and
// never costs the fleet a call.
func TestGhostToolsRequireOwner(t *testing.T) {
	f := newGhostFake().static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60")))
	deps, fake := ghostDeps(f)
	r := newTestRegistry(t, deps)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"ghost_nodes", map[string]any{}},
		{"ghost_sessions", map[string]any{}},
		{"ghost_session_summary", map[string]any{"node": "OFFICEPC"}},
		{"ghost_run_status", map[string]any{"eventId": "e", "runId": "r"}},
		{"ghost_run_start", map[string]any{"repo": "JeremyProffittOrg/live-ninja", "instructions": "tighten the retry logic", "confirm": true}},
		{"ghost_session_command", map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "PAUSE", "confirm": true}},
	} {
		inv := ghostOwnerInvocation(tc.tool, "member-key", tc.args)
		inv.Role = "member"
		res := r.Invoke(context.Background(), inv)
		requireToolError(t, res, CodeForbidden)
		require.Nil(t, fake.RawItem("IDEMP#user-1#member-key", "IDEMP"), "%s: a refused call must not burn the key", tc.tool)
	}
	require.Empty(t, f.calls, "a refused call never reaches the fleet")
}

func TestGhostToolsAreRegisteredAndServerExecuted(t *testing.T) {
	names := map[string]*Definition{}
	for _, def := range definitions() {
		names[def.Name] = def
	}
	for _, want := range []string{"ghost_nodes", "ghost_sessions", "ghost_session_summary", "ghost_run_status"} {
		def, ok := names[want]
		require.Truef(t, ok, "%s is not in definitions()", want)
		require.True(t, def.OwnerOnly, "%s must be owner-only", want)
		require.False(t, def.SideEffecting, "%s only reads", want)
		require.False(t, def.DeviceLocal)
		require.Empty(t, def.Surfaces)
	}
	for _, want := range []string{"ghost_run_start", "ghost_session_command"} {
		def, ok := names[want]
		require.Truef(t, ok, "%s is not in definitions()", want)
		require.True(t, def.OwnerOnly, "%s must be owner-only", want)
		require.True(t, def.SideEffecting, "%s must claim an idempotency key", want)
		require.False(t, def.DeviceLocal)
		require.Empty(t, def.Surfaces)
		hasConfirm := false
		for _, p := range def.Params {
			if p.Name == "confirm" && p.Type == "boolean" {
				hasConfirm = true
			}
		}
		require.True(t, hasConfirm, "%s needs a confirm flag", want)
	}
	// Owner decision 2026-10-19: no kill.
	for _, def := range definitions() {
		for _, p := range def.Params {
			for _, e := range p.Enum {
				require.NotEqual(t, "KILL", e, "%s offers KILL", def.Name)
			}
		}
	}
}
