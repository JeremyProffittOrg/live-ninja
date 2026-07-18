package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLogsDeps returns test deps whose logger writes JSON to buf so a
// test can assert the txId (and other fields) appear on every line.
func captureLogsDeps(buf *bytes.Buffer) *Deps {
	d := newTestDeps()
	d.Log = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return d
}

// logLinesWith returns the decoded JSON log objects whose "msg" contains sub.
func logLinesWith(t *testing.T, buf *bytes.Buffer, sub string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), "log line must be valid JSON: %s", line)
		if msg, _ := m["msg"].(string); strings.Contains(msg, sub) {
			out = append(out, m)
		}
	}
	return out
}

func TestInvokeGeneratesTxIDWhenAbsent(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	res := r.Invoke(context.Background(), invocation("frobnicate", nil))

	require.False(t, res.OK)
	require.NotEmpty(t, res.TxID, "a txId must be minted when the caller supplies none")
	require.NotNil(t, res.Error)
	assert.Equal(t, res.TxID, res.Error.TxID, "the error envelope must carry the same txId")
}

func TestInvokePreservesSuppliedTxID(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	inv := invocation("frobnicate", nil)
	inv.TxID = "forwarded-txn-123"

	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, "forwarded-txn-123", res.TxID)
	require.NotNil(t, res.Error)
	assert.Equal(t, "forwarded-txn-123", res.Error.TxID)
}

func TestInvokeErrorPathsCarryTxID(t *testing.T) {
	// Every distinct error path must surface the txId on the ToolError.
	cases := []struct {
		name string
		mut  func(inv *Invocation)
		tool string
		args map[string]any
	}{
		{"unknown tool", nil, "frobnicate", nil},
		{"missing user", func(i *Invocation) { i.UserID = "" }, "get_weather", map[string]any{"location": "Austin"}},
		{"invalid args", nil, "device_control", map[string]any{"deviceId": "d", "action": "self_destruct"}},
		{"missing idempotency key", nil, "send_email", map[string]any{"subject": "s", "body": "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRegistry(t, newTestDeps())
			inv := invocation(tc.tool, tc.args)
			inv.TxID = "tx-" + tc.name
			if tc.mut != nil {
				tc.mut(&inv)
			}
			res := r.Invoke(context.Background(), inv)
			require.False(t, res.OK)
			require.NotNil(t, res.Error)
			assert.Equal(t, "tx-"+tc.name, res.Error.TxID)
			assert.Equal(t, "tx-"+tc.name, res.TxID)
		})
	}
}

func TestInvokeSuccessResultCarriesTxID(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	// get_weather with a nil HTTP client still validates+reauthorizes; use a
	// registered no-op tool to guarantee a success without external calls.
	require.NoError(t, r.register(&Definition{
		Name:        "noop_tool",
		Description: "test success path",
		Params:      []ParamSpec{{Name: "v", Type: "string", Required: true}},
		Handler: func(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
			return map[string]any{"ok": true}, nil
		},
	}))
	res := r.Invoke(context.Background(), invocation("noop_tool", map[string]any{"v": "x"}))
	require.True(t, res.OK)
	assert.NotEmpty(t, res.TxID)
	assert.Nil(t, res.Error)
}

func TestInvokeLogsIncludeTxID(t *testing.T) {
	buf := &bytes.Buffer{}
	r := newTestRegistry(t, captureLogsDeps(buf))
	inv := invocation("frobnicate", nil)
	inv.TxID = "tx-logged"
	r.Invoke(context.Background(), inv)

	// Both the ingress and egress verbose lines must carry the txId.
	starts := logLinesWith(t, buf, "invoke start")
	dones := logLinesWith(t, buf, "invoke done")
	require.NotEmpty(t, starts, "expected an 'invoke start' log line")
	require.NotEmpty(t, dones, "expected an 'invoke done' log line")
	assert.Equal(t, "tx-logged", starts[0]["txId"])
	assert.Equal(t, "tx-logged", dones[0]["txId"])
	assert.Equal(t, "error", dones[0]["outcome"])
}
