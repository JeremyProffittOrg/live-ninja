package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakePhoneResearch struct {
	targets    []PhoneTarget
	commands   []PhoneCommand
	next       PhoneResult
	statusErr  error
	advanceErr error
}

func (f *fakePhoneResearch) Catalog(_ context.Context, uid string) (PhoneCatalog, error) {
	if uid != "alice" {
		return PhoneCatalog{}, ErrForbidden
	}
	return PhoneCatalog{Targets: f.targets, BlockedReason: "Calls are stopped."}, nil
}
func (f *fakePhoneResearch) Advance(_ context.Context, c PhoneCommand) (PhoneResult, error) {
	f.commands = append(f.commands, c)
	return f.next, f.advanceErr
}
func (f *fakePhoneResearch) Status(_ context.Context, _, _, _ string) (PhoneResult, error) {
	return f.next, f.statusErr
}
func phoneFixture(t *testing.T) (*Service, *FileStore, *time.Time, *fakePhoneResearch, Input) {
	s, store, now := fixture(t)
	p := &fakePhoneResearch{}
	in := Input{Title: "Five California hotels", Kind: PhoneResearchKind, Instructions: PhoneResearchPurpose, Schedule: Schedule{Kind: "once"}}
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("hotel-%02d", i)
		p.targets = append(p.targets, PhoneTarget{ID: id, Name: "Hotel " + id, City: "California", Phone: fmt.Sprintf("+12025550%03d", i), SourceURL: "https://hotel.example/" + id})
		if i <= 5 {
			in.PhoneTargetIDs = append(in.PhoneTargetIDs, id)
		}
	}
	return s.WithPhoneResearch(p), store, now, p, in
}
func phoneResult(status string, version, command int64, ids []string) PhoneResult {
	r := PhoneResult{Status: status, Version: version, CommandVersion: command, Reason: status}
	for _, id := range ids {
		r.Properties = append(r.Properties, PhoneProperty{TargetID: id, Status: "not_started"})
	}
	return r
}

func TestPhoneResearchApprovalStopRequiresExplicitResume(t *testing.T) {
	s, _, now, p, in := phoneFixture(t)
	ctx := context.Background()
	j := create(t, s, in)
	require.Empty(t, p.commands)
	p.next = phoneResult("blocked", 1, 1, in.PhoneTargetIDs)
	run, e := s.RunNow(ctx, "alice", j.ID, j.Version, "approve-phone-1")
	require.NoError(t, e)
	require.Equal(t, "blocked", run.Status)
	require.Equal(t, "alice", run.ApprovedBy)
	require.Equal(t, stamp(now.Add(24*time.Hour)), run.ApprovalExpiresAt)
	require.Len(t, p.commands, 1)
	require.Equal(t, in.PhoneTargetIDs, p.commands[0].TargetIDs)
	_, e = s.RunNow(ctx, "alice", j.ID, j.Version, "approve-phone-1")
	require.NoError(t, e)
	require.Len(t, p.commands, 1, "duplicate approval cannot resume a blocked call")
	tick, e := s.Tick(ctx, now.Add(48*time.Hour), 100)
	require.NoError(t, e)
	require.Zero(t, tick.Processed)
	require.Len(t, p.commands, 1)
	j = current(t, s, j.ID)
	require.Equal(t, "paused", j.Status)
	p.next = phoneResult("running", 2, 2, in.PhoneTargetIDs)
	j, e = s.Action(ctx, "alice", j.ID, "resume", j.Version, "resume-phone-1")
	require.NoError(t, e)
	require.Equal(t, "running", j.LastRun.Status)
	require.Len(t, p.commands, 2)
	require.Equal(t, run.ApprovalExpiresAt, j.LastRun.ApprovalExpiresAt)
	_, e = s.ApproveRun(ctx, "alice", j.ID, run.ID, j.Version, "wrong-approval")
	require.ErrorIs(t, e, ErrInvalidState)
	_, e = s.CancelRun(ctx, "alice", j.ID, run.ID, j.Version, "wrong-cancel-1")
	require.ErrorIs(t, e, ErrInvalidState)
	_, e = s.Update(ctx, "alice", j.ID, in, j.Version, "active-edit-1")
	require.ErrorIs(t, e, ErrInvalidState)
}

func TestPhoneResearchControlsFenceStaleProviderEvents(t *testing.T) {
	s, _, _, p, in := phoneFixture(t)
	ctx := context.Background()
	j := create(t, s, in)
	p.next = phoneResult("running", 1, 1, in.PhoneTargetIDs)
	run, e := s.RunNow(ctx, "alice", j.ID, j.Version, "approve-phone-1")
	require.NoError(t, e)
	j = current(t, s, j.ID)
	p.next = phoneResult("paused", 2, 2, in.PhoneTargetIDs)
	j, e = s.Action(ctx, "alice", j.ID, "pause", j.Version, "pause-phone-1")
	require.NoError(t, e)
	require.Equal(t, "paused", j.LastRun.Status)
	p.next = phoneResult("running", 99, 1, in.PhoneTargetIDs)
	stale, e := s.SyncPhoneRun(ctx, "alice", j.ID, run.ID)
	require.NoError(t, e)
	require.Equal(t, "paused", stale.Status)
	p.next = phoneResult("cancelled", 3, 3, in.PhoneTargetIDs)
	j, e = s.Action(ctx, "alice", j.ID, "cancel", j.Version, "cancel-phone-1")
	require.NoError(t, e)
	require.Equal(t, "cancelled", j.LastRun.Status)
	p.next = phoneResult("running", 100, 3, in.PhoneTargetIDs)
	stale, e = s.SyncPhoneRun(ctx, "alice", j.ID, run.ID)
	require.NoError(t, e)
	require.Equal(t, "cancelled", stale.Status)
	_, e = s.SyncPhoneRun(ctx, "bob", j.ID, run.ID)
	require.ErrorIs(t, e, ErrNotFound)
}

func TestPhoneResearchTerminalEnrichmentHistoricalEditsAndCopies(t *testing.T) {
	s, store, _, p, in := phoneFixture(t)
	ctx := context.Background()
	j := create(t, s, in)
	p.next = phoneResult("succeeded", 1, 1, in.PhoneTargetIDs)
	for i := range p.next.Properties {
		p.next.Properties[i].Status = "completed"
		p.next.Properties[i].CallSID = fmt.Sprintf("CA%032x", i+1)
	}
	p.next.Properties[4].Status = "rejected"
	p.next.Properties[4].CallSID = ""
	run, e := s.RunNow(ctx, "alice", j.ID, j.Version, "approve-phone-1")
	require.NoError(t, e)
	require.Equal(t, "succeeded", run.Status)
	finished := run.FinishedAt
	p.next.Version = 2
	p.next.Properties[0].RecordingStatus = "ready"
	p.next.Properties[0].RecordingSource = "hotel-trial-2026-10-15"
	run, e = s.SyncPhoneRun(ctx, "alice", j.ID, run.ID)
	require.NoError(t, e)
	require.Equal(t, "ready", run.PhoneResearch.Properties[0].RecordingStatus)
	require.Equal(t, finished, run.FinishedAt)
	j = current(t, s, j.ID)
	in.Title = "Edited completed task"
	in.PhoneTargetIDs[4] = "hotel-06"
	j, e = s.Update(ctx, "alice", j.ID, in, j.Version, "edit-completed-1")
	require.NoError(t, e)
	p.next.Version = 3
	run, e = s.SyncPhoneRun(ctx, "alice", j.ID, run.ID)
	require.NoError(t, e)
	require.Equal(t, "hotel-05", run.PhoneResearch.Targets[4].ID)
	j, e = s.Get(ctx, "alice", j.ID)
	require.NoError(t, e)
	require.Equal(t, "hotel-06", j.PhoneResearch.Targets[4].ID)
	run.PhoneResearch.Targets[0].Name = "tampered return value"
	stored, e := store.Get(ctx, "alice", j.ID)
	require.NoError(t, e)
	require.NotEqual(t, "tampered return value", stored.Runs[0].PhoneResearch.Targets[0].Name)
	history, e := s.ListHistory(ctx, "alice", j.ID, HistoryQuery{Limit: 50})
	require.NoError(t, e)
	for _, entry := range history.Entries {
		if entry.Run != nil && entry.Run.PhoneResearch != nil {
			require.NotEqual(t, "tampered return value", entry.Run.PhoneResearch.Targets[0].Name)
		}
	}
}

func TestPhoneResearchRejectsChangedPurposeTargetsAndExpiredResume(t *testing.T) {
	s, _, now, p, in := phoneFixture(t)
	ctx := context.Background()
	bad := in
	bad.Instructions = "Call someone else"
	_, e := s.Create(ctx, "alice", bad, "bad-purpose-1")
	require.ErrorIs(t, e, ErrValidation)
	bad = in
	bad.PhoneTargetIDs = []string{"hotel-01", "hotel-01", "hotel-02", "hotel-03", "hotel-04"}
	_, e = s.Create(ctx, "alice", bad, "bad-targets-1")
	require.ErrorIs(t, e, ErrValidation)
	_, e = s.Create(ctx, "bob", in, "other-owner-1")
	require.ErrorIs(t, e, ErrForbidden)
	j := create(t, s, in)
	p.next = phoneResult("blocked", 1, 1, in.PhoneTargetIDs)
	_, e = s.RunNow(ctx, "alice", j.ID, j.Version, "approve-phone-1")
	require.NoError(t, e)
	*now = now.Add(25 * time.Hour)
	j = current(t, s, j.ID)
	_, e = s.Action(ctx, "alice", j.ID, "resume", j.Version, "expired-resume")
	require.ErrorIs(t, e, ErrInvalidState)
	require.Len(t, p.commands, 1)
}

type phoneWireLambda struct{ request map[string]any }

func (f *phoneWireLambda) Invoke(_ context.Context, in *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	if e := json.Unmarshal(in.Payload, &f.request); e != nil {
		return nil, e
	}
	body, _ := json.Marshal(phoneResult("blocked", 1, 1, []string{"hotel-01", "hotel-02", "hotel-03", "hotel-04", "hotel-05"}))
	outer, _ := json.Marshal(map[string]any{"statusCode": 200, "body": string(body)})
	return &lambda.InvokeOutput{Payload: outer}, nil
}
func TestPhoneResearchRealLambdaWireContract(t *testing.T) {
	client := &phoneWireLambda{}
	p, e := NewPhoneResearchProvider(client, "arn:aws:lambda:us-east-1:759775734231:function:live-ninja-phone-trial-recorder")
	require.NoError(t, e)
	command := PhoneCommand{OwnerUserID: "alice", JobID: "job", RunID: "run", Generation: 2, Command: "run", CommandVersion: 3, ApprovalExpiresAt: 12345, TargetIDs: []string{"hotel-01", "hotel-02", "hotel-03", "hotel-04", "hotel-05"}}
	r, e := p.Advance(context.Background(), command)
	require.NoError(t, e)
	require.Equal(t, "blocked", r.Status)
	require.Equal(t, "phone_research_advance", client.request["operation"])
	require.Equal(t, "alice", client.request["ownerUserId"])
	require.Equal(t, float64(3), client.request["commandVersion"])
	require.Len(t, client.request["targetIds"], 5)
	_, e = NewPhoneResearchProvider(client, "arn:aws:lambda:us-east-2:759775734231:function:other")
	require.Error(t, e)
}

func fixture(t *testing.T) (*Service, *FileStore, *time.Time) {
	t.Helper()
	fs, e := NewFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { fs.Close() })
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	s := NewService(fs)
	s.now = func() time.Time { return now }
	return s, fs, &now
}
func input(kind string) Input {
	return Input{Title: "Follow up", Instructions: "Review the quote", Kind: kind, Schedule: Schedule{Kind: "once"}}
}
func create(t *testing.T, s *Service, in Input) *Job {
	t.Helper()
	j, e := s.Create(context.Background(), "alice", in, "create-request-1")
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func current(t *testing.T, s *Service, id string) *Job {
	t.Helper()
	j, e := s.Get(context.Background(), "alice", id)
	if e != nil {
		t.Fatal(e)
	}
	return j
}

func TestDuplicateCreateAndUserIsolation(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	j := create(t, s, input("reminder"))
	again, e := s.Create(ctx, "alice", input("reminder"), "create-request-1")
	if e != nil || again.ID != j.ID {
		t.Fatalf("duplicate = %+v %v", again, e)
	}
	changed := input("reminder")
	changed.Title = "Different"
	if _, e = s.Create(ctx, "alice", changed, "create-request-1"); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if _, e = s.Get(ctx, "bob", j.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	p, e := s.List(ctx, "bob", 25, "")
	if e != nil || len(p.Jobs) != 0 {
		t.Fatalf("owner leak %+v %v", p, e)
	}
	if _, e = s.RunNow(ctx, "bob", j.ID, j.Version, "attack-req-1"); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	if _, e = s.Create(ctx, "alice", input("coding"), "unsupported-1"); !errors.Is(e, ErrValidation) {
		t.Fatal(e)
	}
}
func TestRepeatedRunClicksAndWorkerDeliverOnce(t *testing.T) {
	s, _, now := fixture(t)
	ctx := context.Background()
	j := create(t, s, input("reminder"))
	var wg sync.WaitGroup
	var fail atomic.Int32
	for n := 0; n < 32; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := s.RunNow(ctx, "alice", j.ID, j.Version, "same-click-request"); e != nil {
				fail.Add(1)
			}
		}()
	}
	wg.Wait()
	if fail.Load() != 0 {
		t.Fatalf("duplicate failures %d", fail.Load())
	}
	p, e := s.ListRuns(ctx, "alice", j.ID, 25, "")
	if e != nil || len(p.Runs) != 1 || p.Runs[0].Status != "succeeded" {
		t.Fatalf("runs=%+v err=%v", p, e)
	}
	for n := 0; n < 16; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := s.Tick(ctx, *now, 100); e != nil {
				fail.Add(1)
			}
		}()
	}
	wg.Wait()
	if fail.Load() != 0 {
		t.Fatal("worker error")
	}
	p, _ = s.ListRuns(ctx, "alice", j.ID, 25, "")
	if len(p.Runs) != 1 || p.Runs[0].Status != "succeeded" || p.Runs[0].Result == "" {
		t.Fatalf("receipt %+v", p)
	}
	if _, e = s.RetryRun(ctx, "alice", j.ID, p.Runs[0].ID, current(t, s, j.ID).Version, "retry-succeeded"); !errors.Is(e, ErrInvalidState) {
		t.Fatal(e)
	}
}
func TestReviewApprovalBoundToVersionAndExpiry(t *testing.T) {
	s, _, now := fixture(t)
	ctx := context.Background()
	j := create(t, s, input("review"))
	run, e := s.RunNow(ctx, "alice", j.ID, j.Version, "run-review-1")
	if e != nil {
		t.Fatal(e)
	}
	s.Tick(ctx, *now, 100)
	j = current(t, s, j.ID)
	if j.LastRun.Status != "waiting_approval" {
		t.Fatalf("%+v", j)
	}
	if _, e = s.ApproveRun(ctx, "alice", j.ID, run.ID, j.Version-1, "approval-stale"); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	approved, e := s.ApproveRun(ctx, "alice", j.ID, run.ID, j.Version, "approval-exact")
	if e != nil || approved.Status != "succeeded" || approved.ApprovedBy != "alice" {
		t.Fatalf("%+v %v", approved, e)
	}
	if _, e = s.ApproveRun(ctx, "alice", j.ID, run.ID, j.Version, "approval-exact"); e != nil {
		t.Fatal("lost response cannot replay", e)
	}
	j = current(t, s, j.ID)
	run, e = s.RunNow(ctx, "alice", j.ID, j.Version, "run-review-2")
	if e != nil {
		t.Fatal(e)
	}
	s.Tick(ctx, *now, 100)
	j = current(t, s, j.ID)
	*now = now.Add(24 * time.Hour)
	if _, e = s.ApproveRun(ctx, "alice", j.ID, run.ID, j.Version, "approval-expired"); !errors.Is(e, ErrInvalidState) {
		t.Fatal(e)
	}
	s.Tick(ctx, *now, 100)
	j = current(t, s, j.ID)
	if j.LastRun.Status != "failed" {
		t.Fatalf("expiry status %+v", j.LastRun)
	}
	retry, e := s.RetryRun(ctx, "alice", j.ID, run.ID, j.Version, "retry-expired")
	if e != nil || retry.Attempt != 2 || retry.RetryOf != run.ID {
		t.Fatalf("retry %+v %v", retry, e)
	}
}
func TestEditInvalidatesReviewAndStaleMutations(t *testing.T) {
	s, _, now := fixture(t)
	ctx := context.Background()
	j := create(t, s, input("review"))
	r, _ := s.RunNow(ctx, "alice", j.ID, j.Version, "run-review-edit")
	s.Tick(ctx, *now, 100)
	j = current(t, s, j.ID)
	in := input("review")
	in.Instructions = "Changed exact content"
	changed, e := s.Update(ctx, "alice", j.ID, in, j.Version, "update-review")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ApproveRun(ctx, "alice", j.ID, r.ID, changed.Version, "old-content-approve"); !errors.Is(e, ErrInvalidState) {
		t.Fatal(e)
	}
	if _, e = s.Action(ctx, "alice", j.ID, "cancel", j.Version, "stale-cancel"); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if _, e = s.RetryRun(ctx, "alice", j.ID, r.ID, changed.Version, "old-content-retry"); !errors.Is(e, ErrInvalidState) {
		t.Fatal(e)
	}
}
func TestRecurringRestartAndMissedRunCoalescing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	fs, e := NewFileStore(path)
	if e != nil {
		t.Fatal(e)
	}
	s := NewService(fs)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	in := input("reminder")
	in.Schedule = Schedule{Kind: "daily", Time: "13:00", Timezone: "UTC"}
	j := create(t, s, in)
	if fs.Close() != nil {
		t.Fatal("close")
	}
	fs, e = NewFileStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer fs.Close()
	s = NewService(fs)
	now = now.Add(72 * time.Hour)
	s.now = func() time.Time { return now }
	result, e := s.Tick(context.Background(), now, 100)
	if e != nil || result.Processed != 1 {
		t.Fatalf("%+v %v", result, e)
	}
	j = current(t, s, j.ID)
	if j.NextRunAt != "2026-10-06T13:00:00Z" || j.LastRun.ScheduledFor != "2026-10-03T13:00:00Z" {
		t.Fatalf("%+v", j)
	}
	result, e = s.Tick(context.Background(), now, 100)
	if e != nil || result.Processed != 0 {
		t.Fatalf("duplicate %+v %v", result, e)
	}
}
func TestPauseResumeCancelAndNoFutureFireOnEdit(t *testing.T) {
	s, _, now := fixture(t)
	ctx := context.Background()
	in := input("review")
	in.Schedule = Schedule{Kind: "once", At: stamp(now.Add(time.Hour))}
	j := create(t, s, in)
	in.Title = "Changed"
	j, e := s.Update(ctx, "alice", j.ID, in, j.Version, "edit-future")
	if e != nil {
		t.Fatal(e)
	}
	res, e := s.Tick(ctx, *now, 100)
	if e != nil || res.Processed != 0 {
		t.Fatalf("early tick %+v %v", res, e)
	}
	j, e = s.Action(ctx, "alice", j.ID, "pause", j.Version, "pause-future")
	if e != nil {
		t.Fatal(e)
	}
	*now = now.Add(2 * time.Hour)
	res, e = s.Tick(ctx, *now, 100)
	if e != nil || res.Processed != 0 {
		t.Fatal("paused fired")
	}
	j, e = s.Action(ctx, "alice", j.ID, "resume", j.Version, "resume-past")
	if e != nil || j.NextRunAt != "" {
		t.Fatalf("resume catchup %+v %v", j, e)
	}
	r, e := s.RunNow(ctx, "alice", j.ID, j.Version, "manual-after-resume")
	if e != nil {
		t.Fatal(e)
	}
	j = current(t, s, j.ID)
	j, e = s.Action(ctx, "alice", j.ID, "cancel", j.Version, "cancel-queued")
	if e != nil {
		t.Fatal(e)
	}
	s.Tick(ctx, *now, 100)
	p, _ := s.ListRuns(ctx, "alice", j.ID, 25, "")
	if len(p.Runs) != 1 || p.Runs[0].ID != r.ID || p.Runs[0].Status != "cancelled" {
		t.Fatalf("%+v", p)
	}
}
func TestCancelRunRetryAndBoundedHistory(t *testing.T) {
	s, _, now := fixture(t)
	ctx := context.Background()
	j := create(t, s, input("review"))
	r, e := s.RunNow(ctx, "alice", j.ID, j.Version, "run-cancel-retry")
	if e != nil {
		t.Fatal(e)
	}
	j = current(t, s, j.ID)
	if _, e = s.CancelRun(ctx, "alice", j.ID, r.ID, j.Version, "cancel-run-only"); e != nil {
		t.Fatal(e)
	}
	j = current(t, s, j.ID)
	retry, e := s.RetryRun(ctx, "alice", j.ID, r.ID, j.Version, "retry-run-only")
	if e != nil || retry.Attempt != 2 {
		t.Fatalf("%+v %v", retry, e)
	}
	s.Tick(ctx, *now, 100)
	j = current(t, s, j.ID)
	j, e = s.Update(ctx, "alice", j.ID, input("reminder"), j.Version, "change-for-history")
	if e != nil {
		t.Fatal(e)
	}
	for n := 0; n < 60; n++ {
		j = current(t, s, j.ID)
		if _, e = s.RunNow(ctx, "alice", j.ID, j.Version, fmt.Sprintf("many-run-%d", n)); e != nil {
			t.Fatal(e)
		}
		if _, e = s.Tick(ctx, *now, 100); e != nil {
			t.Fatal(e)
		}
	}
	p, e := s.ListRuns(ctx, "alice", j.ID, 100, "")
	if e != nil || len(p.Runs) != MaxRuns {
		t.Fatalf("%d %v", len(p.Runs), e)
	}
	p, e = s.ListRuns(ctx, "alice", j.ID, 10, "")
	if e != nil || len(p.Runs) != 10 || p.NextCursor == "" {
		t.Fatal("paging", e)
	}
	q, e := s.ListRuns(ctx, "alice", j.ID, 10, p.NextCursor)
	if e != nil || q.Runs[0].ID == p.Runs[0].ID {
		t.Fatal("overlap", e)
	}
}
func TestPendingHistoryCannotBeSilentlyEvicted(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	j := create(t, s, input("review"))
	for n := 0; n < MaxRuns; n++ {
		j = current(t, s, j.ID)
		if _, e := s.RunNow(ctx, "alice", j.ID, j.Version, fmt.Sprintf("pending-%03d", n)); e != nil {
			t.Fatal(e)
		}
	}
	j = current(t, s, j.ID)
	if _, e := s.RunNow(ctx, "alice", j.ID, j.Version, "pending-overflow"); !errors.Is(e, ErrLimit) {
		t.Fatal(e)
	}
}
func TestFileExclusiveLockAndStaleCAS(t *testing.T) {
	_, fs, _ := fixture(t)
	if duplicate, e := NewFileStore(fs.path); e == nil {
		duplicate.Close()
		t.Fatal("second process/instance lock accepted")
	}
	r := &Record{UserID: "a", Job: Job{ID: "job_12345678901234567890123456789012", Version: 1}}
	ctx := context.Background()
	if e := fs.CompareAndSwap(ctx, r, 0); e != nil {
		t.Fatal(e)
	}
	r.Job.Version = 2
	var success atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := fs.CompareAndSwap(ctx, r, 1); e == nil {
				success.Add(1)
			} else if !errors.Is(e, ErrConflict) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if success.Load() != 1 {
		t.Fatalf("CAS successes %d", success.Load())
	}
}

type gateStore struct {
	Store
	entered chan struct{}
	release chan struct{}
}

func (g gateStore) CompareAndSwap(ctx context.Context, r *Record, v int64) error {
	if len(r.Runs) > 0 && r.Runs[0].Status == "succeeded" {
		close(g.entered)
		<-g.release
	}
	return g.Store.CompareAndSwap(ctx, r, v)
}
func TestCancelWinsAgainstInFlightTick(t *testing.T) {
	s, fs, now := fixture(t)
	ctx := context.Background()
	in := input("reminder")
	in.Schedule = Schedule{Kind: "once", At: stamp(now.Add(time.Minute))}
	j := create(t, s, in)
	*now = now.Add(time.Minute)
	g := gateStore{Store: fs, entered: make(chan struct{}), release: make(chan struct{})}
	worker := NewService(g)
	done := make(chan error, 1)
	go func() { _, e := worker.Tick(ctx, *now, 100); done <- e }()
	<-g.entered
	j = current(t, s, j.ID)
	if _, e := s.Action(ctx, "alice", j.ID, "cancel", j.Version, "cancel-race-winner"); e != nil {
		t.Fatal(e)
	}
	close(g.release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	j = current(t, s, j.ID)
	if j.LastRun != nil || j.Status != "cancelled" {
		t.Fatalf("stale tick won %+v", j)
	}
}

func TestManualReviewExpiresAndRetriesWithoutWorker(t *testing.T) {
	s, _, now := fixture(t)
	ctx := context.Background()
	j := create(t, s, input("review"))
	r, e := s.RunNow(ctx, "alice", j.ID, j.Version, "manual-expire-run")
	if e != nil || r.Status != "waiting_approval" {
		t.Fatal(e)
	}
	*now = now.Add(24*time.Hour + time.Second)
	// No Tick: reading the interface must persist failure and offer Retry.
	page, e := s.List(ctx, "alice", 25, "")
	if e != nil || page.Jobs[0].LastRun.Status != "failed" {
		t.Fatalf("expiry hidden %+v %v", page, e)
	}
	j = current(t, s, j.ID)
	r, e = s.RetryRun(ctx, "alice", j.ID, r.ID, j.Version, "manual-expire-retry")
	if e != nil || r.Status != "waiting_approval" || r.Attempt != 2 {
		t.Fatalf("retry %+v %v", r, e)
	}
}

func TestDuplicateScheduledWorkersProduceSingleOccurrence(t *testing.T) {
	s, _, now := fixture(t)
	ctx := context.Background()
	in := input("reminder")
	in.Schedule = Schedule{Kind: "once", At: stamp(now.Add(time.Minute))}
	j := create(t, s, in)
	*now = now.Add(time.Minute)
	var wg sync.WaitGroup
	for n := 0; n < 24; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := s.Tick(ctx, *now, 100); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	p, e := s.ListRuns(ctx, "alice", j.ID, 25, "")
	if e != nil || len(p.Runs) != 1 || p.Runs[0].Status != "succeeded" || p.Runs[0].ScheduledFor == "" {
		t.Fatalf("duplicate occurrence %+v %v", p, e)
	}
}
