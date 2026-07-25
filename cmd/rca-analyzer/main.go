// Command rca-analyzer is the M17 tool-failure RCA consumer: it drains the
// live-ninja-rca SQS queue that internal/tools' finish hook feeds, and for each
// failed invocation it
//
//  1. decodes the tools.ToolFailure envelope;
//  2. hands it to internal/rca, which dedupes it by failure signature, claims a
//     unit of the day's Opus budget, gathers the transcript window + tool
//     contract + prior RCAs + docs/system-map.md, asks Claude Opus on Bedrock
//     for a structured root-cause analysis, emails the owner via SES, persists
//     an RCA# item (30-day TTL) and files any base-knowledge it inferred as
//     pending PROFSUGG# suggestions;
//  3. reports ONLY transient failures back to SQS as batch item failures.
//
// # Idempotency / retry semantics
//
// The event source is configured with FunctionResponseTypes:
// ReportBatchItemFailures, and this handler always returns a nil error with a
// per-record failure list.
//
// cmd/email-dispatch joins its per-record errors and returns them, which makes
// the WHOLE batch redeliver — acceptable there because sending is guarded by an
// IDEMP#<messageId> marker. It is not acceptable here: a single poison message
// would drag its batch-mates back through the cooldown claim on every retry,
// and a genuinely transient sibling would then be re-suppressed as a duplicate.
// With ReportBatchItemFailures only the transient message returns to the queue.
//
// The complement matters as much: a PERMANENT failure is never reported as a
// batch item failure. An unparseable body, a missing userId, a denied Bedrock
// model, a rejected ValidationException and a malformed model reply all return
// nil from rca.Analyze and are therefore deleted. Nothing in this pipeline can
// retry forever; maxReceiveCount 3 on the queue is the belt-and-braces backstop
// for a transient condition that never clears.
//
// Duplicate delivery of a message that was already analysed is absorbed one
// layer down: the redelivery recomputes the same failure signature, loses the
// cooldown claim, and exits as suppressed — so it costs one conditional write
// and sends no second email.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	awslambda "github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/rca"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

// perRecordBudget bounds one message so a full batch cannot exceed the function
// timeout: BatchSize 2 x 120s + overhead < 300s (template.yaml). Keep this in
// step with RcaAnalyzerFunction.Timeout and RcaQueue's VisibilityTimeout if any
// of them changes.
const perRecordBudget = 120 * time.Second

// maxBodyLogBytes caps how much of an undecodable message body reaches the log
// line — enough to identify the producer that sent it, not enough to dump a
// pathological payload into CloudWatch.
const maxBodyLogBytes = 256

// analyzerAPI is the one operation the handler needs from internal/rca, so
// main_test.go can drive the batch-response logic with a scripted analyzer and
// no AWS anything.
type analyzerAPI interface {
	Handle(ctx context.Context, f tools.ToolFailure) error
}

type handler struct {
	log      *slog.Logger
	analyzer analyzerAPI
}

// Handle processes one SQS batch. It never returns a non-nil error: every
// per-record disposition is expressed through BatchItemFailures (see the
// package comment's retry contract).
func (h *handler) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	var resp events.SQSEventResponse
	for _, record := range event.Records {
		if h.retryable(ctx, record) {
			resp.BatchItemFailures = append(resp.BatchItemFailures,
				events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
		}
	}
	return resp, nil
}

// retryable processes one record and reports whether it must go back on the
// queue.
func (h *handler) retryable(ctx context.Context, record events.SQSMessage) bool {
	var f tools.ToolFailure
	if err := json.Unmarshal([]byte(record.Body), &f); err != nil {
		// Permanent: the same bytes will fail to decode forever. Log loudly
		// (this means a producer is writing a shape nobody consumes) and drop.
		h.log.Error("rca-analyzer: undecodable message body; dropping",
			slog.String("requestId", record.MessageId),
			slog.String("error", err.Error()),
			slog.String("bodyHead", head(record.Body, maxBodyLogBytes)))
		return false
	}

	// Stamping the failure's txId onto this logger is the single most useful
	// thing the analyzer logs: it joins every RCA line to the exact tool call
	// that produced it, so one CloudWatch Logs Insights query on txId shows the
	// invocation, its audit row and its root-cause analysis together.
	l := observ.WithTxn(
		observ.WithRequest(h.log, record.MessageId, f.UserID, surfaceOr(f.Surface, "system")),
		f.TxID)

	recCtx, cancel := context.WithTimeout(ctx, perRecordBudget)
	defer cancel()

	if err := h.analyzer.Handle(recCtx, f); err != nil {
		l.Warn("rca-analyzer: transient failure; returning message to the queue",
			slog.String("tool", f.Tool),
			slog.String("errorCode", f.ErrorCode),
			slog.String("error", err.Error()))
		return true
	}
	return false
}

// surfaceOr defaults the log's surface field for a record whose producer did not
// record one (a device or fallback-turn invocation can legitimately have none).
func surfaceOr(surface, fallback string) string {
	if surface == "" {
		return fallback
	}
	return surface
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func main() {
	ctx := context.Background()
	appCfg := config.FromEnv()
	logger := observ.NewLogger(os.Stdout, appCfg.LogLevel)

	// Every construction failure exits: there is no degraded half-analyzer, and
	// a function that starts without a model or a mailer would look healthy
	// while silently analysing nothing.
	rcaCfg, err := rca.ConfigFromEnv()
	if err != nil {
		logger.Error("rca-analyzer: config failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		logger.Error("rca-analyzer: load aws config failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	st, err := store.New(ctx, appCfg.TableName)
	if err != nil {
		logger.Error("rca-analyzer: store init failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	model, err := rca.NewBedrockInvoker(ctx)
	if err != nil {
		logger.Error("rca-analyzer: bedrock client init failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	analyzer, err := rca.NewAnalyzer(rca.Deps{
		Store: st,
		Model: model,
		Mail:  sesv2.NewFromConfig(awsCfg),
		Log:   logger,
		Cfg:   rcaCfg,
	})
	if err != nil {
		logger.Error("rca-analyzer: analyzer init failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	logger.Info("rca-analyzer: ready",
		slog.String("modelId", rcaCfg.ModelID),
		slog.Int("dailyCap", rcaCfg.DailyCap),
		slog.String("cooldown", rcaCfg.Cooldown.String()))

	h := &handler{log: logger, analyzer: analyzer}
	awslambda.Start(h.Handle)
}
