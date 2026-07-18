package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

// --- fakes ---------------------------------------------------------------

// fakeClient is a scripted clientConn. It hands out inbound events in order,
// then blocks on hangup and finally returns ErrWSClosed; outbound events are
// captured on a buffered channel.
type fakeClient struct {
	inbound []voiceengine.Event
	idx     int
	hangup  chan struct{}
	out     chan voiceengine.Event
}

func newFakeClient(inbound []voiceengine.Event) *fakeClient {
	return &fakeClient{
		inbound: inbound,
		hangup:  make(chan struct{}),
		out:     make(chan voiceengine.Event, 256),
	}
}

func (f *fakeClient) ReadEvent() (voiceengine.Event, error) {
	if f.idx < len(f.inbound) {
		ev := f.inbound[f.idx]
		f.idx++
		return ev, nil
	}
	<-f.hangup
	return voiceengine.Event{}, ErrWSClosed
}

func (f *fakeClient) WriteEvent(ev voiceengine.Event) error {
	f.out <- ev
	return nil
}

func (f *fakeClient) Close() error { return nil }

// fakeNova is a scripted novaStream. It returns queued output documents in
// order, then blocks until novaDone before returning io.EOF; every sent
// document is captured.
type fakeNova struct {
	output   [][]byte
	idx      int
	novaDone chan struct{}

	mu   sync.Mutex
	sent [][]byte
}

func newFakeNova(output [][]byte) *fakeNova {
	return &fakeNova{output: output, novaDone: make(chan struct{})}
}

func (f *fakeNova) Send(_ context.Context, payload []byte) error {
	f.mu.Lock()
	cp := make([]byte, len(payload))
	copy(cp, payload)
	f.sent = append(f.sent, cp)
	f.mu.Unlock()
	return nil
}

func (f *fakeNova) Recv(ctx context.Context) ([]byte, error) {
	if f.idx < len(f.output) {
		doc := f.output[f.idx]
		f.idx++
		return doc, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.novaDone:
		return nil, io.EOF
	}
}

func (f *fakeNova) Close() error { return nil }

func (f *fakeNova) sentEventNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.sent))
	for _, doc := range f.sent {
		names = append(names, novaEventName(doc))
	}
	return names
}

func novaEventName(doc []byte) string {
	var env struct {
		Event map[string]json.RawMessage `json:"event"`
	}
	if json.Unmarshal(doc, &env) != nil {
		return "?"
	}
	for k := range env.Event {
		return k
	}
	return ""
}

// novaDoc builds a Nova output document {"event":{name: body}}.
func novaDoc(t *testing.T, name string, body map[string]any) []byte {
	t.Helper()
	inner, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	doc, err := json.Marshal(map[string]any{"event": map[string]json.RawMessage{name: inner}})
	if err != nil {
		t.Fatalf("marshal envelope %s: %v", name, err)
	}
	return doc
}

// --- test ----------------------------------------------------------------

func TestSessionPump_EndToEnd(t *testing.T) {
	log := observ.NewLogger(io.Discard, "error")

	// Sink server captures the transcript flush and answers tool calls.
	var (
		sinkMu      sync.Mutex
		gotTurns    []transcriptTurn
		gotFinal    bool
		gotToolBody toolInvokeBody
	)
	sinkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sinkMu.Lock()
		defer sinkMu.Unlock()
		switch r.URL.Path {
		case "/api/v1/transcript":
			// Turns settle across several flushes; aggregate them all and
			// remember whether a final marker was seen.
			var b transcriptBody
			_ = json.Unmarshal(body, &b)
			gotTurns = append(gotTurns, b.Turns...)
			gotFinal = gotFinal || b.Final
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"written":1}`))
		case "/api/v1/tools/invoke":
			_ = json.Unmarshal(body, &gotToolBody)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true,"result":"sunny, 24C"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer sinkSrv.Close()

	// Client script: configure, then stream one audio frame.
	client := newFakeClient([]voiceengine.Event{
		{Type: voiceengine.TypeSessionStart, Config: &voiceengine.Config{
			Voice: "tiffany",
			Tools: []voiceengine.ToolSpec{{Name: "get_weather", Description: "weather"}},
		}},
		{Type: voiceengine.TypeAudioIn, Audio: "QUJD", SampleRate: 16000},
	})

	// Nova script: a final user transcript, an assistant turn (text + audio),
	// a barge-in end, and a tool call.
	nova := newFakeNova([][]byte{
		novaDoc(t, "contentStart", map[string]any{
			"contentName": "u1", "type": "TEXT", "role": "USER",
			"additionalModelFields": `{"generationStage":"FINAL"}`,
		}),
		novaDoc(t, "textOutput", map[string]any{"contentName": "u1", "content": "what's the weather", "role": "USER"}),
		novaDoc(t, "contentEnd", map[string]any{"contentName": "u1", "type": "TEXT", "stopReason": "END_TURN"}),

		novaDoc(t, "contentStart", map[string]any{
			"contentName": "a1", "type": "TEXT", "role": "ASSISTANT",
			"additionalModelFields": `{"generationStage":"FINAL"}`,
		}),
		novaDoc(t, "textOutput", map[string]any{"contentName": "a1", "content": "Let me check.", "role": "ASSISTANT"}),
		novaDoc(t, "contentEnd", map[string]any{"contentName": "a1", "type": "TEXT", "stopReason": "END_TURN"}),

		novaDoc(t, "contentStart", map[string]any{"contentName": "a2", "type": "AUDIO", "role": "ASSISTANT"}),
		novaDoc(t, "audioOutput", map[string]any{"contentName": "a2", "content": "QUJD"}),
		novaDoc(t, "contentEnd", map[string]any{"contentName": "a2", "type": "AUDIO", "stopReason": "INTERRUPTED"}),

		novaDoc(t, "toolUse", map[string]any{
			"contentName": "t1", "toolUseId": "call-1", "toolName": "get_weather",
			"content": `{"city":"Denver"}`,
		}),
	})

	sink := newSinkClient(sinkSrv.Client(), sinkSrv.URL, "test-token")
	sess := newSession(log, client, sink, "sess-123", "web",
		func(_ context.Context) (novaStream, error) { return nova, nil })

	// Collect client-bound events concurrently.
	var (
		collectMu sync.Mutex
		collected []voiceengine.Event
	)
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		for ev := range client.out {
			collectMu.Lock()
			collected = append(collected, ev)
			done := countType(collected, voiceengine.TypeToolResult) >= 1
			collectMu.Unlock()
			if done {
				return
			}
		}
	}()

	runDone := make(chan error, 1)
	go func() { runDone <- sess.Run(context.Background()) }()

	// Wait until the tool result has been echoed (all nova docs processed),
	// then hang up both ends to end the session deterministically.
	select {
	case <-collectDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tool result echo")
	}
	close(nova.novaDone)
	close(client.hangup)

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("session Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for session Run to finish")
	}
	close(client.out)
	<-collectDone // drain the rest

	collectMu.Lock()
	defer collectMu.Unlock()

	// 1. Client received assistant audio.
	if countType(collected, voiceengine.TypeAudioOut) < 1 {
		t.Errorf("client never received audio.out; got %v", typesOf(collected))
	}
	// 2. Client received a final assistant transcript.
	if !hasTranscript(collected, voiceengine.RoleAssistant, "Let me check.", true) {
		t.Errorf("missing final assistant transcript; got %v", collected)
	}
	// 3. Barge-in surfaced as an interrupted turn end.
	if !hasInterruptedTurnEnd(collected) {
		t.Errorf("barge-in (INTERRUPTED) did not surface as turn.end interrupted; got %v", typesOf(collected))
	}
	// 4. Tool call + result echoed to client.
	if countType(collected, voiceengine.TypeToolCall) < 1 || countType(collected, voiceengine.TypeToolResult) < 1 {
		t.Errorf("tool call/result not echoed to client; got %v", typesOf(collected))
	}

	// 5. Nova received the init handshake, the audio frame, the tool result,
	//    and the graceful close trio.
	names := nova.sentEventNames()
	for _, want := range []string{"sessionStart", "promptStart", "contentStart", "audioInput", "toolResult", "promptEnd", "sessionEnd"} {
		if !contains(names, want) {
			t.Errorf("nova never received %q; sent sequence: %v", want, names)
		}
	}

	// 6. Sink received the tool invocation and a final transcript flush.
	sinkMu.Lock()
	defer sinkMu.Unlock()
	if gotToolBody.Tool != "get_weather" {
		t.Errorf("sink tool invoke tool = %q, want get_weather", gotToolBody.Tool)
	}
	if !gotFinal {
		t.Errorf("sink never received a final transcript flush")
	}
	if !turnsHaveText(gotTurns, "what's the weather") || !turnsHaveText(gotTurns, "Let me check.") {
		t.Errorf("sink transcript missing expected turns: %+v", gotTurns)
	}
}

func TestSessionPump_ClientHangupClosesNovaGracefully(t *testing.T) {
	log := observ.NewLogger(io.Discard, "error")
	client := newFakeClient([]voiceengine.Event{
		{Type: voiceengine.TypeSessionStart, Config: &voiceengine.Config{}},
	})
	nova := newFakeNova(nil) // no output; will block until ctx cancel

	sess := newSession(log, client, newSinkClient(http.DefaultClient, "", ""), "s1", "android",
		func(_ context.Context) (novaStream, error) { return nova, nil })

	// Drain client output so nothing blocks.
	go func() {
		for range client.out {
		}
	}()

	runDone := make(chan error, 1)
	go func() { runDone <- sess.Run(context.Background()) }()

	// Client hangs up immediately after config.
	close(client.hangup)

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error on clean hangup: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after client hangup")
	}
	close(client.out)

	names := nova.sentEventNames()
	for _, want := range []string{"contentEnd", "promptEnd", "sessionEnd"} {
		if !contains(names, want) {
			t.Errorf("graceful close missing %q; sent: %v", want, names)
		}
	}
}

// --- helpers -------------------------------------------------------------

func countType(evs []voiceengine.Event, t voiceengine.Type) int {
	n := 0
	for _, e := range evs {
		if e.Type == t {
			n++
		}
	}
	return n
}

func typesOf(evs []voiceengine.Event) []voiceengine.Type {
	out := make([]voiceengine.Type, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func hasTranscript(evs []voiceengine.Event, role, text string, final bool) bool {
	for _, e := range evs {
		if e.Type == voiceengine.TypeTranscript && e.Role == role && e.Text == text && e.Final == final {
			return true
		}
	}
	return false
}

func hasInterruptedTurnEnd(evs []voiceengine.Event) bool {
	for _, e := range evs {
		if e.Type == voiceengine.TypeTurnEnd && e.Interrupted {
			return true
		}
	}
	return false
}

func turnsHaveText(turns []transcriptTurn, text string) bool {
	for _, turn := range turns {
		if turn.Text == text {
			return true
		}
	}
	return false
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
