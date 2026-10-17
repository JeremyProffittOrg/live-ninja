package main

import (
	"context"
	"encoding/json"
	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/aws/aws-lambda-go/events"
	"testing"
	"time"
)

type fakeTicker struct{ calls int }

func (f *fakeTicker) Tick(context.Context, time.Time, int) (jobs.TickResult, error) {
	f.calls++
	return jobs.TickResult{Processed: 1}, nil
}
func TestWorkerGate(t *testing.T) {
	f := &fakeTicker{}
	w := worker{service: f}
	r, e := w.handle(context.Background(), events.CloudWatchEvent{})
	if e != nil || r.Processed != 0 || f.calls != 0 {
		t.Fatal("disabled worker ran")
	}
	w.enabled = true
	r, e = w.handle(context.Background(), events.CloudWatchEvent{})
	if e != nil || r.Processed != 1 || f.calls != 1 {
		t.Fatal("enabled worker not processed")
	}
}

type fakePhoneSync struct {
	calls int
	uid   string
	err   error
}

func (f *fakePhoneSync) SyncPhoneRun(_ context.Context, uid, _, _ string) (*jobs.Run, error) {
	f.calls++
	f.uid = uid
	return &jobs.Run{}, f.err
}
func TestPhoneNotificationDoesNotEnableGenericScheduling(t *testing.T) {
	f := &fakeTicker{}
	phone := &fakePhoneSync{}
	w := worker{service: f, phone: phone}
	r, e := w.handleEvent(context.Background(), json.RawMessage(`{"kind":"phone-research","userId":"owner","jobId":"job","runId":"run"}`))
	if e != nil || r.Processed != 1 || phone.calls != 1 || f.calls != 0 {
		t.Fatal("phone notification did not stay isolated", r, e)
	}
	r, e = w.handleEvent(context.Background(), json.RawMessage(`{"source":"aws.events"}`))
	if e != nil || r.Processed != 0 || f.calls != 0 {
		t.Fatal("notification enabled scheduling", r, e)
	}
	phone.err = jobs.ErrForbidden
	r, e = w.handleEvent(context.Background(), json.RawMessage(`{"kind":"phone-research","userId":"forged","jobId":"job","runId":"run"}`))
	if e != nil || r.Skipped != 1 {
		t.Fatal("event identity bypassed service authorization", r, e)
	}
	if _, e = w.handleEvent(context.Background(), json.RawMessage(`{"kind":"phone-research","userId":"owner"}`)); e == nil {
		t.Fatal("incomplete event accepted")
	}
}
