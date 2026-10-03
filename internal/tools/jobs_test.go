package tools

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/stretchr/testify/require"
)

func jobsToolFixture(t *testing.T) (*Registry, *Deps, *jobs.Job) {
	t.Helper()
	st, e := jobs.NewFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, e)
	t.Cleanup(func() { st.Close() })
	d := newTestDeps()
	d.Jobs = jobs.NewService(st)
	j, e := d.Jobs.Create(context.Background(), "user-1", jobs.Input{Title: "Exact saved review", Instructions: "Review the original quote", Kind: "review"}, "create-tool-fixture")
	require.NoError(t, e)
	return newTestRegistry(t, d), d, j
}

func TestJobToolsProposeExactActionsWithoutWriting(t *testing.T) {
	r, d, j := jobsToolFixture(t)
	ctx := context.Background()
	before, e := d.Jobs.ListHistory(ctx, "user-1", j.ID, jobs.HistoryQuery{})
	require.NoError(t, e)
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"job_create", map[string]any{"title": "New reminder", "kind": "reminder", "instructions": "Do not execute any code"}},
		{"job_start", map[string]any{"jobId": j.ID}},
		{"job_pause", map[string]any{"jobId": j.ID}},
		{"job_cancel", map[string]any{"jobId": j.ID}},
		{"job_command", map[string]any{"jobId": j.ID, "text": "Additional context only"}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			for n := 0; n < 2; n++ {
				inv := invocation(tc.tool, tc.args)
				inv.IdempotencyKey = "replayed-proposal"
				res := r.Invoke(ctx, inv)
				require.False(t, res.OK)
				require.False(t, res.Duplicate)
				require.Equal(t, CodeConfirmationRequired, res.Error.Code)
				require.Equal(t, tc.tool, res.Error.Details["operation"])
				p := res.Error.Details["proposed"].(map[string]any)
				require.NotContains(t, p, "requestId")
				if tc.tool != "job_create" {
					require.Equal(t, j.Version, p["expectedVersion"])
					require.Equal(t, j.ID, p["jobId"])
				}
			}
			attacker := map[string]any{}
			for k, v := range tc.args {
				attacker[k] = v
			}
			attacker["confirm"] = true
			res := r.Invoke(ctx, invocation(tc.tool, attacker))
			require.Equal(t, CodeInvalidArgs, res.Error.Code)
		})
	}
	after, e := d.Jobs.ListHistory(ctx, "user-1", j.ID, jobs.HistoryQuery{})
	require.NoError(t, e)
	require.Equal(t, before, after)
	current, e := d.Jobs.Get(ctx, "user-1", j.ID)
	require.NoError(t, e)
	require.Equal(t, j.Version, current.Version)
	list, e := d.Jobs.List(ctx, "user-1", 100, "")
	require.NoError(t, e)
	require.Len(t, list.Jobs, 1)
}

func TestJobVoiceReadsAreScopedPaginatedAndReauthorized(t *testing.T) {
	r, d, j := jobsToolFixture(t)
	ctx := context.Background()
	for n := 0; n < 3; n++ {
		_, e := d.Jobs.Create(ctx, "user-1", jobs.Input{Title: "Reminder", Kind: "reminder"}, string(rune('a'+n))+"create-request")
		require.NoError(t, e)
	}
	res := r.Invoke(ctx, invocation("job_list", map[string]any{"limit": 2}))
	require.True(t, res.OK)
	require.Len(t, res.Output["jobs"], 2)
	require.NotEmpty(t, res.Output["nextCursor"])
	res = r.Invoke(ctx, invocation("job_status", map[string]any{"jobId": j.ID}))
	require.True(t, res.OK)
	require.Equal(t, false, res.Output["externalExecutionConnected"])
	inv := invocation("job_status", map[string]any{"jobId": j.ID})
	inv.UserID = "another-user"
	res = r.Invoke(ctx, inv)
	require.Equal(t, CodeNotFound, res.Error.Code)
	d.Reauthorize = func(context.Context, string) error { return errors.New("revoked") }
	res = r.Invoke(ctx, invocation("job_start", map[string]any{"jobId": j.ID}))
	require.Equal(t, CodeForbidden, res.Error.Code)
}

func TestJobRetryProposalCarriesExactRunAndSchedulingFailsClosed(t *testing.T) {
	r, d, j := jobsToolFixture(t)
	ctx := context.Background()
	run, e := d.Jobs.RunNow(ctx, "user-1", j.ID, j.Version, "run-retry-proposal")
	require.NoError(t, e)
	j, e = d.Jobs.Get(ctx, "user-1", j.ID)
	require.NoError(t, e)
	_, e = d.Jobs.CancelRun(ctx, "user-1", j.ID, run.ID, j.Version, "cancel-review-run")
	require.NoError(t, e)
	res := r.Invoke(ctx, invocation("job_retry", map[string]any{"jobId": j.ID, "runId": run.ID}))
	require.Equal(t, CodeConfirmationRequired, res.Error.Code)
	snapshot := res.Error.Details["run"].(*jobs.Run)
	require.Equal(t, run.ID, snapshot.ID)
	require.Equal(t, run.Instructions, snapshot.Instructions)
	require.Equal(t, "cancelled", snapshot.Status)
	res = r.Invoke(ctx, invocation("job_create", map[string]any{"title": "Daily", "kind": "reminder", "scheduleKind": "daily", "timezone": "UTC", "time": "09:00"}))
	require.Equal(t, CodeNotConfigured, res.Error.Code)
	res = r.Invoke(ctx, invocation("job_create", map[string]any{"title": "Execute code", "kind": "coding"}))
	require.Equal(t, CodeInvalidArgs, res.Error.Code)
	res = r.Invoke(ctx, invocation("job_create", map[string]any{"title": "Forged", "kind": "reminder", "userId": "another"}))
	require.Equal(t, CodeInvalidArgs, res.Error.Code)
}

func TestJobStatusCanFollowRetainedRunCursorSeparatelyFromHistory(t *testing.T) {
	r, d, _ := jobsToolFixture(t)
	ctx := context.Background()
	j, e := d.Jobs.Create(ctx, "user-1", jobs.Input{Title: "Run history", Kind: "reminder"}, "status-paging-create")
	require.NoError(t, e)
	for n := 0; n < 13; n++ {
		_, e = d.Jobs.RunNow(ctx, "user-1", j.ID, j.Version, fmt.Sprintf("status-run-%03d", n))
		require.NoError(t, e)
		j, e = d.Jobs.Get(ctx, "user-1", j.ID)
		require.NoError(t, e)
	}
	first := r.Invoke(ctx, invocation("job_status", map[string]any{"jobId": j.ID, "limit": 2}))
	require.True(t, first.OK)
	require.Len(t, first.Output["runs"], 10)
	require.NotEmpty(t, first.Output["runsNextCursor"])
	second := r.Invoke(ctx, invocation("job_status", map[string]any{"jobId": j.ID, "runCursor": first.Output["runsNextCursor"], "limit": 2}))
	require.True(t, second.OK)
	require.Len(t, second.Output["runs"], 3)
	require.Empty(t, second.Output["runsNextCursor"])
	seen := map[string]bool{}
	for _, result := range []*Result{first, second} {
		for _, run := range result.Output["runs"].([]jobs.Run) {
			require.False(t, seen[run.ID], "run repeated across pages")
			seen[run.ID] = true
		}
	}
	require.Len(t, seen, 13)
	require.Equal(t, first.Output["history"], second.Output["history"], "run pagination must not move the history page")
}

func TestJobToolsManifestAndMissingConfiguration(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	for _, name := range []string{"job_list", "job_status", "job_create", "job_start", "job_pause", "job_resume", "job_cancel", "job_retry", "job_command"} {
		found := false
		for _, def := range definitions() {
			if def.Name == name {
				found = true
				require.False(t, def.SideEffecting)
			}
		}
		require.True(t, found, name)
	}
	res := r.Invoke(context.Background(), invocation("job_list", nil))
	require.Equal(t, CodeNotConfigured, res.Error.Code)
}
