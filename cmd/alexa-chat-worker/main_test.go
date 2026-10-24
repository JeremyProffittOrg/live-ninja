package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/alexa"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/require"
)

func TestAlexaWorkerReportsFailedMessagesWithoutReplayingSuccessfulOnes(t *testing.T) {
	calls := 0
	w := worker{log: slog.New(slog.NewTextHandler(io.Discard, nil)), execute: func(ctx context.Context, job alexa.Job) error {
		calls++
		if job.Owner == "failed" {
			return errors.New("storage unavailable")
		}
		return nil
	}}
	body := `{"owner":"owner","turn":"` + strings.Repeat("a", 64) + `"}`
	result, err := w.Handle(t.Context(), events.SQSEvent{Records: []events.SQSMessage{{MessageId: "ok", Body: body}, {MessageId: "invalid", Body: "bad"}, {MessageId: "failed", Body: strings.Replace(body, `"owner":"owner"`, `"owner":"failed"`, 1)}}})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, []events.SQSBatchItemFailure{{ItemIdentifier: "invalid"}, {ItemIdentifier: "failed"}}, result.BatchItemFailures)
}
