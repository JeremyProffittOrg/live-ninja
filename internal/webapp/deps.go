// Package webapp holds the Fiber route registrars and middleware for the
// Live Ninja web function (cmd/web). Route ownership per the shared spec:
// RegisterAuthRoutes (auth_routes.go) — login/callback/exchange/refresh/
// logout/device pairing/JWKS; RegisterAPIRoutes (api_routes.go) — the
// authenticated /api/v1 resource surface. Both registrars receive a *Deps
// so handlers stay pure functions over injected dependencies.
package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/deliv"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// SQSSendAPI is the subset of the SQS client the webapp needs (enqueue
// new-sign-in / security-alert emails to the EmailQueue consumed by
// cmd/email-dispatch). Interface-typed so tests inject a fake.
type SQSSendAPI interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// LambdaInvokeAPI is the subset of the Lambda client the API routes need
// (invoke the realtime broker function named by Deps.BrokerFn — the web
// function never holds the OpenAI key itself). Interface-typed so tests
// inject a fake.
type LambdaInvokeAPI interface {
	Invoke(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

// S3GetObjectAPI is the read-only subset used by the public Android
// distribution routes. Release CI writes the signed-APK metadata and Digital
// Asset Links document; the web function only reads those two exact objects.
type S3GetObjectAPI interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// Deps carries every dependency the webapp route registrars need. It is
// constructed once in cmd/web/main.go and shared by RegisterAuthRoutes and
// RegisterAPIRoutes.
//
// Note on the shared spec: the spec sketched `Cfg *config.Config`, but the
// config package's real types are App (plain env config) + Loader (SSM
// secrets) — internal/auth's NewLWAClient already consumes *config.Loader
// directly, so Deps carries the two real types instead of a phantom
// wrapper.
type Deps struct {
	Store   *store.Store
	LWA     *auth.LWAClient
	Signer  *auth.Signer
	Cfg     config.App
	Secrets *config.Loader
	Log     *slog.Logger

	// BrokerFn is the realtime broker Lambda function name
	// (BROKER_FUNCTION_NAME env), invoked via Lambda below.
	BrokerFn string
	// SQSEmailURL is the EmailQueue URL (EMAIL_QUEUE_URL env).
	SQSEmailURL string
	// SQSRcaURL is the M17 RcaQueue URL (RCA_QUEUE_URL env). Empty leaves
	// tools.Deps.RCA nil, which makes the tool router's failure hook inert —
	// tools keep working, no root-cause analyses are ever requested. The queue
	// itself is unconditional in template.yaml (only the analyzer function is
	// gated on RcaEnable), so this is normally always set in production.
	SQSRcaURL string

	SQS    SQSSendAPI
	Lambda LambdaInvokeAPI

	// AndroidArtifacts backs GET /v1/app/android/latest and
	// /.well-known/assetlinks.json. The object keys are release-CI-owned and
	// the Lambda role has read access only to those two keys.
	AndroidArtifacts       S3GetObjectAPI
	AndroidArtifactsBucket string
	AndroidLatestKey       string
	AndroidAssetLinksKey   string

	// Deliv is the M9 deliverables service (S3-backed Download Center;
	// internal/deliv). nil when DELIVERABLES_BUCKET is unset — the
	// deliverables routes then answer 503 and the deliverable_* tools
	// report not_configured.
	Deliv *deliv.Service

	// Firehose is the M7 telemetry-lake sink (Kinesis Firehose Direct
	// PUT -> live-ninja-analytics S3 bucket -> Glue/Athena, wired in
	// template.yaml). TelemetryStreamName names the delivery stream
	// (TELEMETRY_FIREHOSE_STREAM_NAME env). Either left unset degrades
	// POST /api/v1/telemetry to an explicit 503 not_configured
	// (telemetry_routes.go) — never a silent no-op.
	Firehose            FirehosePutBatchAPI
	TelemetryStreamName string
}

// emailMessage mirrors cmd/email-dispatch's EmailMessage — the SQS body
// shape that consumer expects: {template,to,subject,text}.
type emailMessage struct {
	Template string `json:"template"`
	To       string `json:"to"`
	Subject  string `json:"subject"`
	Text     string `json:"text"`
}

// EnqueueEmail enqueues one email onto the EmailQueue for cmd/
// email-dispatch to send via SES (from jeremy@jeremy.ninja, per house
// policy baked into that consumer). Callers on the request path treat a
// failure as non-fatal (log and continue) — an email alert must never
// break a login.
func (d *Deps) EnqueueEmail(ctx context.Context, template, to, subject, text string) error {
	if to == "" {
		return errors.New("webapp: email recipient is required")
	}
	if d.SQS == nil || d.SQSEmailURL == "" {
		return errors.New("webapp: email queue not configured (EMAIL_QUEUE_URL / SQS client)")
	}

	body, err := json.Marshal(emailMessage{Template: template, To: to, Subject: subject, Text: text})
	if err != nil {
		return fmt.Errorf("webapp: marshal email message: %w", err)
	}
	if _, err := d.SQS.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(d.SQSEmailURL),
		MessageBody: aws.String(string(body)),
	}); err != nil {
		return fmt.Errorf("webapp: enqueue email: %w", err)
	}
	return nil
}
