package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/codeupdate"
)

const ghostRunRepos = `{"repos":[{"repo":"JeremyProffittOrg/live-ninja"},{"repo":"JeremyProffittOrg/ghost-cli"}]}`

func TestAlexaGhostProposalCannotSurviveConversationOrSettingsChange(t *testing.T) {
	f := ghostRunFake(t)
	deps, _ := ghostDeps(f)
	r := newTestRegistry(t, deps)
	inv := ghostOwnerInvocation("ghost_run_start", "alexa-propose", ghostRunArgs(nil))
	inv.Surface, inv.Scope, inv.ConfirmationContext = "alexa", "alexa:chat", strings.Repeat("a", 64)
	inv.ConfirmationTurn = strings.Repeat("c", 64)
	proposal := r.Invoke(t.Context(), inv)
	terr := requireToolError(t, proposal, CodeConfirmationRequired)
	id := terr.Details["proposalId"].(string)
	inv.Args = ghostConfirmArgs(ghostRunArgs(nil), id)
	inv.IdempotencyKey = "same-utterance"
	inv.ConfirmedByUser = true
	requireToolError(t, r.Invoke(t.Context(), inv), CodeConfirmationRequired)
	require.Empty(t, f.callsTo("POST", "/schedule"))
	inv.ConfirmationTurn = strings.Repeat("d", 64)
	inv.ConfirmedByUser = false
	inv.IdempotencyKey = "different-question"
	requireToolError(t, r.Invoke(t.Context(), inv), CodeConfirmationRequired)
	require.Empty(t, f.callsTo("POST", "/schedule"))
	inv.ConfirmedByUser = true
	inv.IdempotencyKey = "new-session-or-settings"
	inv.ConfirmationContext = strings.Repeat("b", 64)
	requireToolError(t, r.Invoke(t.Context(), inv), CodeInvalidArgs)
	require.Empty(t, f.callsTo("POST", "/schedule"))
	inv.ConfirmationContext = strings.Repeat("a", 64)
	inv.IdempotencyKey = "same-context"
	require.True(t, r.Invoke(t.Context(), inv).OK)
	require.Len(t, f.callsTo("POST", "/schedule"), 1)
}

func ghostRunFake(t *testing.T) *ghostFake {
	t.Helper()
	return newGhostFake().
		static("GET", "/launch/repos", 200, ghostRunRepos).
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		on("POST", "/schedule", func(ev ghostEvent) (int, string) {
			var body map[string]any
			require.NoError(t, json.Unmarshal([]byte(ev.Body), &body))
			b, _ := json.Marshal(map[string]any{"event_id": body["event_id"], "run": map[string]any{
				"run_id": "run-1", "node": body["node"], "status": "queued", "ts": "2026-10-19T12:00:00Z", "command_id": "cmd-1"}})
			return 202, string(b)
		})
}

func ghostRunArgs(overrides map[string]any) map[string]any {
	args := map[string]any{
		"repo":         "JeremyProffittOrg/live-ninja",
		"instructions": "tighten the retry logic on the Bedrock client and run the suite",
	}
	for k, v := range overrides {
		args[k] = v
	}
	return args
}

func TestGhostRunStartRequiresConfirmationAndReleasesTheKey(t *testing.T) {
	f := ghostRunFake(t)
	deps, fake := ghostDeps(f)
	r := newTestRegistry(t, deps)
	res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "proposal-1", ghostRunArgs(nil)))
	terr := requireToolError(t, res, CodeConfirmationRequired)
	require.Equal(t, "ghost_run_start", terr.Details["operation"])
	proposed := terr.Details["proposed"].(map[string]any)
	require.Equal(t, "JeremyProffittOrg/live-ninja", proposed["repo"])
	require.Equal(t, "OFFICEPC", proposed["node"])
	require.Equal(t, true, proposed["nodeOnline"])
	require.Equal(t, "claude", proposed["agent"])
	require.Equal(t, true, proposed["deploy"], "owner decision 2026-08-01: confirmed work ships by default")
	require.Equal(t, false, proposed["preprocess"])
	require.Contains(t, proposed["instructions"], "tighten the retry logic")
	require.Empty(t, f.callsTo("POST", "/schedule"), "a proposal never launches")
	require.Nil(t, fake.RawItem("IDEMP#user-1#proposal-1", "IDEMP"), "the claim is released so the confirmed call can use a fresh key")

	// The proposal is server-issued and stored, bound to this user and these
	// arguments, for 15 minutes.
	id, _ := terr.Details["proposalId"].(string)
	require.Regexp(t, `^[a-f0-9]{32}$`, id)
	require.Equal(t, ghostNow.Add(ghostProposalTTL).UTC().Format(time.RFC3339), terr.Details["proposalExpires"])
	require.Contains(t, terr.Details["confirmWith"], "proposalId")
	row := ghostProposalRow(fake, id)
	require.NotNil(t, row, "the proposal row is written before the model hears the proposal")
	require.Equal(t, "ghost_run_start", itemString(row, "operation"))
	require.Equal(t, "user-1", itemString(row, "userId"))
	require.NotEmpty(t, itemString(row, "hash"))

	// confirm=false explicitly is still a proposal.
	res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "proposal-2", ghostRunArgs(map[string]any{"confirm": false})))
	requireToolError(t, res, CodeConfirmationRequired)
	require.Empty(t, f.callsTo("POST", "/schedule"))
}

// A model-supplied confirm=true is not authority on any surface: without the
// server-issued proposalId the call is refused before anything is launched,
// and a well-formed but unknown proposalId is refused the same way. This is
// what keeps a first call with confirm=true, an injected transcript or a
// misheard turn from starting a run.
func TestGhostRunStartRefusesAnUnboundConfirm(t *testing.T) {
	for _, surface := range []string{"web", "android", "mcp"} {
		t.Run(surface, func(t *testing.T) {
			f := ghostRunFake(t)
			deps, fake := ghostDeps(f)
			r := newTestRegistry(t, deps)

			inv := ghostOwnerInvocation("ghost_run_start", "lone-"+surface, ghostRunArgs(map[string]any{"confirm": true}))
			inv.Surface = surface
			res := r.Invoke(context.Background(), inv)
			terr := requireToolError(t, res, CodeInvalidArgs)
			require.Contains(t, terr.Message, "proposalId")
			require.Empty(t, f.callsTo("POST", "/schedule"), "a lone confirm=true never launches")
			require.Nil(t, fake.RawItem("IDEMP#user-1#lone-"+surface, "IDEMP"), "nothing happened, so the key is released")

			inv = ghostOwnerInvocation("ghost_run_start", "unknown-"+surface,
				ghostRunArgs(map[string]any{"confirm": true, "proposalId": "0123456789abcdef0123456789abcdef"}))
			inv.Surface = surface
			res = r.Invoke(context.Background(), inv)
			terr = requireToolError(t, res, CodeNotFound)
			require.Contains(t, terr.Message, "unknown or was already used")
			require.Empty(t, f.callsTo("POST", "/schedule"))

			// A malformed id is refused at the same gate.
			res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "bad-id-"+surface,
				ghostRunArgs(map[string]any{"confirm": true, "proposalId": "yes"})))
			requireToolError(t, res, CodeInvalidArgs)
			require.Empty(t, f.callsTo("POST", "/schedule"))
		})
	}
}

// The proposal binds the exact arguments that were read back. A confirmed
// call whose repo, deploy flag, instructions, agent, node, model or effort
// differ is refused, launches nothing and leaves the proposal confirmable with
// the right arguments; another account cannot confirm it at all.
func TestGhostRunStartProposalIsBoundToItsArguments(t *testing.T) {
	f := ghostRunFake(t)
	deps, fake := ghostDeps(f)
	r := newTestRegistry(t, deps)
	base := ghostRunArgs(nil)
	id := ghostPropose(t, r, "ghost_run_start", "p-1", base)

	for name, change := range map[string]map[string]any{
		"deploy":       {"deploy": false},
		"instructions": {"instructions": "tighten the retry logic on the Bedrock client and push to main"},
		"agent":        {"agent": "codex"},
		"repo":         {"repo": "JeremyProffittOrg/ghost-cli"},
		"model":        {"model": "opus"},
		"effort":       {"effort": "high"},
	} {
		args := ghostConfirmArgs(base, id)
		for k, v := range change {
			args[k] = v
		}
		res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "changed-"+name, args))
		terr := requireToolError(t, res, CodeInvalidArgs)
		require.Containsf(t, terr.Message, "differ from the proposal", "%s: %s", name, terr.Message)
		require.Empty(t, f.callsTo("POST", "/schedule"), "%s: a changed confirmation never launches", name)
		require.Nil(t, fake.RawItem("IDEMP#user-1#changed-"+name, "IDEMP"))
		require.NotNil(t, ghostProposalRow(fake, id), "%s: a refused confirmation does not consume the proposal", name)
	}

	// Another account presenting the same id is told it is unknown.
	other := ghostOwnerInvocation("ghost_run_start", "foreign-1", ghostConfirmArgs(base, id))
	other.UserID = "someone-else"
	res := r.Invoke(context.Background(), other)
	requireToolError(t, res, CodeNotFound)
	require.Empty(t, f.callsTo("POST", "/schedule"))

	// Spoken variants of the same values still match: the binding uses the
	// resolved repo and node ids, and explicit defaults equal implicit ones.
	args := ghostConfirmArgs(base, id)
	args["node"] = "officepc"
	args["agent"] = "claude"
	args["deploy"] = true
	res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "exact-1", args))
	require.True(t, res.OK, "%+v", res.Error)
	require.Len(t, f.callsTo("POST", "/schedule"), 1)
	require.Nil(t, ghostProposalRow(fake, id), "a confirmed proposal is consumed")
}

// A proposal lasts 15 minutes and is usable once.
func TestGhostRunStartProposalExpiresAndIsSingleUse(t *testing.T) {
	f := ghostRunFake(t)
	deps, fake := ghostDeps(f)
	r := newTestRegistry(t, deps)
	base := ghostRunArgs(nil)

	stale := ghostPropose(t, r, "ghost_run_start", "p-stale", base)
	deps.Now = func() time.Time { return ghostNow.Add(ghostProposalTTL + time.Second) }
	res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "late-1", ghostConfirmArgs(base, stale)))
	terr := requireToolError(t, res, CodeNotFound)
	require.Contains(t, terr.Message, "expired")
	require.Empty(t, f.callsTo("POST", "/schedule"))
	require.Nil(t, fake.RawItem("IDEMP#user-1#late-1", "IDEMP"))

	fresh := ghostPropose(t, r, "ghost_run_start", "p-fresh", base)
	require.NotEqual(t, stale, fresh)
	res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "ok-1", ghostConfirmArgs(base, fresh)))
	require.True(t, res.OK, "%+v", res.Error)
	require.Len(t, f.callsTo("POST", "/schedule"), 1)

	// The same proposal under a new idempotency key is spent.
	res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "ok-2", ghostConfirmArgs(base, fresh)))
	terr = requireToolError(t, res, CodeNotFound)
	require.Contains(t, terr.Message, "already used")
	require.Len(t, f.callsTo("POST", "/schedule"), 1, "a spent proposal cannot launch twice")
}

func TestGhostRunStartLaunchesOnceAndTreatsRedeliveryAsDuplicate(t *testing.T) {
	f := ghostRunFake(t)
	deps, fake := ghostDeps(f)
	r := newTestRegistry(t, deps)
	id := ghostPropose(t, r, "ghost_run_start", "proposal-1", ghostRunArgs(nil))
	inv := ghostOwnerInvocation("ghost_run_start", "confirmed-1", ghostConfirmArgs(ghostRunArgs(nil), id))

	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK, "%+v", res.Error)
	require.Equal(t, "launched", res.Output["status"])
	require.Equal(t, "run-1", res.Output["runId"])
	require.Equal(t, codeupdate.EventID("JeremyProffittOrg/live-ninja"), res.Output["eventId"])
	require.Equal(t, "queued", res.Output["runStatus"])
	require.Equal(t, "cmd-1", res.Output["commandId"])
	require.Equal(t, "OFFICEPC", res.Output["node"])
	require.Equal(t, true, res.Output["deploy"])
	require.NotEmpty(t, res.Output["requestId"])
	require.NotContains(t, res.Output, "recordSaved", "no CODEUPD# store wired in this test")

	launches := f.callsTo("POST", "/schedule")
	require.Len(t, launches, 1)
	body := launches[0].bodyMap(t)
	require.Equal(t, codeupdate.EventID("JeremyProffittOrg/live-ninja"), body["event_id"])
	require.Equal(t, "OFFICEPC", body["node"])
	require.Equal(t, "JeremyProffittOrg/live-ninja", body["repo"])
	require.Equal(t, "claude", body["cli"])
	require.Equal(t, true, body["run_now"])
	require.Equal(t, true, body["deploy"])
	require.Equal(t, codeupdate.DefaultOutputFile, body["output_file"])
	prompt, _ := body["prompt"].(string)
	require.Contains(t, prompt, "tighten the retry logic on the Bedrock client")
	require.Contains(t, prompt, "Repository: JeremyProffittOrg/live-ninja")
	require.Contains(t, prompt, codeupdate.OutputDirective(codeupdate.DefaultOutputFile))
	require.Contains(t, prompt, "The owner authorized a DEPLOY")
	require.NotContains(t, prompt, "Bearer", "no progress token is minted on this path")
	require.Equal(t, res.Output["requestId"], launches[0].CorrelationID, "the request id is the audit correlation")
	require.NotNil(t, fake.RawItem("IDEMP#user-1#confirmed-1", "IDEMP"), "a successful launch keeps its claim")
	require.Nil(t, ghostProposalRow(fake, id), "the proposal is consumed by the launch")

	// The same confirmed call delivered again is a duplicate, not a second run.
	again := r.Invoke(context.Background(), inv)
	require.True(t, again.OK)
	require.True(t, again.Duplicate)
	require.Equal(t, "duplicate", again.Output["status"])
	require.Len(t, f.callsTo("POST", "/schedule"), 1)
}

func TestGhostRunStartHoldsThePushWhenAsked(t *testing.T) {
	f := ghostRunFake(t)
	deps, _ := ghostDeps(f)
	r := newTestRegistry(t, deps)
	args := ghostRunArgs(map[string]any{"deploy": false, "agent": "codex", "node": "officepc"})
	id := ghostPropose(t, r, "ghost_run_start", "held-p", args)
	res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "held-1", ghostConfirmArgs(args, id)))
	require.True(t, res.OK, "%+v", res.Error)
	body := f.callsTo("POST", "/schedule")[0].bodyMap(t)
	require.Equal(t, false, body["deploy"])
	require.Equal(t, "codex", body["cli"])
	require.Equal(t, "OFFICEPC", body["node"], "the spoken name resolves to the exact node id")
	require.Contains(t, body["prompt"], "DO NOT PUSH")
}

func TestGhostRunStartUnknownRepoAndNode(t *testing.T) {
	f := ghostRunFake(t)
	deps, fake := ghostDeps(f)
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "unknown-repo",
		ghostRunArgs(map[string]any{"repo": "JeremyProffittOrg/nope", "confirm": true})))
	terr := requireToolError(t, res, CodeNotFound)
	require.Contains(t, terr.Message, "JeremyProffittOrg/nope")
	require.Contains(t, terr.Message, "code_update_repos")
	require.Empty(t, f.callsTo("POST", "/schedule"))
	require.Nil(t, fake.RawItem("IDEMP#user-1#unknown-repo", "IDEMP"), "nothing happened, so the key is released")

	res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "unknown-node",
		ghostRunArgs(map[string]any{"node": "BASEMENT", "confirm": true})))
	requireToolError(t, res, CodeNotFound)
	require.Empty(t, f.callsTo("POST", "/schedule"))

	// Malformed arguments never reach the fleet at all.
	res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "bad-repo",
		ghostRunArgs(map[string]any{"repo": "live ninja", "confirm": true})))
	requireToolError(t, res, CodeInvalidArgs)
	res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_start", "bad-agent",
		ghostRunArgs(map[string]any{"agent": "grok", "confirm": true})))
	requireToolError(t, res, CodeInvalidArgs)

	// Without an idempotency key the registry refuses before the handler.
	inv := ghostOwnerInvocation("ghost_run_start", "", ghostRunArgs(map[string]any{"confirm": true}))
	res = r.Invoke(context.Background(), inv)
	requireToolError(t, res, CodeInvalidArgs)
	require.Contains(t, res.Error.Message, "idempotencyKey")
}

func TestGhostRunStartMapsLaunchRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   string
		// held: the IDEMP# claim survives. Only upstream_error holds it — the
		// launch WAS attempted and its outcome is unknown, so a redelivery must
		// not start a second run (idempotencyReleasable). The others prove
		// nothing launched, so the key is released for the retry.
		held bool
	}{
		{"in progress", 409, `{"error":"a run is already in progress"}`, CodeAlreadyExists, false},
		{"not authorized", 403, `{"error":"forbidden"}`, CodeForbidden, false},
		{"publisher unavailable", 503, `{}`, CodeNotConfigured, false},
		// A 429 is a definite refusal (nothing launched), so it must release.
		{"quota", 429, `{"error":"quota"}`, CodeNotConfigured, false},
		{"upstream", 500, `{}`, CodeUpstreamError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ghostRunFake(t).static("POST", "/schedule", tc.status, tc.body)
			deps, fake := ghostDeps(f)
			r := newTestRegistry(t, deps)
			id := ghostPropose(t, r, "ghost_run_start", "proposal-"+tc.name, ghostRunArgs(nil))
			res := r.Invoke(context.Background(),
				ghostOwnerInvocation("ghost_run_start", "refused-"+tc.name, ghostConfirmArgs(ghostRunArgs(nil), id)))
			requireToolError(t, res, tc.code)
			if tc.status == 429 {
				require.Contains(t, res.Error.Message, "try again")
			}
			marker := fake.RawItem("IDEMP#user-1#refused-"+tc.name, "IDEMP")
			if tc.held {
				require.NotNil(t, marker, "an unknown upstream outcome keeps the claim (at-most-once)")
			} else {
				require.Nil(t, marker, "a refusal before launch releases the claim")
			}
		})
	}
}

func TestGhostRunStartRecordsTheRunWhenAStoreIsWired(t *testing.T) {
	f := ghostRunFake(t)
	deps, _ := ghostDeps(f)
	db := &cuDDB{}
	deps.CodeUpdate = codeupdate.NewStore(db, "live-ninja-test", nil)
	r := newTestRegistry(t, deps)
	id := ghostPropose(t, r, "ghost_run_start", "recorded-p", ghostRunArgs(nil))
	res := r.Invoke(context.Background(),
		ghostOwnerInvocation("ghost_run_start", "recorded-1", ghostConfirmArgs(ghostRunArgs(nil), id)))
	require.True(t, res.OK, "%+v", res.Error)
	require.Equal(t, true, res.Output["recordSaved"])
	require.Len(t, db.puts, 1)
	item := db.puts[0]
	require.Equal(t, "USER#user-1", itemString(item, "pk"))
	require.Equal(t, codeupdate.SortKey(res.Output["requestId"].(string)), itemString(item, "sk"))
	require.Equal(t, codeupdate.StatusLaunched, itemString(item, "status"))
	require.Equal(t, "run-1", itemString(item, "runId"))
	require.Equal(t, codeupdate.EventID("JeremyProffittOrg/live-ninja"), itemString(item, "eventId"))
	require.Equal(t, ghostRunNoteNotRewritten, itemString(item, "rewriteNote"))
}

func TestGhostRunStatusReadsTheRunBack(t *testing.T) {
	eventID := codeupdate.EventID("JeremyProffittOrg/live-ninja")
	schedule, _ := json.Marshal(map[string]any{"events": []map[string]any{{
		"event_id": eventID, "node": "OFFICEPC", "repo": "JeremyProffittOrg/live-ninja", "cli": "claude",
		"last_run_id": "run-1", "last_run_status": "running", "last_run_ts": "2026-10-19T12:01:00Z",
		"runs": []map[string]any{{"run_id": "run-1", "status": "running", "ts": "2026-10-19T12:00:00Z", "node": "OFFICEPC", "trigger": "run_now", "summary": ""}},
	}}})
	f := newGhostFake().static("GET", "/schedule", 200, string(schedule))
	deps, _ := ghostDeps(f)
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_status", "", map[string]any{"eventId": eventID, "runId": "run-1"}))
	require.True(t, res.OK, "%+v", res.Error)
	require.Equal(t, "running", res.Output["status"])
	require.Equal(t, "OFFICEPC", res.Output["node"])
	require.Equal(t, "JeremyProffittOrg/live-ninja", res.Output["repo"])
	require.Equal(t, "claude", res.Output["agent"])
	require.Equal(t, "run_now", res.Output["trigger"])
	require.Equal(t, "running", res.Output["lastRunStatus"])
	require.NotContains(t, res.Output, "summary")

	res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_status", "", map[string]any{"eventId": eventID, "runId": "run-9"}))
	requireToolError(t, res, CodeNotFound)

	// Without a store a requestId cannot be resolved.
	res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_status", "", map[string]any{"requestId": "20261019T120000Z-gabc"}))
	requireToolError(t, res, CodeInvalidArgs)
}

// ghostScheduledRunsBody is a schedule where the newest visible run is the
// nightly Windows app build, one event never ran, one ran on LAPTOP and one
// names a computer outside the owner's fleet (BASEMENT), which ScheduledWork
// must drop however recent it is.
func ghostScheduledRunsBody() string {
	b, _ := json.Marshal(map[string]any{"events": []map[string]any{
		{"event_id": "voice-update-live-ninja", "node": "OFFICEPC", "repo": "JeremyProffittOrg/live-ninja", "cli": "claude", "enabled": false,
			"last_run_id": "run-7", "last_run_status": "done", "last_run_ts": "2026-10-18T09:00:00Z", "runs": []map[string]any{}},
		{"event_id": "nightly-windows-app", "node": "OFFICEPC", "repo": "JeremyProffittOrg/windows-app", "cli": "codex", "enabled": true,
			"last_run_id": "run-22", "last_run_status": "succeeded", "last_run_ts": "2026-10-19T03:00:00Z", "last_run_node": "OFFICEPC",
			"runs": []map[string]any{{"run_id": "run-22", "status": "succeeded", "ts": "2026-10-19T03:00:00Z", "node": "OFFICEPC", "trigger": "schedule", "summary": "Built and published 1.4.2."}}},
		{"event_id": "never-ran", "node": "OFFICEPC", "repo": "JeremyProffittOrg/event", "cli": "claude", "enabled": true, "runs": []map[string]any{}},
		{"event_id": "laptop-docs", "node": "LAPTOP", "repo": "JeremyProffittOrg/docs", "cli": "claude", "enabled": true,
			"last_run_id": "run-30", "last_run_status": "failed", "last_run_ts": "2026-10-19T10:00:00Z", "runs": []map[string]any{}},
		{"event_id": "secret-box", "node": "BASEMENT", "repo": "JeremyProffittOrg/secret", "cli": "claude", "enabled": true,
			"last_run_id": "run-99", "last_run_status": "done", "last_run_ts": "2026-10-19T11:00:00Z", "runs": []map[string]any{}},
	}})
	return string(b)
}

func ghostScheduledRunsFake(schedule string) *ghostFake {
	return newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"), ghostNode("LAPTOP", "offline", false, "1.1.60"))).
		static("GET", "/schedule", 200, schedule)
}

// "Did the nightly build run?" with no ids and no Live Ninja-launched run
// record reads the newest run on the ghost schedule instead of answering
// not_found (production 2026-10-24: the model said it "couldn't verify").
func TestGhostRunStatusWithoutIDsReadsTheNewestScheduledRun(t *testing.T) {
	for name, withStore := range map[string]bool{"no store": false, "store without a record": true} {
		t.Run(name, func(t *testing.T) {
			f := ghostScheduledRunsFake(ghostScheduledRunsBody())
			deps, _ := ghostDeps(f)
			if withStore {
				deps.CodeUpdate = codeupdate.NewStore(&cuDDB{}, "live-ninja-test", nil)
			}
			r := newTestRegistry(t, deps)
			res := r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_status", "", map[string]any{}))
			require.True(t, res.OK, "%+v", res.Error)
			out := res.Output
			require.Equal(t, "ghost_schedule", out["source"])
			require.Equal(t, "laptop-docs", out["eventId"], "newest visible run across the fleet")
			require.Equal(t, "failed", out["status"])
			require.NotContains(t, fmt.Sprint(out), "BASEMENT", "a run on a computer outside the fleet is never shown")
			require.NotContains(t, fmt.Sprint(out), "run-99")

			// Scoped to one computer: the nightly Windows build, with its summary.
			res = r.Invoke(context.Background(), ghostOwnerInvocation("ghost_run_status", "", map[string]any{"node": "officepc"}))
			require.True(t, res.OK, "%+v", res.Error)
			out = res.Output
			require.Equal(t, "ghost_schedule", out["source"])
			require.Equal(t, "nightly-windows-app", out["eventId"])
			require.Equal(t, "run-22", out["runId"])
			require.Equal(t, "succeeded", out["status"])
			require.Equal(t, "2026-10-19T03:00:00Z", out["ts"])
			require.Equal(t, "OFFICEPC", out["node"])
			require.Equal(t, "JeremyProffittOrg/windows-app", out["repo"])
			require.Equal(t, "codex", out["agent"])
			require.Equal(t, "schedule", out["trigger"])
			require.Equal(t, "Built and published 1.4.2.", out["summary"])
			recent := out["recentScheduledRuns"].([]map[string]any)
			require.Len(t, recent, 2, "only runs on OFFICEPC that actually ran")
			require.Equal(t, "nightly-windows-app", recent[0]["eventId"])
			require.Equal(t, "voice-update-live-ninja", recent[1]["eventId"])
			require.Empty(t, f.callsTo("GET", "/history/sessions"), "scheduled runs are never joined to sessions")
		})
	}
}

func TestGhostRunStatusWithoutAnyRunIsNotFound(t *testing.T) {
	empty, _ := json.Marshal(map[string]any{"events": []map[string]any{
		{"event_id": "never-ran", "node": "OFFICEPC", "repo": "JeremyProffittOrg/event", "cli": "claude", "enabled": true, "runs": []map[string]any{}},
	}})
	f := ghostScheduledRunsFake(string(empty))
	deps, _ := ghostDeps(f)
	deps.CodeUpdate = codeupdate.NewStore(&cuDDB{}, "live-ninja-test", nil)
	res := newTestRegistry(t, deps).Invoke(context.Background(), ghostOwnerInvocation("ghost_run_status", "", map[string]any{}))
	terr := requireToolError(t, res, CodeNotFound)
	require.Contains(t, terr.Message, "schedule")

	// An unknown computer is refused, not silently widened to the fleet.
	f2 := ghostScheduledRunsFake(ghostScheduledRunsBody())
	deps2, _ := ghostDeps(f2)
	res = newTestRegistry(t, deps2).Invoke(context.Background(), ghostOwnerInvocation("ghost_run_status", "", map[string]any{"node": "BASEMENT"}))
	requireToolError(t, res, CodeNotFound)
	require.Empty(t, f2.callsTo("GET", "/schedule"))
}

func TestGhostRunStatusResolvesARecordedRequest(t *testing.T) {
	eventID := codeupdate.EventID("JeremyProffittOrg/live-ninja")
	schedule, _ := json.Marshal(map[string]any{"events": []map[string]any{{
		"event_id": eventID, "node": "OFFICEPC", "repo": "JeremyProffittOrg/live-ninja", "cli": "claude",
		"last_run_id": "run-1", "last_run_status": "done", "last_run_ts": "2026-10-19T12:30:00Z",
		"runs": []map[string]any{{"run_id": "run-1", "status": "done", "ts": "2026-10-19T12:00:00Z", "node": "OFFICEPC", "trigger": "run_now", "summary": "Tests pass; pushed 1 commit."}},
	}}})
	f := newGhostFake().static("GET", "/schedule", 200, string(schedule))
	deps, _ := ghostDeps(f)
	db := &cuDDB{}
	deps.CodeUpdate = codeupdate.NewStore(db, "live-ninja-test", nil)
	require.NoError(t, deps.CodeUpdate.Put(context.Background(), codeupdate.Record{
		RequestID: "20261019T120000Z-gabc", UserID: "user-1", Status: codeupdate.StatusLaunched,
		Repo: "JeremyProffittOrg/live-ninja", Node: "OFFICEPC", CLI: "claude", Deploy: true,
		EventID: eventID, RunID: "run-1",
	}))
	db.lookup = db.puts[0]

	out, terr := handleGhostRunStatus(context.Background(), deps, ghostOwnerInvocation("ghost_run_status", "", nil),
		map[string]any{"requestId": "20261019T120000Z-gabc"})
	require.Nil(t, terr)
	require.Equal(t, "20261019T120000Z-gabc", out["requestId"])
	require.Equal(t, codeupdate.StatusLaunched, out["recordStatus"])
	require.Equal(t, "done", out["status"])
	require.Equal(t, "Tests pass; pushed 1 commit.", out["summary"])
	require.Equal(t, true, out["deploy"])

	// Another account cannot read it.
	inv := ghostOwnerInvocation("ghost_run_status", "", nil)
	inv.UserID = "someone-else"
	_, terr = handleGhostRunStatus(context.Background(), deps, inv, map[string]any{"requestId": "20261019T120000Z-gabc"})
	require.NotNil(t, terr)
	require.Equal(t, CodeNotFound, terr.Code)
}

// The CODEUPD# sort key must stay in time order beside the UUIDv7 ids the
// rest of the store holds (codeupdate.Store.Latest relies on lexical order),
// so the ghost request id is a UUIDv7-shaped id: millisecond prefix, version
// 7, RFC 4122 variant, and key-bound bits in place of the random ones.
func TestGhostRunRequestIDIsTimeOrderedAndKeyBound(t *testing.T) {
	a := ghostRunRequestID(ghostNow, Invocation{UserID: "user-1", IdempotencyKey: "k1"})
	b := ghostRunRequestID(ghostNow, Invocation{UserID: "user-1", IdempotencyKey: "k2"})
	c := ghostRunRequestID(ghostNow, Invocation{UserID: "user-2", IdempotencyKey: "k1"})
	require.NotEqual(t, a, b)
	require.NotEqual(t, a, c)
	require.Equal(t, a, ghostRunRequestID(ghostNow, Invocation{UserID: "user-1", IdempotencyKey: "k1"}), "deterministic for one claim")

	parsed, err := uuid.Parse(a)
	require.NoError(t, err, "%q must parse as a UUID", a)
	require.Equal(t, uuid.Version(7), parsed.Version())
	require.Equal(t, uuid.RFC4122, parsed.Variant())
	sec, nsec := parsed.Time().UnixTime()
	require.Equal(t, ghostNow.UnixMilli(), time.Unix(sec, nsec).UnixMilli(), "the first 48 bits are the millisecond")

	// Lexical order is time order, within this id family and against a real
	// UUIDv7 minted now (ghostNow is in the past relative to the test clock).
	later := ghostRunRequestID(ghostNow.Add(time.Millisecond), Invocation{UserID: "user-1", IdempotencyKey: "k1"})
	require.Less(t, a, later)
	require.Less(t, codeupdate.SortKey(a), codeupdate.SortKey(later))
	real7, err := uuid.NewV7()
	require.NoError(t, err)
	require.Less(t, codeupdate.SortKey(a), codeupdate.SortKey(real7.String()), "a ghost id from 2026-10-19 sorts before a UUIDv7 minted now")
}

// cuLatestDDB answers the CODEUPD# Latest query with the one stored record.
type cuLatestDDB struct{ *cuDDB }

func (f cuLatestDDB) Query(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if len(f.puts) == 0 {
		return &dynamodb.QueryOutput{}, nil
	}
	return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{f.puts[len(f.puts)-1]}}, nil
}

// With a Live Ninja run on record, the no-id call still reads it first, and
// lists the recent scheduled runs beside it so an older or different-repo
// record is not mistaken for the run the owner asked about.
func TestGhostRunStatusWithoutIDsListsScheduledRunsBesideTheRecord(t *testing.T) {
	f := ghostScheduledRunsFake(ghostScheduledRunsBody())
	deps, _ := ghostDeps(f)
	db := cuLatestDDB{&cuDDB{}}
	deps.CodeUpdate = codeupdate.NewStore(db, "live-ninja-test", nil)
	require.NoError(t, deps.CodeUpdate.Put(context.Background(), codeupdate.Record{
		RequestID: "0192f000-0000-7000-8000-000000000001", UserID: "user-1", Status: codeupdate.StatusLaunched,
		Repo: "JeremyProffittOrg/windows-app", Node: "OFFICEPC", CLI: "codex", Deploy: true,
		EventID: "nightly-windows-app", RunID: "run-22",
	}))
	out, terr := handleGhostRunStatus(context.Background(), deps, ghostOwnerInvocation("ghost_run_status", "", nil), map[string]any{})
	require.Nil(t, terr)
	require.Equal(t, "live_ninja", out["source"])
	require.Equal(t, "run-22", out["runId"])
	require.Equal(t, "succeeded", out["status"])
	require.Equal(t, true, out["recentScheduledRunsAvailable"])
	recent := out["recentScheduledRuns"].([]map[string]any)
	require.Equal(t, "laptop-docs", recent[0]["eventId"])
	require.NotContains(t, fmt.Sprint(out), "BASEMENT")
}

// A latest record that never launched still lists the scheduled runs beside
// its "not launched yet" note.
func TestGhostRunStatusUnlaunchedRecordListsScheduledRuns(t *testing.T) {
	f := ghostScheduledRunsFake(ghostScheduledRunsBody())
	deps, _ := ghostDeps(f)
	deps.CodeUpdate = codeupdate.NewStore(cuLatestDDB{&cuDDB{}}, "live-ninja-test", nil)
	require.NoError(t, deps.CodeUpdate.Put(context.Background(), codeupdate.Record{
		RequestID: "0192f000-0000-7000-8000-000000000002", UserID: "user-1", Status: codeupdate.StatusQueued,
		Repo: "JeremyProffittOrg/windows-app", Node: "OFFICEPC", CLI: "codex",
	}))
	out, terr := handleGhostRunStatus(context.Background(), deps, ghostOwnerInvocation("ghost_run_status", "", nil), map[string]any{})
	require.Nil(t, terr)
	require.Equal(t, "live_ninja", out["source"])
	require.Contains(t, out["note"], "not been launched")
	require.Equal(t, true, out["recentScheduledRunsAvailable"])
	require.Equal(t, "laptop-docs", out["recentScheduledRuns"].([]map[string]any)[0]["eventId"])
}
