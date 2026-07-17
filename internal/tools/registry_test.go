package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func newTestDeps() *Deps {
	fake := testutil.NewFakeDynamo()
	return &Deps{
		Store:       store.NewWithClient(fake, "live-ninja-test"),
		TableName:   "live-ninja-test",
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reauthorize: func(ctx context.Context, userID string) error { return nil },
		Now:         time.Now,
	}
}

func newTestRegistry(t *testing.T, deps *Deps) *Registry {
	t.Helper()
	r, err := NewRegistry(deps)
	require.NoError(t, err)
	return r
}

func invocation(tool string, args map[string]any) Invocation {
	return Invocation{
		Tool:      tool,
		Args:      args,
		CallID:    "call-1",
		UserID:    "user-1",
		SessionID: "sess-1",
		Surface:   "web",
	}
}

func TestInvokeUnknownTool(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	res := r.Invoke(context.Background(), invocation("frobnicate", nil))
	require.False(t, res.OK)
	require.NotNil(t, res.Error)
	assert.Equal(t, CodeUnknownTool, res.Error.Code)
	assert.Equal(t, 404, res.StatusCode())
}

func TestInvokeRequiresUserContext(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	inv := invocation("get_weather", map[string]any{"location": "Austin"})
	inv.UserID = ""
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeForbidden, res.Error.Code)
}

func TestDeviceControlEnumRejectsBadAction(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())

	// An action outside the fixed enum must be rejected at validation,
	// before re-authorization or any side effect.
	res := r.Invoke(context.Background(), invocation("device_control", map[string]any{
		"deviceId": "dev-1",
		"action":   "self_destruct",
	}))
	require.False(t, res.OK)
	require.NotNil(t, res.Error)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
	assert.Contains(t, res.Error.Message, "action")
	assert.Equal(t, 400, res.StatusCode())
}

func TestValidateArgsTableDriven(t *testing.T) {
	def := &Definition{
		Name: "test_tool",
		Params: []ParamSpec{
			{Name: "mode", Type: "string", Required: true, Enum: []string{"fast", "slow"}},
			{Name: "count", Type: "integer", Min: floatPtr(1), Max: floatPtr(10)},
			{Name: "note", Type: "string", MinLen: 2, MaxLen: 5},
			{Name: "flag", Type: "boolean"},
			{Name: "tags", Type: "string_array"},
		},
	}

	cases := []struct {
		name    string
		args    map[string]any
		wantErr string // substring of the error message; "" = valid
	}{
		{"valid minimal", map[string]any{"mode": "fast"}, ""},
		{"valid full", map[string]any{"mode": "slow", "count": float64(3), "note": "abc", "flag": true, "tags": []any{"a", "b"}}, ""},
		{"enum violation", map[string]any{"mode": "warp"}, "must be one of"},
		{"enum wrong type", map[string]any{"mode": 7.0}, "must be a string"},
		{"missing required", map[string]any{}, "missing required"},
		{"unknown arg", map[string]any{"mode": "fast", "bogus": 1}, "unexpected argument"},
		{"int below min", map[string]any{"mode": "fast", "count": float64(0)}, ">= 1"},
		{"int above max", map[string]any{"mode": "fast", "count": float64(11)}, "<= 10"},
		{"int not whole", map[string]any{"mode": "fast", "count": 2.5}, "whole number"},
		{"string too short", map[string]any{"mode": "fast", "note": "a"}, "at least 2"},
		{"string too long", map[string]any{"mode": "fast", "note": "abcdef"}, "at most 5"},
		{"bool wrong type", map[string]any{"mode": "fast", "flag": "yes"}, "must be a boolean"},
		{"array with non-string", map[string]any{"mode": "fast", "tags": []any{"a", 1}}, "only strings"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, err := validateArgs(def, tc.args)
			if tc.wantErr == "" {
				require.Nil(t, err)
				require.NotNil(t, clean)
				return
			}
			require.NotNil(t, err)
			assert.Equal(t, CodeInvalidArgs, err.Code)
			assert.Contains(t, err.Message, tc.wantErr)
		})
	}
}

func TestValidateArgsCoercesTypes(t *testing.T) {
	def := &Definition{
		Name: "coerce_tool",
		Params: []ParamSpec{
			{Name: "n", Type: "integer", Required: true},
			{Name: "tags", Type: "string_array"},
		},
	}
	clean, err := validateArgs(def, map[string]any{"n": float64(7), "tags": []any{"x"}})
	require.Nil(t, err)
	assert.Equal(t, 7, clean["n"]) // JSON float64 -> int
	assert.Equal(t, []string{"x"}, clean["tags"])
}

func TestSetTimerBounds(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())

	res := r.Invoke(context.Background(), invocation("set_timer", map[string]any{
		"inSeconds": float64(999999999),
	}))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)

	res = r.Invoke(context.Background(), invocation("set_timer", map[string]any{
		"inSeconds": float64(0),
	}))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
}

// registerCounterTool adds a side-effecting tool whose executions are
// counted, for exercising the idempotency pipeline without AWS clients.
func registerCounterTool(t *testing.T, r *Registry, calls *atomic.Int64) {
	t.Helper()
	require.NoError(t, r.register(&Definition{
		Name:          "counter_tool",
		Description:   "test-only side-effecting tool",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "value", Type: "string", Required: true},
		},
		Handler: func(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
			calls.Add(1)
			return map[string]any{"echo": args["value"]}, nil
		},
	}))
}

func TestIdempotencyKeyDedup(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	var calls atomic.Int64
	registerCounterTool(t, r, &calls)

	inv := invocation("counter_tool", map[string]any{"value": "hello"})
	inv.IdempotencyKey = "idem-1"

	// First delivery executes.
	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK)
	assert.False(t, res.Duplicate)
	assert.Equal(t, "hello", res.Output["echo"])
	assert.Equal(t, int64(1), calls.Load())

	// Duplicate delivery with the same key: OK but marked duplicate, and
	// the handler must NOT run again.
	res = r.Invoke(context.Background(), inv)
	require.True(t, res.OK)
	assert.True(t, res.Duplicate)
	assert.Equal(t, int64(1), calls.Load())

	// A different key executes again.
	inv.IdempotencyKey = "idem-2"
	res = r.Invoke(context.Background(), inv)
	require.True(t, res.OK)
	assert.False(t, res.Duplicate)
	assert.Equal(t, int64(2), calls.Load())
}

func TestIdempotencyKeysScopedPerUser(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	var calls atomic.Int64
	registerCounterTool(t, r, &calls)

	inv := invocation("counter_tool", map[string]any{"value": "x"})
	inv.IdempotencyKey = "shared-key"
	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK)
	require.False(t, res.Duplicate)

	// Same key, DIFFERENT user: not a duplicate (IDEMP# is user-scoped).
	inv2 := inv
	inv2.UserID = "user-2"
	res = r.Invoke(context.Background(), inv2)
	require.True(t, res.OK)
	assert.False(t, res.Duplicate)
	assert.Equal(t, int64(2), calls.Load())
}

func TestSideEffectingToolRequiresIdempotencyKey(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	var calls atomic.Int64
	registerCounterTool(t, r, &calls)

	inv := invocation("counter_tool", map[string]any{"value": "x"})
	inv.IdempotencyKey = ""
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
	assert.Contains(t, res.Error.Message, "idempotencyKey")
	assert.Equal(t, int64(0), calls.Load())
}

func TestReauthorizationDeniesRevokedUser(t *testing.T) {
	deps := newTestDeps()
	deps.Reauthorize = func(ctx context.Context, userID string) error {
		return errors.New("user disabled since token mint")
	}
	r := newTestRegistry(t, deps)
	var calls atomic.Int64
	registerCounterTool(t, r, &calls)

	inv := invocation("counter_tool", map[string]any{"value": "x"})
	inv.IdempotencyKey = "k"
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeForbidden, res.Error.Code)
	assert.Equal(t, 403, res.StatusCode())
	assert.Equal(t, int64(0), calls.Load(), "handler must never run for a de-authorized user")
}

func TestManifestAdvertisesEnforcedSchema(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	manifest := r.Manifest()
	require.NotEmpty(t, manifest)

	byName := make(map[string]map[string]any, len(manifest))
	for _, m := range manifest {
		assert.Equal(t, "function", m["type"])
		byName[m["name"].(string)] = m
	}

	// device_control's advertised enum matches the enforced one.
	dc, ok := byName["device_control"]
	require.True(t, ok)
	params := dc["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	action := props["action"].(map[string]any)
	assert.ElementsMatch(t, deviceControlActions, action["enum"])
	assert.ElementsMatch(t, []string{"action", "deviceId"}, params["required"])

	// Full M2 catalog is present.
	for _, name := range []string{"send_email", "set_timer", "set_reminder",
		"device_control", "get_weather", "web_lookup", "remember_note", "recall_note"} {
		assert.Contains(t, byName, name)
	}
}
