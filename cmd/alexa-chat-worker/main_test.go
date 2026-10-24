package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/alexa"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/require"
)

func TestAlexaWorkerReportsFailedMessagesWithoutReplayingSuccessfulOnes(t *testing.T) {
	calls := 0
	var logs bytes.Buffer
	w := worker{log: slog.New(slog.NewJSONHandler(&logs, nil)), execute: func(ctx context.Context, job alexa.Job) error {
		calls++
		if job.Owner == "failed" {
			return errors.New("storage unavailable PRIVATE-PROVIDER-TOKEN")
		}
		return nil
	}}
	body := `{"owner":"PRIVATE-OWNER","turn":"` + strings.Repeat("a", 64) + `"}`
	result, err := w.Handle(t.Context(), events.SQSEvent{Records: []events.SQSMessage{{MessageId: "ok", Body: body}, {MessageId: "invalid", Body: "PRIVATE-INVALID-BODY"}, {MessageId: "failed", Body: strings.Replace(body, `"owner":"PRIVATE-OWNER"`, `"owner":"failed"`, 1)}}})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, []events.SQSBatchItemFailure{{ItemIdentifier: "invalid"}, {ItemIdentifier: "failed"}}, result.BatchItemFailures)
	for _, stage := range []string{"queue_batch_received", "queue_message_received", "queue_message_completed", "queue_batch_completed"} {
		require.Contains(t, logs.String(), stage)
	}
	require.Contains(t, logs.String(), `"failureCount":2`)
	require.NotContains(t, logs.String(), "PRIVATE-")
}
