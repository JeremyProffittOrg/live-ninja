package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
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

// newTestDepsWithFake is newTestDeps plus a handle on the FakeDynamo behind
// the Store, with DDB pointing at that same fake so NewRegistry can default
// Deps.IdempotencyRelease from it — production wires one *dynamodb.Client into
// both fields. newTestDeps deliberately leaves DDB nil (that is the "no
// releaser wired" degradation path), so the two constructors stay separate.
func newTestDepsWithFake() (*Deps, *testutil.FakeDynamo) {
	fake := testutil.NewFakeDynamo()
	d := newTestDeps()
	d.Store = store.NewWithClient(fake, "live-ninja-test")
	d.DDB = fake
	return d, fake
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

// registerFlakyTool adds a side-effecting tool whose handler counts its
// executions and fails with whatever code *code points at ("" = succeed), so a
// test can flip the outcome between two deliveries of the same call.
func registerFlakyTool(t *testing.T, r *Registry, calls *atomic.Int64, code *string) {
	t.Helper()
	require.NoError(t, r.register(&Definition{
		Name:          "flaky_tool",
		Description:   "test-only side-effecting tool with a switchable failure",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "value", Type: "string", Required: true},
		},
		Handler: func(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
			calls.Add(1)
			if *code != "" {
				return nil, toolErrf(*code, "handler failed before doing anything")
			}
			return map[string]any{"echo": args["value"]}, nil
		},
	}))
}

// TestIdempotencyClaimReleasedOnPreSideEffectFailure is the WS-3 3.2
// regression test. The IDEMP# marker is claimed before the handler runs (which
// is correct — it is what makes a duplicate delivery a no-op), but nothing
// released it again when the handler failed without reaching its external
// dependency. The next delivery of the same call therefore hit the marker and
// was answered ok:true / duplicate:true "this call was already processed":
// the side effect had never happened, yet the model — and the human it is
// talking to — were told it had. This test fails on that behaviour (the second
// Invoke reports a duplicate and the handler count stays at 1) and passes once
// the claim is released.
func TestIdempotencyClaimReleasedOnPreSideEffectFailure(t *testing.T) {
	deps, fake := newTestDepsWithFake()
	r := newTestRegistry(t, deps)

	var calls atomic.Int64
	code := CodeNotConfigured
	registerFlakyTool(t, r, &calls, &code)

	inv := invocation("flaky_tool", map[string]any{"value": "x"})
	inv.IdempotencyKey = "idem-release"

	// First delivery: the claim is taken, the handler runs and fails before
	// any side effect.
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	require.NotNil(t, res.Error)
	assert.Equal(t, CodeNotConfigured, res.Error.Code)
	assert.Equal(t, int64(1), calls.Load())
	assert.Nil(t, fake.RawItem("IDEMP#user-1#idem-release", "IDEMP"),
		"a handler failure with no side effect must not leave the claim behind")

	// Re-delivery of the SAME call now executes for real instead of being
	// swallowed as an already-processed duplicate.
	code = ""
	res = r.Invoke(context.Background(), inv)
	require.True(t, res.OK)
	assert.False(t, res.Duplicate, "the retry must execute, not report a phantom duplicate")
	assert.Equal(t, "x", res.Output["echo"])
	assert.Equal(t, int64(2), calls.Load())

	// The successful claim IS retained, so a third delivery is a duplicate.
	assert.NotNil(t, fake.RawItem("IDEMP#user-1#idem-release", "IDEMP"))
	res = r.Invoke(context.Background(), inv)
	require.True(t, res.OK)
	assert.True(t, res.Duplicate)
	assert.Equal(t, int64(2), calls.Load())
}

// TestIdempotencyClaimHeldOnUpstreamError pins the other half of the policy:
// upstream_error means the external system was called and its outcome is
// unknown, so the claim is kept and an identical re-delivery is refused rather
// than risking a second email / a second MQTT publish. At-most-once wins over
// retryability exactly where — and only where — the ambiguity is real.
func TestIdempotencyClaimHeldOnUpstreamError(t *testing.T) {
	deps, fake := newTestDepsWithFake()
	r := newTestRegistry(t, deps)

	var calls atomic.Int64
	code := CodeUpstreamError
	registerFlakyTool(t, r, &calls, &code)

	inv := invocation("flaky_tool", map[string]any{"value": "x"})
	inv.IdempotencyKey = "idem-hold"

	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeUpstreamError, res.Error.Code)
	assert.NotNil(t, fake.RawItem("IDEMP#user-1#idem-hold", "IDEMP"),
		"an ambiguous upstream failure must keep the claim")

	// Even though the handler would now succeed, the identical re-delivery is
	// refused: we cannot know the first attempt did not already act.
	code = ""
	res = r.Invoke(context.Background(), inv)
	require.True(t, res.OK)
	assert.True(t, res.Duplicate)
	assert.Equal(t, int64(1), calls.Load())
}

func TestIdempotencyReleasableCodes(t *testing.T) {
	for _, code := range []string{CodeInvalidArgs, CodeForbidden, CodeNotFound,
		CodeAlreadyExists, CodeNotConfigured, CodeConfirmationRequired} {
		assert.True(t, idempotencyReleasable(code), code)
	}
	// upstream_error is ambiguous; unknown_tool never comes from a handler;
	// an unrecognized future code must default to holding the claim.
	for _, code := range []string{CodeUpstreamError, CodeUnknownTool, "some_new_code", ""} {
		assert.False(t, idempotencyReleasable(code), code)
	}
}

// TestIdempotencyReleaseWithoutDeleteCapableClientIsInert guards the
// degradation path: a registry whose DDB client cannot DeleteItem keeps the
// pre-fix behaviour (the marker lives out its TTL) instead of panicking.
func TestIdempotencyReleaseWithoutDeleteCapableClientIsInert(t *testing.T) {
	deps := newTestDeps() // DDB nil -> no releaser defaulted
	r := newTestRegistry(t, deps)
	require.Nil(t, deps.IdempotencyRelease)

	var calls atomic.Int64
	code := CodeNotConfigured
	registerFlakyTool(t, r, &calls, &code)

	inv := invocation("flaky_tool", map[string]any{"value": "x"})
	inv.IdempotencyKey = "idem-no-releaser"

	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeNotConfigured, res.Error.Code)

	res = r.Invoke(context.Background(), inv)
	require.True(t, res.OK)
	assert.True(t, res.Duplicate)
	assert.Equal(t, int64(1), calls.Load())
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

// captureStdout runs fn with os.Stdout redirected to a temp file and returns
// what was written. observ.EmitMetric writes EMF straight to os.Stdout (that IS
// the CloudWatch delivery mechanism under Lambda), so this is the only way to
// assert on a metric's dimensions. Tests in this package do not run in
// parallel, so swapping the global is safe here.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "emf")
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = f
	defer func() {
		os.Stdout = orig
		_ = f.Close()
	}()
	fn()
	require.NoError(t, f.Sync())
	raw, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	return string(raw)
}

// TestUnknownToolIsNotAMetricDimension: a CloudWatch dimension VALUE is billed
// per distinct combination — one custom metric per unique value, indefinitely.
// inv.Tool is raw client input on the unknown-tool path (Invoke rejects unknown
// tools before any validation), so emitting it as a dimension let an
// authenticated caller mint unbounded custom metrics by looping over random tool
// names off a 404. The name still reaches the (cardinality-free,
// retention-bounded) log line.
func TestUnknownToolIsNotAMetricDimension(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())

	emf := captureStdout(t, func() {
		res := r.Invoke(context.Background(), invocation("frobnicate-9f3a21", nil))
		require.False(t, res.OK)
		require.Equal(t, CodeUnknownTool, res.Error.Code)
	})

	assert.NotContains(t, emf, "frobnicate-9f3a21",
		"a client-supplied tool name must never become a CloudWatch dimension value")
	assert.Contains(t, emf, `"Tool":"`+unknownToolSentinel+`"`)

	// A real tool still reports under its own name — the sentinel must not
	// flatten the useful per-tool breakdown.
	emf = captureStdout(t, func() {
		_ = r.Invoke(context.Background(), invocation("get_weather", map[string]any{"location": "Austin"}))
	})
	assert.Contains(t, emf, `"Tool":"get_weather"`)
}

// TestHandlerPanicPreservesTheOriginalPanic: finish runs in a defer, so it also
// runs while a panicking handler unwinds — at which point res is neither OK nor
// carrying an Error. Logging res.Error.Code unconditionally there raised a
// nil-pointer dereference INSIDE the defer, which replaces the original panic
// value and throws away the stack naming the handler that actually broke.
func TestHandlerPanicPreservesTheOriginalPanic(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	require.NoError(t, r.register(&Definition{
		Name:        "panic_tool",
		Description: "test-only tool that panics",
		Handler: func(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
			panic("handler exploded")
		},
	}))

	recovered := func() (v any) {
		defer func() { v = recover() }()
		_ = r.Invoke(context.Background(), invocation("panic_tool", nil))
		return nil
	}()

	require.NotNil(t, recovered, "the panic must propagate, not be swallowed")
	assert.Equal(t, "handler exploded", recovered,
		"finish must not replace the handler's panic with its own nil dereference")
}
