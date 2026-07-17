// Command email-dispatch consumes the EmailQueue SQS queue (see
// template.yaml: EmailDispatchFunction's event source).
//
// M0 real behavior (per plan.md): read a message shaped
// {template,to,subject,text}, send it via SES from
// "Jeremy Proffitt <jeremy@jeremy.ninja>" with Reply-To
// proffitt.jeremy@gmail.com, and guarantee at-most-once delivery per SQS
// MessageId via a conditional PutItem at IDEMP#<messageId> (TTL-bounded).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// fromAddress/replyTo are fixed by house policy: SES sends MUST originate
// from a DKIM-verified @jeremy.ninja identity, with Reply-To pointed at
// the human inbox. Never send *from* proffitt.jeremy@gmail.com — that
// identity has DKIM disabled, so SES accepts the send and returns a
// MessageId, but Gmail silently drops it on DMARC failure.
const (
	fromAddress = "Jeremy Proffitt <jeremy@jeremy.ninja>"
	replyTo     = "proffitt.jeremy@gmail.com"

	// idempTTL bounds how long an IDEMP# marker survives: long enough to
	// absorb SQS/Lambda redelivery and retries, short enough not to bloat
	// the table indefinitely.
	idempTTL = 7 * 24 * time.Hour
)

// EmailMessage is the SQS message body shape this consumer expects.
type EmailMessage struct {
	Template string `json:"template"`
	To       string `json:"to"`
	Subject  string `json:"subject"`
	Text     string `json:"text"`
}

var (
	logger    = observ.NewLogger(os.Stdout, config.FromEnv().LogLevel)
	st        *store.Store
	sesClient *sesv2.Client
)

func handler(ctx context.Context, event events.SQSEvent) error {
	var failures []error

	for _, record := range event.Records {
		if err := processRecord(ctx, record); err != nil {
			failures = append(failures, fmt.Errorf("message %s: %w", record.MessageId, err))
		}
	}

	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

func processRecord(ctx context.Context, record events.SQSMessage) error {
	l := observ.WithRequest(logger, record.MessageId, "", "system")

	var msg EmailMessage
	if err := json.Unmarshal([]byte(record.Body), &msg); err != nil {
		l.Error("email-dispatch: invalid message body", slog.String("error", err.Error()))
		return fmt.Errorf("unmarshal body: %w", err)
	}
	if msg.To == "" || msg.Subject == "" || msg.Text == "" {
		err := errors.New("message missing required fields (to/subject/text)")
		l.Error("email-dispatch: rejected message", slog.String("error", err.Error()), slog.String("template", msg.Template))
		return err
	}

	// Idempotency: exactly one delivery proceeds per SQS MessageId, even
	// across Lambda retries or SQS redelivery of the same message.
	err := st.ConditionalPut(ctx, "IDEMP#"+record.MessageId, "IDEMP", map[string]any{
		"to":       msg.To,
		"template": msg.Template,
	}, time.Now().Add(idempTTL).Unix())
	if errors.Is(err, store.ErrAlreadyExists) {
		l.Info("email-dispatch: duplicate delivery skipped (already processed)")
		return nil
	}
	if err != nil {
		l.Error("email-dispatch: idempotency put failed", slog.String("error", err.Error()))
		return fmt.Errorf("idempotency put: %w", err)
	}

	if err := sendEmail(ctx, msg); err != nil {
		l.Error("email-dispatch: send failed", slog.String("error", err.Error()), slog.String("template", msg.Template))
		return fmt.Errorf("send email: %w", err)
	}

	l.Info("email-dispatch: sent", slog.String("template", msg.Template))
	observ.EmitMetric("LiveNinja/EmailDispatch", "EmailsSent", 1, "Count",
		map[string]string{"Template": msg.Template})
	return nil
}

func sendEmail(ctx context.Context, msg EmailMessage) error {
	_, err := sesClient.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(fromAddress),
		ReplyToAddresses: []string{replyTo},
		Destination: &types.Destination{
			ToAddresses: []string{msg.To},
		},
		Content: &types.EmailContent{
			Simple: &types.Message{
				Subject: &types.Content{Data: aws.String(msg.Subject)},
				Body: &types.Body{
					Text: &types.Content{Data: aws.String(msg.Text)},
				},
			},
		},
	})
	return err
}

func main() {
	ctx := context.Background()
	cfg := config.FromEnv()

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		logger.Error("email-dispatch: aws config load failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	st = store.NewWithClient(dynamodb.NewFromConfig(awsCfg), cfg.TableName)
	sesClient = sesv2.NewFromConfig(awsCfg)

	lambda.Start(handler)
}
