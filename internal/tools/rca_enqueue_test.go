package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rcaFixedNow pins the clock so an enqueued envelope's occurredAt and an audit
// row's LOG# sequence are both deterministic.
var rcaFixedNow = time.Date(2026, 7, 25, 14, 3, 11, 482913000, time.UTC)

// fakeEnqueuer is the tools.FailureEnqueuer test double: it records every
// envelope finish hands it and can be made to fail.
type fakeEnqueuer struct {
	mu   sync.Mutex
	got  []ToolFailure
	fail error
}

func (f *fakeEnqueuer) EnqueueToolFailure(ctx context.Context, tf ToolFailure) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, tf)
	return f.fail
}

func (f *fakeEnqueuer) calls() []ToolFailure {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ToolFailure, len(f.got))
	copy(out, f.got)
	return out
}

// newRCADeps returns deps with a fake enqueuer wired and a frozen clock.
func newRCADeps() (*Deps, *fakeEnqueuer) {
	deps, _ := newTestDepsWithFake()
	enq := &fakeEnqueuer{}
	deps.RCA = enq
	deps.Now = func() time.Time { return rcaFixedNow }
	return deps, enq
}

// registerFailingTool adds a NON-side-effecting tool that fails with a chosen
// code, so the enqueue allowlist can be exercised for codes no real handler
// happens to return.
func registerFailingTool(t *testing.T, r *Registry, code string) {
	t.Helper()
	require.NoError(t, r.register(&Definition{
		Name:        "failing_tool",
		Description: "test-only failing tool",
		Params:      []ParamSpec{{Name: "value", Type: "string", Required: true}},
		Handler: func(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
			return nil, toolErrf(code, "handler returned %s", code)
		},
	}))
}

func TestFinishEnqueuesOnErrorOutcome(t *testing.T) {
	deps, enq := newRCADeps()
	r := newTestRegistry(t, deps)

	inv := invocation("device_control", map[string]any{
		"deviceId": "dev-1",
		"action":   "self_destruct",
	})
	inv.TxID = "tx-rca-1"
	inv.Role = "owner"

	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	require.Equal(t, CodeInvalidArgs, res.Error.Code)

	got := enq.calls()
	require.Len(t, got, 1)
	f := got[0]
	assert.Equal(t, 1, f.V)
	assert.Equal(t, "tool-router", f.Source)
	assert.Equal(t, "device_control", f.Tool)
	assert.Empty(t, f.RequestedTool)
	assert.Equal(t, CodeInvalidArgs, f.ErrorCode)
	assert.Contains(t, f.ErrorMessage, "action")
	assert.Equal(t, "tx-rca-1", f.TxID)
	assert.Equal(t, res.TxID, f.TxID)
	assert.Equal(t, "user-1", f.UserID)
	assert.Equal(t, "sess-1", f.SessionID)
	assert.Equal(t, "web", f.Surface)
	assert.Equal(t, "owner", f.Role)
	assert.Equal(t, "call-1", f.CallID)

	// args round-trip as a JSON object string, not a map.
	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.ArgsJSON), &args))
	assert.Equal(t, "self_destruct", args["action"])

	ts, err := time.Parse(time.RFC3339Nano, f.OccurredAt)
	require.NoError(t, err)
	assert.Equal(t, rcaFixedNow, ts.UTC())

	// Reserved fields are never populated by the tool router.
	assert.Empty(t, f.ConvID)
	assert.Empty(t, f.Engine)
}

func TestFinishDoesNotEnqueueOnSuccess(t *testing.T) {
	deps, enq := newRCADeps()
	r := newTestRegistry(t, deps)
	var calls atomic.Int64
	registerCounterTool(t, r, &calls)

	inv := invocation("counter_tool", map[string]any{"value": "hello"})
	inv.IdempotencyKey = "idem-ok"
	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK)
	assert.Empty(t, enq.calls())
}

func TestFinishDoesNotEnqueueDuplicate(t *testing.T) {
	deps, enq := newRCADeps()
	r := newTestRegistry(t, deps)
	var calls atomic.Int64
	registerCounterTool(t, r, &calls)

	inv := invocation("counter_tool", map[string]any{"value": "hello"})
	inv.IdempotencyKey = "idem-dup"
	require.True(t, r.Invoke(context.Background(), inv).OK)
	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK)
	require.True(t, res.Duplicate)

	assert.Empty(t, enq.calls(), "an idempotent replay is the system working, not a failure")
}

func TestFinishEnqueueAllowlist(t *testing.T) {
	cases := []struct {
		code string
		want bool
	}{
		{CodeInvalidArgs, true},
		{CodeNotFound, true},
		{CodeUpstreamError, true},
		{CodeNotConfigured, true},
		{CodeAlreadyExists, true},
		{CodeUnknownTool, true},
		{CodeForbidden, false},
		{CodeConfirmationRequired, false},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			deps, enq := newRCADeps()
			r := newTestRegistry(t, deps)
			registerFailingTool(t, r, tc.code)

			res := r.Invoke(context.Background(), invocation("failing_tool", map[string]any{"value": "x"}))
			require.False(t, res.OK)
			require.Equal(t, tc.code, res.Error.Code)

			if tc.want {
				require.Len(t, enq.calls(), 1)
				assert.Equal(t, tc.code, enq.calls()[0].ErrorCode)
				return
			}
			assert.Empty(t, enq.calls())
		})
	}
}

func TestFinishSentinelisesUnknownTool(t *testing.T) {
	deps, enq := newRCADeps()
	r := newTestRegistry(t, deps)

	raw := "evil#tool\ninjected"
	res := r.Invoke(context.Background(), invocation(raw, nil))
	require.False(t, res.OK)
	require.Equal(t, CodeUnknownTool, res.Error.Code)

	got := enq.calls()
	require.Len(t, got, 1)
	assert.Equal(t, "unknown_tool", got[0].Tool,
		"a client-supplied tool name must never key the RCA partition")
	assert.Equal(t, raw, got[0].RequestedTool, "the raw name is preserved as evidence")
	assert.Equal(t, "{}", got[0].ArgsJSON)
}

func TestFinishSkipsEnqueueWithoutUser(t *testing.T) {
	deps, enq := newRCADeps()
	r := newTestRegistry(t, deps)

	inv := invocation("get_weather", map[string]any{"location": "Austin"})
	inv.UserID = ""
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	require.Equal(t, CodeForbidden, res.Error.Code)
	assert.Empty(t, enq.calls())
}

func TestFinishEnqueueErrorNeverAffectsResult(t *testing.T) {
	inv := invocation("device_control", map[string]any{"deviceId": "dev-1", "action": "self_destruct"})
	inv.TxID = "tx-stable"

	// Baseline: no enqueuer at all.
	baseDeps, _ := newTestDepsWithFake()
	baseDeps.Now = func() time.Time { return rcaFixedNow }
	baseline := newTestRegistry(t, baseDeps).Invoke(context.Background(), inv)

	// Same call with an enqueuer that fails.
	deps, enq := newRCADeps()
	enq.fail = errors.New("sqs unavailable")
	res := newTestRegistry(t, deps).Invoke(context.Background(), inv)

	require.Len(t, enq.calls(), 1)
	assert.Equal(t, baseline, res, "an RCA enqueue failure must not change the invocation result")
}

func TestNilEnqueuerIsInert(t *testing.T) {
	inv := invocation("device_control", map[string]any{"deviceId": "dev-1", "action": "self_destruct"})
	inv.TxID = "tx-inert"

	// The audit LOG# row written must be byte-identical with and without the
	// RCA hook — the "pre-M17 behaviour is unchanged" claim, asserted rather
	// than assumed. The sk is derived from Deps.Now, hence the frozen clock.
	sk := fmt.Sprintf("LOG#sess-1#%06d", rcaFixedNow.UnixMilli()%1_000_000)
	run := func(withEnqueuer bool) map[string]types.AttributeValue {
		deps, fake := newTestDepsWithFake()
		deps.Now = func() time.Time { return rcaFixedNow }
		if withEnqueuer {
			deps.RCA = &fakeEnqueuer{}
		}
		r := newTestRegistry(t, deps)
		res := r.Invoke(context.Background(), inv)
		require.False(t, res.OK)

		item := fake.RawItem("USER#user-1", sk)
		require.NotNil(t, item, "expected an audit row at %s", sk)
		return item
	}

	assert.Equal(t, run(false), run(true))
}

func TestFinishCapsEnqueuedArgs(t *testing.T) {
	deps, enq := newRCADeps()
	r := newTestRegistry(t, deps)
	registerFailingTool(t, r, CodeUpstreamError)

	res := r.Invoke(context.Background(), invocation("failing_tool",
		map[string]any{"value": strings.Repeat("x", 10_000)}))
	require.False(t, res.OK)

	got := enq.calls()
	require.Len(t, got, 1)
	assert.LessOrEqual(t, len(got[0].ArgsJSON), maxFailureArgsJSON)
}
