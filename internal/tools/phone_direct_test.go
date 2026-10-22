package tools

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/websearch"
)

func phoneInvocation(tool string, args map[string]any, call string) Invocation {
	inv := invocation(tool, args)
	inv.Surface, inv.Scope, inv.Role = "phone", PhoneScope, store.RoleOwner
	inv.SessionID, inv.CallID = "phone-session-1", call
	inv.IdempotencyKey = inv.SessionID + ":" + call
	inv.Capabilities = []string{websearch.BudgetCapability}
	return inv
}

func TestPhoneToolSetMatchesServerCatalog(t *testing.T) {
	for _, name := range []string{"set_volume", "take_photo", "record_video", "play_media",
		"stop_listening", "start_new_conversation", "unknown", " memory_search", "MEMORY_SEARCH"} {
		require.False(t, PhoneToolAllowed(name), name)
	}
	for _, name := range []string{"memory_write", "send_email", "set_reminder", "job_create", "job_pause", "rule_save", "rule_delete",
		"ghost_nodes", "ghost_sessions", "ghost_session_summary", "ghost_run_status", "ghost_run_start", "ghost_session_command",
		"web_lookup", "web_research", "file_create", "deliverable_create", "code_update_repos", "code_update_start", "plan_upsert",
		"profile_suggest", "call_message", "device_control"} {
		require.True(t, PhoneToolAllowed(name), name)
	}
	for _, def := range definitions() {
		require.Equal(t, !def.DeviceLocal, PhoneToolAllowed(def.Name), def.Name)
	}
}

func TestPhoneScopeIsOwnerOnlyAndLimitedToPhoneTools(t *testing.T) {
	r, _, _ := jobsToolFixture(t)
	ctx := context.Background()

	member := phoneInvocation("job_list", nil, "member")
	member.Role = store.RoleMember
	res := r.Invoke(ctx, member)
	require.Equal(t, CodeForbidden, res.Error.Code)

	wrongSurface := phoneInvocation("job_list", nil, "surface")
	wrongSurface.Surface = "web"
	require.Equal(t, CodeForbidden, r.Invoke(ctx, wrongSurface).Error.Code)

	for _, tool := range []string{"set_volume", "take_photo"} {
		res := r.Invoke(ctx, phoneInvocation(tool, map[string]any{"message": "hi", "action": "on", "device": "lamp"}, "x-"+tool))
		require.NotNil(t, res.Error, tool)
		require.Equal(t, CodeForbidden, res.Error.Code, tool)
	}
	require.True(t, r.Invoke(ctx, phoneInvocation("job_list", nil, "read")).OK)
}

func TestPhoneWebSearchExecutesThroughOwnerCallScope(t *testing.T) {
	fx := newResearchFixture(t)
	d := newTestDeps()
	d.WebResearch = fx.search
	r := newTestRegistry(t, d)
	for _, name := range []string{"web_lookup", "web_research"} {
		t.Run(name, func(t *testing.T) {
			res := r.Invoke(t.Context(), phoneInvocation(name, map[string]any{"query": "recent security breach"}, name))
			require.True(t, res.OK, "%s must execute on an owner call: %+v", name, res.Error)
			if name == "web_lookup" {
				require.NotEmpty(t, res.Output["summary"])
				require.NotEmpty(t, res.Output["source"])
			} else {
				require.Len(t, res.Output["sources"], 2)
				require.Equal(t, "recent security breach", fx.lastQuery)
			}
		})
	}
}

func TestOldPhoneBridgeCannotStartAnUnreservedPaidSearch(t *testing.T) {
	fx := newResearchFixture(t)
	d := newTestDeps()
	d.WebResearch = fx.search
	r := newTestRegistry(t, d)
	inv := phoneInvocation("web_research", map[string]any{"query": "news"}, "old-bridge")
	inv.Capabilities = nil
	result := r.Invoke(t.Context(), inv)
	require.False(t, result.OK)
	require.Equal(t, CodeForbidden, result.Error.Code)
	require.Empty(t, fx.lastQuery, "the old bridge must not incur a search charge")
}

func TestPhoneJobCreateNeedsSpokenConfirmationThenExecutesOnce(t *testing.T) {
	r, d, _ := jobsToolFixture(t)
	ctx := context.Background()
	args := map[string]any{"title": "Call the plumber", "kind": "reminder", "instructions": "Ask about Tuesday"}

	res := r.Invoke(ctx, phoneInvocation("job_create", args, "ask"))
	require.False(t, res.OK)
	require.Equal(t, CodeConfirmationRequired, res.Error.Code)
	require.Equal(t, true, res.Error.Details["confirmOnCall"])
	list, e := d.Jobs.List(ctx, "user-1", 100, "")
	require.NoError(t, e)
	require.Len(t, list.Jobs, 1, "an unconfirmed phone request changes nothing")

	confirmed := map[string]any{"confirm": true}
	for k, v := range args {
		confirmed[k] = v
	}
	res = r.Invoke(ctx, phoneInvocation("job_create", confirmed, "yes"))
	require.True(t, res.OK, "%+v", res.Error)
	require.Equal(t, "created", res.Output["status"])

	replay := r.Invoke(ctx, phoneInvocation("job_create", confirmed, "yes"))
	require.True(t, replay.Duplicate, "the registry idempotency claim covers a confirmed phone action")
	list, e = d.Jobs.List(ctx, "user-1", 100, "")
	require.NoError(t, e)
	require.Len(t, list.Jobs, 2)
}

func TestPhoneJobActionsExecuteThroughJobsService(t *testing.T) {
	r, d, j := jobsToolFixture(t)
	ctx := context.Background()

	res := r.Invoke(ctx, phoneInvocation("job_command", map[string]any{"jobId": j.ID, "text": "Owner note from the call", "confirm": true}, "note"))
	require.True(t, res.OK, "%+v", res.Error)
	history, e := d.Jobs.ListHistory(ctx, "user-1", j.ID, jobs.HistoryQuery{})
	require.NoError(t, e)
	require.Contains(t, history.Entries[len(history.Entries)-1].Text, "Owner note from the call")

	res = r.Invoke(ctx, phoneInvocation("job_pause", map[string]any{"jobId": j.ID, "confirm": true}, "pause"))
	require.True(t, res.OK, "%+v", res.Error)
	current, e := d.Jobs.Get(ctx, "user-1", j.ID)
	require.NoError(t, e)
	require.Equal(t, "paused", current.Status)

	// Web review is unchanged: the same arguments only propose.
	web := invocation("job_resume", map[string]any{"jobId": j.ID, "confirm": true})
	web.IdempotencyKey = "web-resume"
	require.Equal(t, CodeConfirmationRequired, r.Invoke(ctx, web).Error.Code)
	current, e = d.Jobs.Get(ctx, "user-1", j.ID)
	require.NoError(t, e)
	require.Equal(t, "paused", current.Status)
}

func TestPhoneRuleSaveAndDeleteUseRuleReview(t *testing.T) {
	d := newTestDeps()
	require.NoError(t, d.Store.CreateUser(context.Background(), &store.User{UserID: "user-1", AmazonUserID: "amzn.user-1", Email: "owner@example.com", Role: store.RoleOwner, Status: store.UserStatusActive}))
	r := newTestRegistry(t, d)
	ctx := context.Background()
	args := map[string]any{"name": "trip-check", "description": "Load when the owner plans a trip.", "body": "Check the weather first."}

	res := r.Invoke(ctx, phoneInvocation("rule_save", args, "ask"))
	require.Equal(t, CodeConfirmationRequired, res.Error.Code)
	_, err := d.Store.GetRule(ctx, "user-1", "trip-check")
	require.ErrorIs(t, err, store.ErrNotFound)

	args["confirm"] = true
	res = r.Invoke(ctx, phoneInvocation("rule_save", args, "save"))
	require.True(t, res.OK, "%+v", res.Error)
	saved, err := d.Store.GetRule(ctx, "user-1", "trip-check")
	require.NoError(t, err)
	require.Equal(t, "Check the weather first.", saved.Body)
	require.True(t, saved.Enabled)

	args["body"] = "Check the weather and traffic."
	res = r.Invoke(ctx, phoneInvocation("rule_save", args, "update"))
	require.True(t, res.OK, "%+v", res.Error)
	saved, err = d.Store.GetRule(ctx, "user-1", "trip-check")
	require.NoError(t, err)
	require.Equal(t, "Check the weather and traffic.", saved.Body)

	res = r.Invoke(ctx, phoneInvocation("rule_delete", map[string]any{"name": "trip-check", "confirm": true}, "delete"))
	require.True(t, res.OK, "%+v", res.Error)
	_, err = d.Store.GetRule(ctx, "user-1", "trip-check")
	require.ErrorIs(t, err, store.ErrNotFound)

	res = r.Invoke(ctx, phoneInvocation("rule_delete", map[string]any{"name": "trip-check", "confirm": true}, "again"))
	require.Equal(t, CodeNotFound, res.Error.Code)
}

// Owner decision 2026-10-19 (ghost-phone-access, "read and control"): a
// recognized owner call reads the fleet freely and steers it only through the
// server-issued proposal. A spoken yes is modelled as confirm=true WITH the
// proposalId of the proposal the model read back; confirm=true alone, from a
// phone call like from every other surface, executes nothing.
func TestPhoneGhostToolsReadFreelyAndManageOnlyThroughAProposal(t *testing.T) {
	f := ghostRunFake(t).
		static("GET", "/history/sessions", 200, ghostSessionsBody("OFFICEPC", "", ghostSession(ghostSessionA, time.Minute))).
		static("POST", "/command", 202, `{"command_id":"019a7c3e-cmd"}`)
	deps, fake := ghostDeps(f)
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	for _, tool := range []string{"ghost_nodes", "ghost_sessions"} {
		res := r.Invoke(ctx, phoneInvocation(tool, nil, "read-"+tool))
		require.True(t, res.OK, "%s: %+v", tool, res.Error)
	}
	res := r.Invoke(ctx, phoneInvocation("ghost_run_status", nil, "read-status"))
	require.NotNil(t, res.Error)
	require.NotEqual(t, CodeForbidden, res.Error.Code, "the read tool is on the call; it has no run to report yet")

	args := map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA, "command": "NUDGE", "text": "run the tests before you push"}
	res = r.Invoke(ctx, phoneInvocation("ghost_session_command", args, "ask"))
	terr := requireToolError(t, res, CodeConfirmationRequired)
	id, _ := terr.Details["proposalId"].(string)
	require.Regexp(t, `^[a-f0-9]{32}$`, id)
	require.Equal(t, "ghost_session_command", terr.Details["operation"])
	require.Empty(t, f.callsTo("POST", "/command"), "a proposal sends nothing")

	lone := map[string]any{"confirm": true}
	for k, v := range args {
		lone[k] = v
	}
	res = r.Invoke(ctx, phoneInvocation("ghost_session_command", lone, "lone-yes"))
	requireToolError(t, res, CodeInvalidArgs)
	require.Empty(t, f.callsTo("POST", "/command"), "a spoken yes without the proposal sends nothing")

	res = r.Invoke(ctx, phoneInvocation("ghost_session_command", ghostConfirmArgs(args, id), "yes"))
	require.True(t, res.OK, "%+v", res.Error)
	require.Equal(t, "accepted", res.Output["status"])
	require.Len(t, f.callsTo("POST", "/command"), 1)
	require.Nil(t, ghostProposalRow(fake, id), "the proposal is consumed by the send")

	res = r.Invoke(ctx, phoneInvocation("ghost_session_command", ghostConfirmArgs(args, id), "yes-again"))
	requireToolError(t, res, CodeNotFound)
	require.Len(t, f.callsTo("POST", "/command"), 1, "a consumed proposal cannot be confirmed twice")

	runArgs := ghostRunArgs(nil)
	res = r.Invoke(ctx, phoneInvocation("ghost_run_start", runArgs, "run-ask"))
	terr = requireToolError(t, res, CodeConfirmationRequired)
	runID, _ := terr.Details["proposalId"].(string)
	require.Regexp(t, `^[a-f0-9]{32}$`, runID)
	require.Empty(t, f.callsTo("POST", "/schedule"), "a proposal launches nothing")

	res = r.Invoke(ctx, phoneInvocation("ghost_run_start", ghostConfirmArgs(runArgs, runID), "run-yes"))
	require.True(t, res.OK, "%+v", res.Error)
	require.Equal(t, "launched", res.Output["status"])
	require.Len(t, f.callsTo("POST", "/schedule"), 1)

}

func TestPhoneManifestSharesCompleteServerToolSchemas(t *testing.T) {
	manifest := CatalogManifestForPhone()
	require.Equal(t, CatalogManifestForServerExecution(), manifest)
	for _, tool := range manifest {
		name := tool["name"].(string)
		require.True(t, PhoneToolAllowed(name), name)
	}
}
