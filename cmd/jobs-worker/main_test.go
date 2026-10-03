package main

import (
	"context"
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
