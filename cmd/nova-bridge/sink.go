package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

// sinkClient makes the two authenticated server-to-server calls a Nova
// session needs against the web API (contracts/api.md):
//
//   - POST /api/v1/transcript   — mirror final transcript turns so topics/
//     memory/history are identical to the OpenAI path (FR-VE-01).
//   - POST /api/v1/tools/invoke — execute a model tool call, re-authorized
//     per call server-side, exactly as the client-direct engines do.
//
// Both carry the caller's own session JWT (the token that authorized the
// bridge connection) as a Bearer credential; the web API verifies it and
// re-checks the allowlist on every tool call. No cookie is sent, so the
// app's CSRF double-submit check is skipped for these machine calls.
type sinkClient struct {
	http    *http.Client
	baseURL string
	token   string
}

func newSinkClient(httpClient *http.Client, baseURL, token string) *sinkClient {
	return &sinkClient{
		http:    httpClient,
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
	}
}

// transcriptTurn matches the POST /api/v1/transcript turn shape.
type transcriptTurn struct {
	Seq    int    `json:"seq"`
	Role   string `json:"role"`
	Text   string `json:"text"`
	Engine string `json:"engine"`
}

type transcriptBody struct {
	SessionID string           `json:"sessionId"`
	Final     bool             `json:"final"`
	Turns     []transcriptTurn `json:"turns"`
}

// FlushTranscript posts a batch of turns (and/or the final marker) for a
// session. A final-only flush with zero turns is valid.
func (s *sinkClient) FlushTranscript(ctx context.Context, sessionID string, turns []transcriptTurn, final bool) error {
	if s == nil || s.baseURL == "" {
		return nil // sink disabled (e.g. local run without an API base URL)
	}
	if len(turns) == 0 && !final {
		return nil
	}
	body, err := json.Marshal(transcriptBody{SessionID: sessionID, Final: final, Turns: turns})
	if err != nil {
		return err
	}
	resp, err := s.post(ctx, "/api/v1/transcript", body)
	if err != nil {
		return err
	}
	defer drainClose(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("nova-bridge: transcript sink status %d", resp.StatusCode)
	}
	return nil
}

// toolInvokeBody matches the POST /api/v1/tools/invoke request shape.
type toolInvokeBody struct {
	Tool           string          `json:"tool"`
	Args           json.RawMessage `json:"args,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	CallID         string          `json:"callId,omitempty"`
}

// InvokeTool runs one model tool call and returns the raw JSON result body
// (fed straight back to Nova as the toolResult content) plus the HTTP
// status. A non-2xx still returns its body so the model gets a structured
// error rather than a stall.
func (s *sinkClient) InvokeTool(ctx context.Context, call voiceengine.Event) (result []byte, status int, err error) {
	if s == nil || s.baseURL == "" {
		return []byte(`{"error":"tools_unavailable"}`), http.StatusServiceUnavailable, nil
	}
	body, err := json.Marshal(toolInvokeBody{
		Tool:           call.ToolName,
		Args:           call.ToolArgs,
		IdempotencyKey: call.ToolCallID, // per-call idempotency for side-effecting tools
		CallID:         call.ToolCallID,
	})
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.post(ctx, "/api/v1/tools/invoke", body)
	if err != nil {
		return nil, 0, err
	}
	defer drainClose(resp.Body)
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxMessageBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}

func (s *sinkClient) post(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)
	return s.http.Do(req)
}

func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 4096))
	_ = rc.Close()
}
