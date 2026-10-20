package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
)

func ghostCommandFake() *ghostFake {
	return newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		static("GET", "/history/sessions", 200, ghostSessionsBody("OFFICEPC", "",
			ghostSession(ghostSessionA, time.Minute), ghostSession(ghostSessionB, time.Hour))).
		static("POST", "/command", 202, `{"command_id":"019a7c3e-cmd"}`)
}

func TestGhostSessionCommandSendsEachAllowedType(t *testing.T) {
	for _, tc := range []struct {
		command, text string
	}{
		{ghost.CommandNudge, "run the tests before you push"},
		{ghost.CommandAnswer, "Yes"},
		{ghost.CommandPause, ""},
		{ghost.CommandResume, ""},
	} {
		t.Run(tc.command, func(t *testing.T) {
			f := ghostCommandFake()
			deps, fake := ghostDeps(f)
			r := newTestRegistry(t, deps)
			args := map[string]any{"node": "officepc", "sessionId": ghostSessionA, "command": tc.command}
			if tc.text != "" {
				args["text"] = tc.text
			}
			id := ghostPropose(t, r, "ghost_session_command", "propose-"+tc.command, args)
			require.Empty(t, f.callsTo("POST", "/command"), "a proposal sends nothing")
			res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_session_command", "cmd-"+tc.command, ghostConfirmArgs(args, id)))
			require.True(t, res.OK, "%+v", res.Error)
			require.Equal(t, "accepted", res.Output["status"])
			require.Equal(t, "accepted", res.Output["result"])
			require.Equal(t, "019a7c3e-cmd", res.Output["commandId"])
			require.Equal(t, tc.command, res.Output["command"])
			require.Equal(t, "OFFICEPC", res.Output["node"])
			require.Equal(t, ghostSessionA, res.Output["sessionId"])
			require.Equal(t, "asynchronous", res.Output["delivery"])

			sent := f.callsTo("POST", "/command")
			require.Len(t, sent, 1)
			body := sent[0].bodyMap(t)
			require.Equal(t, tc.command, body["type"])
			require.Equal(t, "OFFICEPC", body["node"])
			params := body["params"].(map[string]any)
			require.Equal(t, ghostSessionA, params["session_id"])
			if tc.text == "" {
				require.NotContains(t, params, "text")
			} else {
				require.Equal(t, tc.text, params["text"])
			}
			require.NotNil(t, fake.RawItem("IDEMP#user-1#cmd-"+tc.command, "IDEMP"), "a sent command keeps its claim")
			require.Nil(t, ghostProposalRow(fake, id), "the proposal is consumed by the send")
		})
	}
}

// Same rule as ghost_run_start: confirm=true is honoured only with the
// server-issued proposalId for exactly these arguments. A lone confirm, an
// unknown id or changed text sends nothing, on every surface.
func TestGhostSessionCommandRefusesAnUnboundConfirm(t *testing.T) {
	for _, surface := range []string{"web", "android", "mcp"} {
		t.Run(surface, func(t *testing.T) {
			// /launch/repos serves the run proposal used for the cross-check.
			f := ghostCommandFake().static("GET", "/launch/repos", 200, ghostRunRepos)
			deps, fake := ghostDeps(f)
			r := newTestRegistry(t, deps)
			base := map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "NUDGE", "text": "push it"}

			lone := ghostOwnerInvocation("ghost_session_command", "lone-"+surface, map[string]any{
				"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "NUDGE", "text": "push it", "confirm": true})
			lone.Surface = surface
			res := r.Invoke(context.Background(), lone)
			terr := requireToolError(t, res, CodeInvalidArgs)
			require.Contains(t, terr.Message, "proposalId")
			require.Empty(t, f.callsTo("POST", "/command"), "a lone confirm=true never sends")
			require.Nil(t, fake.RawItem("IDEMP#user-1#lone-"+surface, "IDEMP"))

			id := ghostPropose(t, r, "ghost_session_command", "p-"+surface, base)
			changed := ghostConfirmArgs(base, id)
			changed["text"] = "rm -rf the worktree"
			inv := ghostOwnerInvocation("ghost_session_command", "changed-"+surface, changed)
			inv.Surface = surface
			res = r.Invoke(context.Background(), inv)
			terr = requireToolError(t, res, CodeInvalidArgs)
			require.Contains(t, terr.Message, "differ from the proposal")
			require.Empty(t, f.callsTo("POST", "/command"))
			require.NotNil(t, ghostProposalRow(fake, id), "a refused confirmation leaves the proposal confirmable")

			// A run proposal cannot confirm a command, even for the same user.
			runID := ghostPropose(t, r, "ghost_run_start", "run-p-"+surface, ghostRunArgs(nil))
			res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_session_command", "cross-"+surface, ghostConfirmArgs(base, runID)))
			requireToolError(t, res, CodeNotFound)
			require.Empty(t, f.callsTo("POST", "/command"))

			inv = ghostOwnerInvocation("ghost_session_command", "exact-"+surface, ghostConfirmArgs(base, id))
			inv.Surface = surface
			res = r.Invoke(context.Background(), inv)
			require.True(t, res.OK, "%+v", res.Error)
			require.Len(t, f.callsTo("POST", "/command"), 1)
		})
	}
}

// POST /command is not on ghost-cli's internal-invoke allowlist until
// ghost-command-allowlist ships there; ghost-cli then refuses the route with a
// FunctionError before its router runs. The tool must say so honestly
// (not_configured, "not enabled on the fleet service yet"), never "not found"
// or an unknown upstream outcome, and must release the idempotency claim
// because nothing was sent.
func TestGhostSessionCommandNotEnabledOnTheFleetService(t *testing.T) {
	f := ghostCommandFake().fnError("POST", "/command", ghostAllowlistRefusal("POST", "/command"))
	deps, fake := ghostDeps(f)
	r := newTestRegistry(t, deps)
	args := map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "PAUSE"}
	id := ghostPropose(t, r, "ghost_session_command", "allow-p", args)
	res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_session_command", "allow-1", ghostConfirmArgs(args, id)))
	terr := requireToolError(t, res, CodeNotConfigured)
	require.Contains(t, terr.Message, "session commands are not enabled on the fleet service yet")
	require.Len(t, f.callsTo("POST", "/command"), 1, "the refusal comes from the fleet service, so the route was tried once")
	require.Nil(t, fake.RawItem("IDEMP#user-1#allow-1", "IDEMP"), "a definite refusal releases the claim")
}

func TestGhostSessionCommandRequiresConfirmation(t *testing.T) {
	f := ghostCommandFake()
	deps, fake := ghostDeps(f)
	res := newTestRegistry(t, deps).Invoke(context.Background(), ghostOwnerInvocation("ghost_session_command", "propose-1",
		map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "NUDGE", "text": "please run the tests"}))
	terr := requireToolError(t, res, CodeConfirmationRequired)
	require.Equal(t, "ghost_session_command", terr.Details["operation"])
	id, _ := terr.Details["proposalId"].(string)
	require.Regexp(t, `^[a-f0-9]{32}$`, id)
	require.NotNil(t, ghostProposalRow(fake, id))
	require.Equal(t, "ghost_session_command", itemString(ghostProposalRow(fake, id), "operation"))
	proposed := terr.Details["proposed"].(map[string]any)
	require.Equal(t, "OFFICEPC", proposed["node"])
	require.Equal(t, ghostSessionA, proposed["sessionId"])
	require.Equal(t, "NUDGE", proposed["command"])
	require.Equal(t, "please run the tests", proposed["text"])
	require.Empty(t, f.callsTo("POST", "/command"))
	require.Nil(t, fake.RawItem("IDEMP#user-1#propose-1", "IDEMP"))
}

func TestGhostSessionCommandRejectsKillAndOtherTypes(t *testing.T) {
	f := ghostCommandFake()
	deps, fake := ghostDeps(f)
	r := newTestRegistry(t, deps)
	for _, typ := range []string{"KILL", "LAUNCH", "SAVE_LOCAL", "nudge", "KILL_ALL"} {
		res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_session_command", "rejected-"+typ,
			map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA, "command": typ, "confirm": true}))
		terr := requireToolError(t, res, CodeInvalidArgs)
		require.Contains(t, terr.Message, "command")
		require.Nil(t, fake.RawItem("IDEMP#user-1#rejected-"+typ, "IDEMP"), "schema rejection happens before the claim")
	}
	require.Empty(t, f.calls, "a rejected type never reaches the fleet")
}

func TestGhostSessionCommandTextRules(t *testing.T) {
	f := ghostCommandFake()
	deps, _ := ghostDeps(f)
	r := newTestRegistry(t, deps)
	for name, args := range map[string]map[string]any{
		"nudge without text":  {"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "NUDGE", "confirm": true},
		"answer without text": {"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "ANSWER", "confirm": true},
		"pause with text":     {"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "PAUSE", "text": "x", "confirm": true},
		"multi-line nudge":    {"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "NUDGE", "text": "a\nb", "confirm": true},
		"too long":            {"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "NUDGE", "text": strings.Repeat("x", 281), "confirm": true},
		"bad session shape":   {"node": "OFFICEPC", "sessionId": "latest", "command": "PAUSE", "confirm": true},
	} {
		res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_session_command", "rule-"+name, args))
		requireToolError(t, res, CodeInvalidArgs)
	}
	require.Empty(t, f.callsTo("POST", "/command"))
}

func TestGhostSessionCommandUnknownSessionAndNode(t *testing.T) {
	f := ghostCommandFake()
	deps, fake := ghostDeps(f)
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_session_command", "unknown-session",
		map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionC, "command": "PAUSE", "confirm": true}))
	terr := requireToolError(t, res, CodeNotFound)
	require.Contains(t, terr.Message, ghostSessionC)
	require.Contains(t, terr.Message, "ghost_sessions")
	require.Empty(t, f.callsTo("POST", "/command"))
	require.Nil(t, fake.RawItem("IDEMP#user-1#unknown-session", "IDEMP"), "nothing was sent, so the key is released")
	listings := len(f.callsTo("GET", "/history/sessions"))

	res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_session_command", "unknown-node",
		map[string]any{"node": "BASEMENT", "sessionId": ghostSessionA, "command": "PAUSE", "confirm": true}))
	requireToolError(t, res, CodeNotFound)
	require.Empty(t, f.callsTo("POST", "/command"))
	require.Len(t, f.callsTo("GET", "/history/sessions"), listings, "an unknown node is refused before its sessions are listed")
}

func TestGhostSessionCommandFindsSessionsOnLaterPages(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		on("GET", "/history/sessions", func(ev ghostEvent) (int, string) {
			if ev.Query["cursor"] == "" {
				return 200, ghostSessionsBody("OFFICEPC", "page2", ghostSession(ghostSessionA, time.Minute))
			}
			return 200, ghostSessionsBody("OFFICEPC", "", ghostSession(ghostSessionB, time.Minute))
		}).
		static("POST", "/command", 202, `{"command_id":"c2"}`)
	deps, _ := ghostDeps(f)
	r := newTestRegistry(t, deps)
	args := map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionB, "command": "RESUME"}
	id := ghostPropose(t, r, "ghost_session_command", "page2-p", args)
	res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_session_command", "page2-1", ghostConfirmArgs(args, id)))
	require.True(t, res.OK, "%+v", res.Error)
	require.Len(t, f.callsTo("GET", "/history/sessions"), 4, "both calls walk to the second page")
	require.Len(t, f.callsTo("POST", "/command"), 1)
}

func TestGhostSessionCommandMapsUpstreamRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		// held: the IDEMP# claim survives. Only upstream_error holds it — the
		// command WAS sent and its outcome is unknown, so a redelivery must not
		// publish it twice (idempotencyReleasable). The others prove nothing
		// was accepted, so the key is released for the retry.
		held bool
	}{
		{"forbidden", 403, CodeForbidden, false},
		{"invalid", 400, CodeInvalidArgs, false},
		{"unavailable", 503, CodeNotConfigured, false},
		// A 429 is a definite refusal (nothing published), so it must release.
		{"quota", 429, CodeNotConfigured, false},
		{"publish failed", 502, CodeUpstreamError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ghostCommandFake().static("POST", "/command", tc.status, `{"error":"x"}`)
			deps, fake := ghostDeps(f)
			r := newTestRegistry(t, deps)
			args := map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "PAUSE"}
			id := ghostPropose(t, r, "ghost_session_command", "proposal-"+tc.name, args)
			res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_session_command", "refused-"+tc.name, ghostConfirmArgs(args, id)))
			requireToolError(t, res, tc.code)
			marker := fake.RawItem("IDEMP#user-1#refused-"+tc.name, "IDEMP")
			if tc.held {
				require.NotNil(t, marker, "an unknown upstream outcome keeps the claim (at-most-once)")
			} else {
				require.Nil(t, marker, "a refusal before acceptance releases the claim")
			}
		})
	}
}

func TestGhostSessionCommandDuplicateDeliveryIsNotResent(t *testing.T) {
	f := ghostCommandFake()
	deps, _ := ghostDeps(f)
	r := newTestRegistry(t, deps)
	args := map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "NUDGE", "text": "go on"}
	id := ghostPropose(t, r, "ghost_session_command", "dup-p", args)
	inv := ghostOwnerInvocation("ghost_session_command", "dup-1", ghostConfirmArgs(args, id))
	first := r.Invoke(context.Background(), inv)
	require.True(t, first.OK, "%+v", first.Error)
	second := r.Invoke(context.Background(), inv)
	require.True(t, second.OK)
	require.True(t, second.Duplicate)
	require.Len(t, f.callsTo("POST", "/command"), 1)
}
