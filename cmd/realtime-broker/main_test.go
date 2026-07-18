package main

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

// newTestBroker builds a broker whose only wired dependency is a JSON logger
// writing to buf. The error paths exercised here (missing userId, invalid
// surface, unknown mode) all return before any gate/minter/ddb touch, so the
// remaining nil dependencies are never dereferenced.
func newTestBroker(buf *bytes.Buffer) *broker {
	return &broker{
		log: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

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

func TestHandleErrorsCarryTxID(t *testing.T) {
	cases := []struct {
		name string
		req  Request
	}{
		{"missing userId", Request{Surface: "web"}},
		{"invalid surface", Request{UserID: "u1", Surface: "bogus"}},
		{"unknown mode", Request{UserID: "u1", Surface: "web", Mode: "teleport"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestBroker(&bytes.Buffer{})
			resp, err := b.Handle(context.Background(), tc.req)
			require.NoError(t, err)
			assert.NotEmpty(t, resp.Error, "expected an error response")
			assert.NotEmpty(t, resp.TxID, "every error response must carry a txId")
		})
	}
}

func TestHandleGeneratesTxIDWhenAbsent(t *testing.T) {
	b := newTestBroker(&bytes.Buffer{})
	resp, err := b.Handle(context.Background(), Request{Surface: "web"}) // missing userId
	require.NoError(t, err)
	require.NotEmpty(t, resp.TxID)
	assert.NotEqual(t, "", resp.Error)
}

func TestHandlePreservesSuppliedTxID(t *testing.T) {
	b := newTestBroker(&bytes.Buffer{})
	resp, err := b.Handle(context.Background(), Request{
		TxID:    "web-forwarded-txn",
		Surface: "web", // missing userId -> error, but txId must be echoed
	})
	require.NoError(t, err)
	assert.Equal(t, "web-forwarded-txn", resp.TxID)
}

func TestHandleLogsIncludeTxID(t *testing.T) {
	buf := &bytes.Buffer{}
	b := newTestBroker(buf)
	_, err := b.Handle(context.Background(), Request{
		TxID:    "tx-broker-logged",
		UserID:  "u1",
		Surface: "web",
		Mode:    "teleport", // unknown mode -> error, but start/done still log
	})
	require.NoError(t, err)

	starts := logLinesWith(t, buf, "invoke start")
	dones := logLinesWith(t, buf, "invoke done")
	require.NotEmpty(t, starts, "expected an 'invoke start' log line")
	require.NotEmpty(t, dones, "expected an 'invoke done' log line")
	assert.Equal(t, "tx-broker-logged", starts[0]["txId"])
	assert.Equal(t, "tx-broker-logged", dones[0]["txId"])
	assert.Equal(t, "error", dones[0]["outcome"])
}
