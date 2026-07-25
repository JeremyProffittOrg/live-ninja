package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

// scriptedAnalyzer returns a per-callId error so one batch can mix a transient
// record with a permanent one.
type scriptedAnalyzer struct {
	errByCallID map[string]error
	seen        []tools.ToolFailure
}

func (s *scriptedAnalyzer) Handle(_ context.Context, f tools.ToolFailure) error {
	s.seen = append(s.seen, f)
	return s.errByCallID[f.CallID]
}

func newTestHandler(a analyzerAPI) *handler {
	return &handler{log: observ.NewLogger(io.Discard, "error"), analyzer: a}
}

func record(t *testing.T, id string, f tools.ToolFailure) events.SQSMessage {
	t.Helper()
	body, err := json.Marshal(f)
	require.NoError(t, err)
	return events.SQSMessage{MessageId: id, Body: string(body)}
}

func failure(callID string) tools.ToolFailure {
	return tools.ToolFailure{
		V:            1,
		Source:       "tool-router",
		Tool:         "get_weather",
		ErrorCode:    tools.CodeInvalidArgs,
		ErrorMessage: `argument "location" must be at least 2 characters`,
		ArgsJSON:     `{"location":"x"}`,
		CallID:       callID,
		TxID:         "tx-" + callID,
		UserID:       "u1",
		SessionID:    "sess-1",
		Surface:      "web",
		OccurredAt:   "2026-07-25T14:03:11.482913Z",
	}
}

// TestHandlerReportsOnlyTransientFailures is the reason this function uses
// ReportBatchItemFailures at all: a poison message must not drag its batch-mates
// back through the cooldown claim.
func TestHandlerReportsOnlyTransientFailures(t *testing.T) {
	a := &scriptedAnalyzer{errByCallID: map[string]error{
		"call_transient": errors.New("rca: bedrock converse: throttled"),
		// call_permanent returns nil — internal/rca reports every permanent
		// condition through Outcome.Status, never as an error.
	}}
	h := newTestHandler(a)

	resp, err := h.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{
		record(t, "msg-1", failure("call_transient")),
		record(t, "msg-2", failure("call_permanent")),
	}})
	require.NoError(t, err, "the handler itself never fails the whole batch")
	require.Len(t, resp.BatchItemFailures, 1)
	assert.Equal(t, "msg-1", resp.BatchItemFailures[0].ItemIdentifier)
	assert.Len(t, a.seen, 2, "both records are still processed")
}

func TestHandlerDropsUnparseableBody(t *testing.T) {
	a := &scriptedAnalyzer{}
	h := newTestHandler(a)

	resp, err := h.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "msg-bad", Body: "not json"},
	}})
	require.NoError(t, err)
	assert.Empty(t, resp.BatchItemFailures, "the same bytes would fail to decode forever")
	assert.Empty(t, a.seen)
}

func TestHandlerEmptyBatch(t *testing.T) {
	h := newTestHandler(&scriptedAnalyzer{})
	resp, err := h.Handle(context.Background(), events.SQSEvent{})
	require.NoError(t, err)
	assert.Empty(t, resp.BatchItemFailures)
}

func TestHandlerPassesEnvelopeThroughVerbatim(t *testing.T) {
	a := &scriptedAnalyzer{}
	h := newTestHandler(a)
	f := failure("call_x")
	f.RequestedTool = "evil#tool"

	_, err := h.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{
		record(t, "msg-1", f),
	}})
	require.NoError(t, err)
	require.Len(t, a.seen, 1)
	assert.Equal(t, f, a.seen[0])
}

// TestHandlerGivesEachRecordItsOwnDeadline pins the budget that keeps a full
// batch inside the function timeout.
func TestHandlerGivesEachRecordItsOwnDeadline(t *testing.T) {
	var deadlines int
	h := newTestHandler(deadlineProbe(&deadlines))

	_, err := h.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{
		record(t, "msg-1", failure("a")),
		record(t, "msg-2", failure("b")),
	}})
	require.NoError(t, err)
	assert.Equal(t, 2, deadlines)
}

type probeFunc func(ctx context.Context, f tools.ToolFailure) error

func (p probeFunc) Handle(ctx context.Context, f tools.ToolFailure) error { return p(ctx, f) }

func deadlineProbe(count *int) analyzerAPI {
	return probeFunc(func(ctx context.Context, _ tools.ToolFailure) error {
		if _, ok := ctx.Deadline(); ok {
			*count++
		}
		return nil
	})
}

func TestSurfaceOrAndHead(t *testing.T) {
	assert.Equal(t, "web", surfaceOr("web", "system"))
	assert.Equal(t, "system", surfaceOr("", "system"))

	assert.Equal(t, "abc", head("abc", 10))
	assert.Equal(t, "ab", head("abcdef", 2))
	assert.Len(t, head(strings.Repeat("x", 1000), maxBodyLogBytes), maxBodyLogBytes)
}
