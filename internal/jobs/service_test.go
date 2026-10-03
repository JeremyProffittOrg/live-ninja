package jobs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
