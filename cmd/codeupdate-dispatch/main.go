// Command codeupdate-dispatch turns a voice request into a running coding
// session on one of the owner's machines.
//
// It drains the live-ninja-code-update SQS queue that the `code_update_start`
// tool feeds, and for each request it
//
//  1. mints a per-run token and writes its row, so the coding agent has a way to
//     email progress back (the node has no mail path of its own);
//  2. optionally asks ghost-cli for an Opus rewrite of the owner's instructions
//     and polls it to a conclusion;
//  3. assembles the final prompt — rewrite (or original) + deploy gate +
//     progress-reporting block + the output directive that arms ghost-cli's
//     capture-and-summarize path;
//  4. creates a run_now schedule event on ghost-cli, which dispatches the LAUNCH
//     to the node;
//  5. emails the owner exactly what was sent, or why nothing was.
//
// # Why this is a worker and not part of the tool call
//
// The Opus rewrite regularly runs 30–90 s and the web function's timeout is
// 30 s, so a realtime tool handler physically cannot wait for it. More
// importantly it should not have to: once the owner has said what they want,
// ending the conversation must not cancel the work.
//
// # Retry semantics
//
// The event source uses FunctionResponseTypes: ReportBatchItemFailures, and this
// handler always returns nil with a per-record failure list. Only TRANSIENT
// failures are reported back — a DynamoDB write error, a 5xx from ghost-cli.
// A permanent one (an unreadable body, a denied principal, a repo ghost-cli
// rejects) is emailed to the owner and then deleted, because redelivering it
// produces the same rejection and a second email.
//
// A duplicate delivery re-mints the token and re-launches. ghost-cli's own
// in-flight guard is what makes that safe: the second launch against the same
// stable event id is refused with a 409 while the first is still running.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"
	awslambda "github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awslambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/JeremyProffittOrg/live-ninja/internal/codeupdate"
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
)

// perRecordBudget bounds one request so a full batch cannot exceed the function
// timeout: BatchSize 1 x 280s + overhead < 300s (template.yaml). The preprocess
// poll has its own, shorter ceiling inside this.
const perRecordBudget = 280 * time.Second

func main() {
	app := config.FromEnv()
	logger := observ.NewLogger(os.Stdout, app.LogLevel).With(slog.String("function", "codeupdate-dispatch"))

	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		logger.Error("codeupdate-dispatch: load aws config", slog.String("error", err.Error()))
		os.Exit(1)
	}

	ddb := dynamodb.NewFromConfig(awsCfg)
	dispatcher := &codeupdate.Dispatcher{
		Ghost: ghost.New(ghost.Config{
			API:      awslambdasvc.NewFromConfig(awsCfg),
			Function: os.Getenv("GHOST_COMMAND_FUNCTION_ARN"),
			Log:      logger,
		}),
		Store:             codeupdate.NewStore(ddb, app.TableName, nil),
		SQS:               sqs.NewFromConfig(awsCfg),
		EmailQueueURL:     app.EmailQueueURL,
		OwnerEmail:        os.Getenv("OWNER_EMAIL"),
		ProgressURL:       progressURL(app.DomainName),
		OutputFile:        getenv("CODE_UPDATE_OUTPUT_FILE", codeupdate.DefaultOutputFile),
		PreprocessTimeout: preprocessTimeout(logger),
		Log:               logger,
	}

	// Say so loudly rather than degrade quietly. Without a progress URL the
	// prompt simply omits the reporting block, so the owner never receives a
	// progress email and nothing anywhere reads as broken — exactly the failure
	// mode that is hardest to notice.
	if dispatcher.ProgressURL == "" {
		logger.Warn("codeupdate-dispatch: progress emails disabled (DOMAIN_NAME not set)")
	}
	if dispatcher.OwnerEmail == "" {
		logger.Warn("codeupdate-dispatch: OWNER_EMAIL not set; every dispatch will fail")
	}

	awslambda.Start(func(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
		var resp events.SQSEventResponse
		for _, record := range event.Records {
			var req codeupdate.Request
			if err := json.Unmarshal([]byte(record.Body), &req); err != nil {
				// Permanent: an unparseable body will never parse. Drop it rather
				// than looping it to the DLQ three times first.
				logger.Error("codeupdate-dispatch: unparseable message",
					slog.String("message_id", record.MessageId))
				continue
			}
			recordCtx, cancel := context.WithTimeout(ctx, perRecordBudget)
			err := dispatcher.Dispatch(recordCtx, req)
			cancel()
			if err != nil {
				logger.Error("codeupdate-dispatch: transient failure, returning to queue",
					slog.String("request_id", req.RequestID), slog.String("error", err.Error()))
				resp.BatchItemFailures = append(resp.BatchItemFailures,
					events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
			}
		}
		return resp, nil
	})
}

// progressURL is the absolute endpoint embedded in every generated prompt. It is
// derived from DOMAIN_NAME rather than configured separately so it can never
// point somewhere the rest of the app does not.
func progressURL(domain string) string {
	if domain == "" {
		return ""
	}
	return "https://" + domain + "/v1/code-update/progress"
}

func preprocessTimeout(logger *slog.Logger) time.Duration {
	raw := os.Getenv("PREPROCESS_POLL_TIMEOUT_SECONDS")
	if raw == "" {
		return codeupdate.DefaultPreprocessTimeout
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		logger.Warn("codeupdate-dispatch: ignoring invalid PREPROCESS_POLL_TIMEOUT_SECONDS",
			slog.String("value", raw))
		return codeupdate.DefaultPreprocessTimeout
	}
	return time.Duration(secs) * time.Second
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
