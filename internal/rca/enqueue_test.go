package rca

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

type fakeSQS struct {
	sent []*sqs.SendMessageInput
	err  error
	// gotDeadline records whether the send context carried a deadline, which is
	// the property enqueueSendTimeout exists to guarantee.
	gotDeadline bool
}

func (f *fakeSQS) SendMessage(ctx context.Context, in *sqs.SendMessageInput,
	_ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	_, f.gotDeadline = ctx.Deadline()
	if f.err != nil {
		return nil, f.err
	}
	f.sent = append(f.sent, in)
	return &sqs.SendMessageOutput{MessageId: aws.String("sqs-1")}, nil
}

// TestNewSQSEnqueuerNilWithoutConfig pins the whole configuration story: no
// RCA_QUEUE_URL means a nil enqueuer, which leaves tools.Deps.RCA a true nil
// interface so the finish hook stays inert.
func TestNewSQSEnqueuerNilWithoutConfig(t *testing.T) {
	assert.Nil(t, NewSQSEnqueuer(&fakeSQS{}, ""))
	assert.Nil(t, NewSQSEnqueuer(nil, "https://sqs.example/queue"))
	assert.NotNil(t, NewSQSEnqueuer(&fakeSQS{}, "https://sqs.example/queue"))
}

func TestEnqueueToolFailureRoundTrips(t *testing.T) {
	fake := &fakeSQS{}
	enq := NewSQSEnqueuer(fake, "https://sqs.example/live-ninja-rca")
	require.NotNil(t, enq)

	f := baseFailure()
	require.NoError(t, enq.EnqueueToolFailure(context.Background(), f))
	require.Len(t, fake.sent, 1)
	assert.Equal(t, "https://sqs.example/live-ninja-rca", aws.ToString(fake.sent[0].QueueUrl))
	assert.True(t, fake.gotDeadline, "the send must be bounded, not open-ended")

	// The wire contract: the consumer decodes exactly what the producer encoded.
	var got tools.ToolFailure
	require.NoError(t, json.Unmarshal([]byte(aws.ToString(fake.sent[0].MessageBody)), &got))
	assert.Equal(t, f, got)

	// And the JSON field names are the ones the spec fixed.
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(aws.ToString(fake.sent[0].MessageBody)), &raw))
	for _, key := range []string{"v", "source", "tool", "errorCode", "errorMessage",
		"args", "callId", "txId", "userId", "sessionId", "surface", "role", "occurredAt"} {
		assert.Contains(t, raw, key)
	}
	assert.NotContains(t, raw, "convId", "reserved fields stay omitted from the tool-router shape")
	assert.NotContains(t, raw, "engine")
}

// TestEnqueueIgnoresCallerContext covers the fasthttp use-after-free hazard: the
// caller's request context is recycled the moment the handler returns, so the
// send must derive its own.
func TestEnqueueIgnoresCallerContext(t *testing.T) {
	fake := &fakeSQS{}
	enq := NewSQSEnqueuer(fake, "https://sqs.example/live-ninja-rca")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, enq.EnqueueToolFailure(cancelled, baseFailure()))
	assert.Len(t, fake.sent, 1)
	assert.Equal(t, enqueueSendTimeout, enq.timeout)
	assert.Equal(t, 2*time.Second, enqueueSendTimeout)
}

func TestEnqueueSurfacesSendErrors(t *testing.T) {
	fake := &fakeSQS{err: errors.New("sqs: no such queue")}
	enq := NewSQSEnqueuer(fake, "https://sqs.example/live-ninja-rca")

	err := enq.EnqueueToolFailure(context.Background(), baseFailure())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rca: enqueue tool failure")
}
