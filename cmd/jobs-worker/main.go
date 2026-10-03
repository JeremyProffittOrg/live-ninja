// Command jobs-worker processes due in-app reminders and review checkpoints.
// Scheduled deployment is gated by JOBS_WORKER_ENABLED; this command never calls
// external execution providers or accepts an event-supplied user identity.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

type ticker interface {
	Tick(context.Context, time.Time, int) (jobs.TickResult, error)
}
type worker struct {
	service ticker
	enabled bool
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
	if w.enabled {
		store, err := jobs.NewDynamoStore(context.Background(), os.Getenv("TABLE_NAME"))
		if err != nil {
			slog.Error("jobs worker initialization failed", "error", err.Error())
			os.Exit(1)
		}
		w.service = jobs.NewService(store)
	}
	lambda.Start(w.handle)
}
