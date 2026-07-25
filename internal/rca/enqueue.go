package rca

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

// SQSSendAPI is the SendMessage subset of the SQS client. It is declared with
// the identical method set as tools.SQSAPI / webapp.SQSSendAPI on purpose, so
// the client the web function already holds satisfies all three without an
// adapter.
type SQSSendAPI interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// enqueueSendTimeout bounds the one blocking call M17 adds to the request path.
// The send is deliberately SYNCHRONOUS rather than fired into a goroutine:
//
//   - the web function runs behind the Lambda Web Adapter, and a goroutine that
//     outlives the HTTP response is frozen with the execution environment — it
//     may not run again until the next invocation, or ever;
//   - the caller's ctx is Fiber's *fasthttp* request context, which is recycled
//     the moment the handler returns, so using it off-goroutine is a
//     use-after-free;
//   - this path only executes on outcome=error, which is rare, and 2s is a hard
//     ceiling on the added latency.
//
// "Non-blocking" in the plan's sense — never alters the Result, never returns an
// error onto the request path — is satisfied by the caller (finish logs and
// swallows). If per-request latency ever becomes a concern the correct fix is a
// buffered channel drained by one goroutine started at cold start; that is not
// warranted for a <=10/day flow.
const enqueueSendTimeout = 2 * time.Second

// SQSEnqueuer is the tools.FailureEnqueuer backed by the live-ninja-rca queue.
type SQSEnqueuer struct {
	sqs      SQSSendAPI
	queueURL string
	timeout  time.Duration
}

// NewSQSEnqueuer returns nil when client or queueURL is empty — the caller then
// leaves tools.Deps.RCA nil and the enqueue hook stays completely inert. The nil
// return is the whole configuration story: no RCA_QUEUE_URL, no RCA.
func NewSQSEnqueuer(client SQSSendAPI, queueURL string) *SQSEnqueuer {
	if client == nil || queueURL == "" {
		return nil
	}
	return &SQSEnqueuer{sqs: client, queueURL: queueURL, timeout: enqueueSendTimeout}
}

// EnqueueToolFailure marshals f and sends it to the RCA queue.
//
// The passed ctx is deliberately NOT used for the send: it derives its own
// bounded context from context.Background() for the reasons on
// enqueueSendTimeout (the caller's ctx is a recycled fasthttp request context).
// The parameter stays in the signature to satisfy tools.FailureEnqueuer and so a
// future in-process implementation can honour it.
func (e *SQSEnqueuer) EnqueueToolFailure(ctx context.Context, f tools.ToolFailure) error {
	body, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("rca: marshal tool failure: %w", err)
	}

	sendCtx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()

	if _, err := e.sqs.SendMessage(sendCtx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(e.queueURL),
		MessageBody: aws.String(string(body)),
	}); err != nil {
		return fmt.Errorf("rca: enqueue tool failure: %w", err)
	}
	return nil
}
