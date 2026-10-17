// Command jobs-worker processes due in-app reminders and review checkpoints.
// Scheduled deployment is gated by JOBS_WORKER_ENABLED; this command never calls
// external execution providers or accepts an event-supplied user identity.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
)

type ticker interface {
	Tick(context.Context, time.Time, int) (jobs.TickResult, error)
}
type worker struct {
	service ticker
	enabled bool
	phone   interface {
		SyncPhoneRun(context.Context, string, string, string) (*jobs.Run, error)
	}
}

func (w worker) handleEvent(ctx context.Context, raw json.RawMessage) (jobs.TickResult, error) {
	var event struct {
		Kind   string `json:"kind"`
		UserID string `json:"userId"`
		JobID  string `json:"jobId"`
		RunID  string `json:"runId"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return jobs.TickResult{}, errors.New("invalid worker event")
	}
	if event.Kind == "phone-research" {
		if w.phone == nil || event.UserID == "" || event.JobID == "" || event.RunID == "" {
			return jobs.TickResult{}, errors.New("phone notification unavailable")
		}
		_, e := w.phone.SyncPhoneRun(ctx, event.UserID, event.JobID, event.RunID)
		if errors.Is(e, jobs.ErrNotFound) || errors.Is(e, jobs.ErrForbidden) {
			return jobs.TickResult{Skipped: 1}, nil
		}
		if e != nil {
			return jobs.TickResult{Failed: 1}, e
		}
		return jobs.TickResult{Processed: 1}, nil
	}
	if event.Kind != "" {
		return jobs.TickResult{}, errors.New("unsupported worker event")
	}
	var scheduled events.CloudWatchEvent
	if json.Unmarshal(raw, &scheduled) != nil || scheduled.Source != "aws.events" {
		return jobs.TickResult{}, errors.New("invalid scheduled event")
	}
	return w.handle(ctx, scheduled)
}

func (w worker) handle(ctx context.Context, _ events.CloudWatchEvent) (jobs.TickResult, error) {
	if !w.enabled {
		return jobs.TickResult{}, nil
	}
	result, err := w.service.Tick(ctx, time.Now().UTC(), 100)
	slog.Info("jobs tick", "processed", result.Processed, "conflicts", result.Conflicts, "skipped", result.Skipped, "failed", result.Failed)
	if err != nil {
		slog.Error("jobs tick failed", "error", err.Error())
	}
	return result, err
}
func main() {
	w := worker{enabled: os.Getenv("JOBS_WORKER_ENABLED") == "true"}
	phoneFunction := os.Getenv("PHONE_MESSAGES_FUNCTION_ARN")
	if w.enabled || phoneFunction != "" {
		store, err := jobs.NewDynamoStore(context.Background(), os.Getenv("TABLE_NAME"))
		if err != nil {
			slog.Error("jobs worker initialization failed", "error", err.Error())
			os.Exit(1)
		}
		svc := jobs.NewService(store)
		if phoneFunction != "" {
			cfg, e := awsconfig.LoadDefaultConfig(context.Background())
			if e != nil {
				slog.Error("phone worker config failed")
				os.Exit(1)
			}
			provider, e := jobs.NewPhoneResearchProvider(lambdasvc.NewFromConfig(cfg, func(o *lambdasvc.Options) { o.Region = "us-east-1" }), phoneFunction)
			if e != nil {
				slog.Error("phone worker provider unavailable")
				os.Exit(1)
			}
			svc = svc.WithPhoneResearch(provider)
			w.phone = svc
		}
		w.service = svc
	}
	lambda.Start(w.handleEvent)
}
