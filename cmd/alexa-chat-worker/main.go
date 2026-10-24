package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/alexa"
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/webapp"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

type worker struct {
	log     *slog.Logger
	execute func(context.Context, alexa.Job) error
}

func (w *worker) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	response := events.SQSEventResponse{}
	alexa.LogDiagnostic(ctx, w.log, "queue_batch_received", nil, slog.Int("batchSize", len(event.Records)))
	for _, record := range event.Records {
		started := time.Now()
		receiveCount, _ := strconv.Atoi(record.Attributes["ApproximateReceiveCount"])
		alexa.LogDiagnostic(ctx, w.log, "queue_message_received", nil, slog.String("messageRef", alexa.DiagnosticRef(record.MessageId)), slog.Int("receiveCount", receiveCount), slog.Int("bodyBytes", len(record.Body)))
		var job alexa.Job
		decoder := json.NewDecoder(strings.NewReader(record.Body))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&job)
		alexa.LogDiagnostic(ctx, w.log, "queue_decoded", err, slog.String("messageRef", alexa.DiagnosticRef(record.MessageId)))
		if err == nil && w.execute == nil {
			err = errors.New("Alexa worker unavailable")
		}
		if err == nil {
			err = w.execute(ctx, job)
		}
		outcome := "complete"
		if err != nil {
			outcome = "failed"
			response.BatchItemFailures = append(response.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
		}
		alexa.LogDiagnostic(ctx, w.log, "queue_message_completed", err, slog.String("messageRef", alexa.DiagnosticRef(record.MessageId)), slog.String("receiptRef", alexa.DiagnosticRef(job.Turn)), slog.String("outcome", outcome), slog.Int64("elapsedMs", time.Since(started).Milliseconds()))
	}
	alexa.LogDiagnostic(ctx, w.log, "queue_batch_completed", nil, slog.Int("batchSize", len(event.Records)), slog.Int("failureCount", len(response.BatchItemFailures)))
	return response, nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	w := &worker{log: log}
	if os.Getenv("ALEXA_ENABLED") == "true" {
		ctx := context.Background()
		runtime, err := alexa.NewRuntime(ctx)
		if err == nil {
			var deps *webapp.Deps
			deps, err = webapp.NewServerDependencies(ctx, config.FromEnv(), log)
			if err == nil {
				runtime.Service.Run, err = webapp.NewAlexaRunner(deps)
			}
		}
		if err == nil {
			w.execute = runtime.Service.Execute
		} else {
			log.Error("Alexa worker initialization failed")
			alexa.LogDiagnostic(ctx, log, "worker_initialization", err)
		}
	}
	alexa.LogDiagnostic(context.Background(), log, "worker_ready", nil, slog.Bool("enabled", os.Getenv("ALEXA_ENABLED") == "true"), slog.Bool("executorReady", w.execute != nil))
	lambda.Start(w.Handle)
}
