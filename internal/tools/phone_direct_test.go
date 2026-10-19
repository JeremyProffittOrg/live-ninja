package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

func phoneInvocation(tool string, args map[string]any, call string) Invocation {
	inv := invocation(tool, args)
	inv.Surface, inv.Scope, inv.Role = "phone", PhoneScope, store.RoleOwner
	inv.SessionID, inv.CallID = "phone-session-1", call
	inv.IdempotencyKey = inv.SessionID + ":" + call
	return inv
}

func TestPhoneToolSetExcludesDeviceControlsAndOutgoingCalls(t *testing.T) {
	for _, name := range []string{"call_message", "device_control", "set_volume", "take_photo", "record_video", "play_media",
		"stop_listening", "start_new_conversation", "code_update_start", "profile_suggest", "web_research", "web_lookup",
		"file_create", "deliverable_create", "code_update_repos", "plan_upsert"} {
		require.False(t, PhoneToolAllowed(name), name)
	}
	for _, name := range []string{"memory_write", "send_email", "set_reminder", "job_create", "job_pause", "rule_save", "rule_delete"} {
		require.True(t, PhoneToolAllowed(name), name)
	}
	byName := map[string]*Definition{}
	for _, def := range definitions() {
		byName[def.Name] = def
	}
	for name := range phoneTools {
		def, ok := byName[name]
		require.True(t, ok, name)
		require.False(t, def.DeviceLocal, name)
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

	for _, tool := range []string{"call_message", "device_control"} {
		res := r.Invoke(ctx, phoneInvocation(tool, map[string]any{"message": "hi", "action": "on", "device": "lamp"}, "x-"+tool))
		require.NotNil(t, res.Error, tool)
		require.Equal(t, CodeForbidden, res.Error.Code, tool)
	}
	require.True(t, r.Invoke(ctx, phoneInvocation("job_list", nil, "read")).OK)
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

func TestPhoneManifestIsCompactAndMarksDirectActions(t *testing.T) {
	manifest := CatalogManifestForPhone()
	require.Len(t, manifest, len(phoneTools))
	for _, tool := range manifest {
		name := tool["name"].(string)
		description := tool["description"].(string)
		require.True(t, PhoneToolAllowed(name), name)
		require.LessOrEqual(t, len(description), 260, name)
		params := tool["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		_, hasConfirm := props["confirm"]
		direct := false
		for _, def := range definitions() {
			if def.Name == name {
				direct = def.PhoneDirect
			}
		}
		require.Equal(t, direct, hasConfirm, name)
		if direct {
			require.Contains(t, description, "confirm=true", name)
		}
	}
	require.Equal(t, "Absolute fire time, RFC3339 with offset, e.g. 2026-07-18T09:00:00-04:00.", firstSentence("Absolute fire time, RFC3339 with offset, e.g. 2026-07-18T09:00:00-04:00. More."))
}
