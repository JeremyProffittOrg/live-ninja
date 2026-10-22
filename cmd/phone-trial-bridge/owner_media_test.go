package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func ownerFixture(t *testing.T) ownerBootstrap {
	t.Helper()
	// Exact JSON names and session object shape emitted by broker mode phone-session.
	raw, _ := json.Marshal(map[string]any{"mode": "phone", "engine": "gpt-live-azure", "sessionId": "fresh-owner-session",
		"bridgeToken": "test-phone-token", "bridgeTokenExpiresAt": time.Now().Add(660 * time.Second).UTC().Format(time.RFC3339),
		"callSid": "CA" + strings.Repeat("1", 32), "deadline": time.Now().Add(600 * time.Second).Unix(), "costCapUSD": "1.00",
		"sessionConfig": map[string]any{"type": "realtime", "instructions": "Saved owner persona, rules and memory.", "tool_choice": "auto",
			"tools": []any{map[string]any{"type": "function", "name": "memory_search"}}, "audio": map[string]any{"output": map[string]any{"voice": "marin"}}}})
	var b ownerBootstrap
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	setOwnerCap(t, &b, b.CostCapUSD)
	return b
}

// setOwnerCap gives a loop-only test the validated cap ownerSession would
// have parsed from the bootstrap.
func setOwnerCap(t *testing.T, b *ownerBootstrap, capUSD string) {
	t.Helper()
	b.CostCapUSD = capUSD
	v, err := ownerCostCap(*b)
	if err != nil {
		t.Fatalf("test cap %q: %v", capUSD, err)
	}
	b.capUSD = v
}

func TestOwnerSessionUsesBrokerContextAndRejectsExpiredCapability(t *testing.T) {
	b := ownerFixture(t)
	config, err := ownerSession(&b)
	if err != nil {
		t.Fatal(err)
	}
	session := objectAt(config, "session")
	audio := objectAt(session, "audio")
	if !strings.HasPrefix(stringAt(session, "instructions"), "Saved owner persona, rules and memory.") || len(session["tools"].([]any)) != 2 {
		t.Fatal("owner context lost")
	}
	if stringAt(session["tools"].([]any)[1].(map[string]any), "name") != ownerEndCallTool || !strings.Contains(stringAt(session, "instructions"), "only when the owner explicitly asks") {
		t.Fatal("explicit owner hangup tool missing")
	}
	if stringAt(objectAt(objectAt(audio, "input"), "format"), "type") != "audio/pcmu" || stringAt(objectAt(audio, "output"), "voice") != "marin" {
		t.Fatal("wrong owner media configuration")
	}
	b.BridgeTokenExpiresAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	if _, err := ownerSession(&b); err == nil {
		t.Fatal("expired capability accepted")
	}
	b = ownerFixture(t)
	b.Deadline = time.Now().Add(11 * time.Minute).Unix()
	if _, err := ownerSession(&b); err == nil {
		t.Fatal("unbounded deadline accepted")
	}
	for _, bad := range []string{"", "0", "-1", "10.01", "NaN", "abc"} {
		b = ownerFixture(t)
		b.CostCapUSD = bad
		if _, err := ownerSession(&b); err == nil {
			t.Fatalf("invalid cost cap %q accepted", bad)
		}
	}
}

func TestOwnerLoopHangupStaysLocal(t *testing.T) {
	s, _ := fixture()
	b := ownerFixture(t)
	if _, err := ownerSession(&b); err != nil {
		t.Fatal(err)
	}
	s.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Error("hangup must not invoke the owner tool router")
		return nil, io.ErrUnexpectedEOF
	})}
	tw, _ := wsPair(t)
	az, _ := wsPair(t)
	events := make(chan wireEvent, 1)
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "response.function_call_arguments.done", "name": ownerEndCallTool, "call_id": "hangup-one", "arguments": `{}`}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if reason := s.ownerLoop(ctx, &b, tw, az, "MZtest", "capability", events); reason != "owner_hangup" {
		t.Fatalf("hangup did not end the bridge: %s", reason)
	}
}

func TestOwnerLoopInactivityEndsCallAndSpeechRestartsWait(t *testing.T) {
	if ownerIdleTimeout != 120*time.Second {
		t.Fatalf("phone inactivity timeout=%s; want 2 minutes", ownerIdleTimeout)
	}
	s, _ := fixture()
	b := ownerFixture(t)
	tw, peer := wsPair(t)
	az, _ := wsPair(t)
	events := make(chan wireEvent, 1)
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		done <- s.ownerLoopWithTiming(ctx, &b, tw, az, "MZtest", "capability", events, ownerLoopTiming{Idle: 200 * time.Millisecond, WrapHard: ownerWrapHardLimit})
	}()
	time.Sleep(100 * time.Millisecond)
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "input_audio_buffer.speech_started"}}
	if readOwnerEvent(t, peer)["event"] != "clear" {
		t.Fatal("speech not processed")
	}
	select {
	case reason := <-done:
		t.Fatalf("speech did not reset inactivity: %s", reason)
	case <-time.After(250 * time.Millisecond):
	}
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "input_audio_buffer.speech_stopped"}}
	select {
	case reason := <-done:
		if reason != "idle_limit" {
			t.Fatalf("inactivity reason=%s", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("inactivity did not end call")
	}
}

func TestOwnerLoopWebSearchAllocationPrecedesSpendAndDeduplicates(t *testing.T) {
	for _, tc := range []struct {
		name, cap string
		wantCalls int
		wantCost  float64
	}{
		{"admitted", "1.00", 1, 0.03},
		{"cap exhausted", "0.01", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fixture()
			b := ownerFixture(t)
			setOwnerCap(t, &b, tc.cap)
			b.SessionConfig["tools"] = []any{map[string]any{"type": "function", "name": "web_research"}}
			calls := 0
			s.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/api/v1/tools/invoke" {
					t.Fatalf("unexpected request: %s", req.URL.Path)
				}
				if req.Header.Get("X-LN-Capabilities") != "paid-web-search-v2" {
					t.Fatal("search reservation capability missing")
				}
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"tool":"web_research","ok":true,"output":{"summary":"Evidence","sources":[{"title":"Source","url":"https://news.example/report"}]}}`))}, nil
			})}
			tw, _ := wsPair(t)
			az, _ := wsPair(t)
			events := make(chan wireEvent, 3)
			for range 2 {
				events <- wireEvent{Source: "azure", Data: map[string]any{"type": "response.function_call_arguments.done", "name": "web_research", "call_id": "search-once", "arguments": `{"query":"local bank robbery November 2025"}`}}
			}
			events <- wireEvent{Source: "twilio", Data: map[string]any{"event": "stop"}}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			s.ownerLoop(ctx, &b, tw, az, "MZtest", "capability", events)
			if calls != tc.wantCalls || math.Abs(b.CostUSD-tc.wantCost) > 1e-9 {
				t.Fatalf("calls=%d cost=%f; want %d/%f", calls, b.CostUSD, tc.wantCalls, tc.wantCost)
			}
		})
	}
}

func readOwnerEvent(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var v map[string]any
	if err := c.ReadJSON(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOwnerCancellationBeforeResponseCreatedFencesLateAudio(t *testing.T) {
	s, _ := fixture()
	b := ownerFixture(t)
	tw, peer := wsPair(t)
	az, model := wsPair(t)
	events := make(chan wireEvent, 16)
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { done <- s.ownerLoop(ctx, &b, tw, az, "MZtest", "capability", events) }()
	emit := func(kind string, fields map[string]any) {
		fields["type"] = kind
		events <- wireEvent{Source: "azure", Data: fields}
	}
	emit("session.updated", map[string]any{})
	readOwnerEvent(t, model)
	emit("input_audio_buffer.speech_started", map[string]any{})
	if readOwnerEvent(t, model)["type"] != "response.cancel" {
		t.Fatal("cancel missing")
	}
	if readOwnerEvent(t, peer)["event"] != "clear" {
		t.Fatal("clear missing")
	}
	emit("response.created", map[string]any{"response": map[string]any{"id": "late"}})
	emit("response.done", map[string]any{"response": map[string]any{"id": "late", "status": "cancelled", "usage": map[string]any{"input_tokens": float64(1), "output_tokens": float64(1)}}})
	stale := base64.StdEncoding.EncodeToString([]byte("stale"))
	emit("response.output_audio.delta", map[string]any{"response_id": "late", "item_id": "late-item", "delta": stale})
	emit("input_audio_buffer.committed", map[string]any{})
	readOwnerEvent(t, model)
	emit("response.created", map[string]any{"response": map[string]any{"id": "fresh"}})
	fresh := base64.StdEncoding.EncodeToString([]byte("fresh"))
	emit("response.output_audio.delta", map[string]any{"response_id": "fresh", "item_id": "fresh-item", "delta": fresh})
	if stringAt(objectAt(readOwnerEvent(t, peer), "media"), "payload") != fresh {
		t.Fatal("late cancelled audio leaked")
	}
	events <- wireEvent{Source: "twilio", Data: map[string]any{"event": "stop"}}
	if <-done != "remote_hangup" {
		t.Fatal("loop failed")
	}
}

func TestOwnerMediaClosesPlaybackBeforeSlowFinalization(t *testing.T) {
	s, _ := fixture()
	s.cfg.Events = "https://recorder.example/events"
	b := ownerFixture(t)
	bootstrap, _ := json.Marshal(b)
	finalStarted, release := make(chan struct{}), make(chan struct{})
	finish, final := make(chan map[string]any, 1), make(chan map[string]any, 1)
	s.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{}`
		if strings.HasSuffix(req.URL.Path, "/redeem") {
			body = string(bootstrap)
		}
		if req.URL.Path == "/api/v1/transcript" {
			var payload map[string]any
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			final <- payload
			close(finalStarted)
			<-release
		}
		if strings.HasSuffix(req.URL.Path, "/finish") {
			var payload map[string]any
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			finish <- payload
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var config map[string]any
		if conn.ReadJSON(&config) != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "session.updated"})
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if conn.ReadJSON(&config) != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "speaking"}})
		_ = conn.WriteJSON(map[string]any{"type": "response.output_audio.delta", "response_id": "speaking", "item_id": "spoken", "delta": base64.StdEncoding.EncodeToString([]byte("queued telephone audio"))})
		// 1000 input and 900 output tokens without details: priced at the audio rates, $0.028.
		_ = conn.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"id": "speaking", "status": "completed", "usage": map[string]any{"input_tokens": 1000, "output_tokens": 900, "input_token_details": map[string]any{"cached_tokens": 0}}}})
		_ = conn.WriteJSON(map[string]any{"type": "response.function_call_arguments.done", "name": ownerEndCallTool, "call_id": "hangup", "arguments": `{}`})
	}))
	defer azure.Close()
	s.cfg.Azure = strings.Replace(azure.URL, "http://", "ws://", 1)
	host := httptest.NewServer(http.HandlerFunc(s.ownerMedia))
	defer host.Close()
	defer close(release)
	headers := http.Header{"X-Twilio-Signature": []string{signature(s.cfg.Auth, strings.Replace(s.cfg.Base, "https://", "wss://", 1)+"/owner-media", nil)}}
	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(host.URL, "http://", "ws://", 1)+"/owner-media", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"event": "start", "start": map[string]any{"accountSid": s.cfg.Account, "callSid": b.CallSID,
		"streamSid": "MZ" + strings.Repeat("2", 32), "customParameters": map[string]any{"capability": strings.Repeat("3", 64)},
		"mediaFormat": map[string]any{"encoding": "audio/x-mulaw", "sampleRate": 8000, "channels": 1}}}); err != nil {
		t.Fatal(err)
	}
	if readOwnerEvent(t, conn)["event"] != "media" {
		t.Fatal("test did not queue phone audio")
	}
	if readOwnerEvent(t, conn)["event"] != "clear" {
		t.Fatal("queued playback not cleared")
	}
	select {
	case <-finalStarted:
	case <-time.After(time.Second):
		t.Fatal("finalization never reached")
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var message map[string]any
	if err := conn.ReadJSON(&message); err == nil {
		t.Fatal("phone socket still open during blocked finalization")
	}
	release <- struct{}{}
	select {
	case payload := <-finish:
		if payload["reason"] != "owner_hangup" || payload["callSid"] != b.CallSID || payload["transcriptFinalized"] != true {
			t.Fatalf("hangup cleanup was not sent to recorder: %#v", payload)
		}
		// Recorder contract: modelCostUSD is a six-place decimal string priced from every response.done.
		if payload["modelCostUSD"] != "0.028000" || payload["inputTokens"] != float64(1000) || payload["outputTokens"] != float64(900) || payload["cachedInputTokens"] != float64(0) {
			t.Fatalf("finish payload cost fields: %#v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("hangup never requested recorder finish")
	}
	sink := <-final
	cost := objectAt(sink, "cost")
	if sink["final"] != true || sink["sessionId"] != b.SessionID || math.Abs(cost["usd"].(float64)-0.028) > 1e-9 || cost["textTokens"] != float64(1900) || cost["audioTokens"] != float64(0) || cost["cachedTokens"] != float64(0) {
		t.Fatalf("final transcript cost: %#v", sink)
	}
}

func TestOwnerLoopCancelStaleAudioToolsAndTranscriptWire(t *testing.T) {
	s, _ := fixture()
	b := ownerFixture(t)
	requests := make(chan map[string]any, 8)
	s.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "live.jeremy.ninja" || req.Header.Get("Authorization") != "Bearer test-phone-token" {
			t.Error("unbound sink credential")
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		body["path"] = req.URL.Path
		requests <- body
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"data":{"matches":[]}}`)), Header: http.Header{}}, nil
	})}
	tw, twPeer := wsPair(t)
	az, azPeer := wsPair(t)
	events := make(chan wireEvent, 32)
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { done <- s.ownerLoop(ctx, &b, tw, az, "MZtest", "capability", events) }()
	emit := func(kind string, fields map[string]any) {
		fields["type"] = kind
		events <- wireEvent{Source: "azure", Data: fields}
	}
	emit("session.updated", map[string]any{})
	if readOwnerEvent(t, azPeer)["type"] != "response.create" {
		t.Fatal("no greeting")
	}
	emit("response.created", map[string]any{"response": map[string]any{"id": "old"}})
	old := base64.StdEncoding.EncodeToString([]byte("old audio"))
	fresh := base64.StdEncoding.EncodeToString([]byte("fresh audio"))
	emit("response.output_audio.delta", map[string]any{"response_id": "old", "item_id": "old-item", "delta": old})
	if stringAt(objectAt(readOwnerEvent(t, twPeer), "media"), "payload") != old {
		t.Fatal("missing initial audio")
	}
	emit("input_audio_buffer.speech_started", map[string]any{})
	if readOwnerEvent(t, azPeer)["type"] != "response.cancel" {
		t.Fatal("generation not cancelled")
	}
	if readOwnerEvent(t, twPeer)["event"] != "clear" {
		t.Fatal("playback not cleared")
	}
	if readOwnerEvent(t, azPeer)["type"] != "conversation.item.truncate" {
		t.Fatal("unheard audio not truncated")
	}
	emit("response.output_audio.delta", map[string]any{"response_id": "old", "item_id": "old-item", "delta": old})
	emit("response.done", map[string]any{"response": map[string]any{"id": "old", "status": "cancelled", "usage": map[string]any{"input_tokens": float64(100), "output_tokens": float64(20)}}})
	emit("input_audio_buffer.committed", map[string]any{})
	readOwnerEvent(t, azPeer)
	emit("response.created", map[string]any{"response": map[string]any{"id": "new"}})
	emit("response.output_audio.delta", map[string]any{"response_id": "old", "item_id": "old-item", "delta": old})
	emit("response.output_audio.delta", map[string]any{"response_id": "new", "item_id": "new-item", "delta": fresh})
	if stringAt(objectAt(readOwnerEvent(t, twPeer), "media"), "payload") != fresh {
		t.Fatal("stale response audio leaked")
	}
	emit("conversation.item.input_audio_transcription.completed", map[string]any{"item_id": "user-one", "transcript": "Find my saved preference"})
	transcript := <-requests
	if transcript["path"] != "/api/v1/transcript" || transcript["sessionId"] != b.SessionID {
		t.Fatal("wrong transcript session")
	}
	turn := transcript["turns"].([]any)[0].(map[string]any)
	if turn["seq"] != float64(1) || turn["role"] != "user" || turn["engine"] != "gpt-live-azure" {
		t.Fatalf("invalid transcript turn: %#v", turn)
	}
	emit("response.function_call_arguments.done", map[string]any{"name": "memory_search", "call_id": "tool-one", "arguments": `{"query":"preference"}`})
	tool := <-requests
	if tool["path"] != "/api/v1/tools/invoke" || tool["tool"] != "memory_search" || tool["callId"] != "tool-one" || tool["idempotencyKey"] != b.SessionID+":tool-one" {
		t.Fatalf("invalid tool wire: %#v", tool)
	}
	if stringAt(objectAt(readOwnerEvent(t, azPeer), "item"), "type") != "function_call_output" {
		t.Fatal("tool output missing")
	}
	emit("response.done", map[string]any{"response": map[string]any{"id": "new", "status": "completed", "usage": map[string]any{"input_tokens": float64(120), "output_tokens": float64(30), "input_token_details": map[string]any{"audio_tokens": float64(70)}, "output_token_details": map[string]any{"audio_tokens": float64(20)}}}})
	readOwnerEvent(t, azPeer)
	events <- wireEvent{Source: "twilio", Data: map[string]any{"event": "stop"}}
	if reason := <-done; reason != "remote_hangup" {
		t.Fatal(reason)
	}
	if b.InputTokens != 220 || b.OutputTokens != 50 || b.AudioTokens != 90 {
		t.Fatal("model usage lost")
	}
}

func TestOwnerMediaRejectedReplayDoesNotEndActiveCall(t *testing.T) {
	for _, status := range []int{403, 409, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s, _ := fixture()
			s.cfg.Events = "https://recorder.example/events"
			var finishes atomic.Int32
			done := make(chan struct{})
			s.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				code := status
				if strings.HasSuffix(req.URL.Path, "/finish") {
					finishes.Add(1)
					code = 200
				}
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
			})}
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); s.ownerMedia(w, r) }))
			defer host.Close()
			headers := http.Header{"X-Twilio-Signature": []string{signature(s.cfg.Auth, strings.Replace(s.cfg.Base, "https://", "wss://", 1)+"/owner-media", nil)}}
			conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(host.URL, "http://", "ws://", 1)+"/owner-media", headers)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			err = conn.WriteJSON(map[string]any{"event": "start", "start": map[string]any{"accountSid": s.cfg.Account, "callSid": "CA" + strings.Repeat("1", 32), "streamSid": "MZ" + strings.Repeat("2", 32),
				"customParameters": map[string]any{"capability": strings.Repeat("3", 64)}, "mediaFormat": map[string]any{"encoding": "audio/x-mulaw", "sampleRate": 8000, "channels": 1}}})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not terminate")
			}
			want := int32(0)
			if status == 500 {
				want = 1
			}
			if finishes.Load() != want {
				t.Fatalf("finish count=%d want %d", finishes.Load(), want)
			}
		})
	}
}

func ownerUsageEvent(id string, input, output any) wireEvent {
	return wireEvent{Source: "azure", Data: map[string]any{"type": "response.done", "response": map[string]any{
		"id": id, "status": "completed", "usage": map[string]any{"input_tokens": input, "output_tokens": output}}}}
}

func runOwnerUsageEvents(t *testing.T, eventsToSend []wireEvent) (ownerBootstrap, string) {
	t.Helper()
	return runOwnerUsageEventsWithCap(t, eventsToSend, "1.00")
}

func runOwnerUsageEventsWithCap(t *testing.T, eventsToSend []wireEvent, capUSD string) (ownerBootstrap, string) {
	t.Helper()
	s, _ := fixture()
	b := ownerFixture(t)
	setOwnerCap(t, &b, capUSD)
	tw, _ := wsPair(t)
	az, _ := wsPair(t)
	events := make(chan wireEvent, len(eventsToSend)+1)
	for _, event := range eventsToSend {
		events <- event
	}
	events <- wireEvent{Source: "twilio", Data: map[string]any{"event": "stop"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reason := s.ownerLoop(ctx, &b, tw, az, "MZtest", "capability", events)
	return b, reason
}

func TestOwnerUsagePricingUsesDetailsAndNeverUndercounts(t *testing.T) {
	full := map[string]any{
		"input_token_details": map[string]any{"text_tokens": float64(37700), "audio_tokens": float64(917),
			"cached_tokens_details": map[string]any{"text_tokens": float64(30000), "audio_tokens": float64(0)}},
		"output_token_details": map[string]any{"text_tokens": float64(100), "audio_tokens": float64(1913)}}
	got, cached := ownerUsageCostUSD(full, 38617, 2013)
	want := (30000*0.06 + 7700*0.60 + 917*10.00 + 100*2.40 + 1913*20.00) / 1e6
	if math.Abs(got-want) > 1e-12 || cached != 30000 {
		t.Fatalf("detailed price=%f cached=%d want %f / 30000", got, cached, want)
	}
	uncached, cachedNone := ownerUsageCostUSD(map[string]any{
		"input_token_details":  map[string]any{"text_tokens": float64(37700), "audio_tokens": float64(917)},
		"output_token_details": map[string]any{"text_tokens": float64(100), "audio_tokens": float64(1913)}}, 38617, 2013)
	if math.Abs(uncached-(37700*0.60+917*10.00+100*2.40+1913*20.00)/1e6) > 1e-12 || uncached < got || cachedNone != 0 {
		t.Fatalf("missing cache detail must price uncached: %f", uncached)
	}
	// The reported 104 s call: about $0.07, far below any cap.
	if uncached < 0.06 || uncached > 0.08 {
		t.Fatalf("reported call priced at %f", uncached)
	}
	missing, _ := ownerUsageCostUSD(map[string]any{}, 1000, 100)
	if math.Abs(missing-(1000*10.00+100*20.00)/1e6) > 1e-12 {
		t.Fatalf("missing details must use the highest rates: %f", missing)
	}
	inconsistent, _ := ownerUsageCostUSD(map[string]any{"input_token_details": map[string]any{"text_tokens": float64(5000), "audio_tokens": float64(0)}}, 1000, 0)
	if math.Abs(inconsistent-1000*10.00/1e6) > 1e-12 {
		t.Fatalf("inconsistent details must use the highest rate: %f", inconsistent)
	}
}

func TestOwnerLoopLongCallIsNotEndedByTokenCounts(t *testing.T) {
	events := []wireEvent{}
	// 38600 input tokens ended the reported call at 104 s; text-heavy turns now continue.
	for i := 0; i < 12; i++ {
		event := ownerUsageEvent("text-"+strings.Repeat("x", i+1), float64(13000), float64(600))
		usage := objectAt(objectAt(event.Data, "response"), "usage")
		usage["input_token_details"] = map[string]any{"text_tokens": float64(13000), "audio_tokens": float64(0)}
		usage["output_token_details"] = map[string]any{"text_tokens": float64(0), "audio_tokens": float64(600)}
		events = append(events, event)
	}
	b, reason := runOwnerUsageEventsWithCap(t, events, "10.00")
	if reason != "remote_hangup" || b.InputTokens != 156000 || b.CostUSD > 0.25 {
		t.Fatalf("reason=%s input=%d cost=%f", reason, b.InputTokens, b.CostUSD)
	}
}

func TestOwnerLoopCostCapSaysGoodbyeThenEnds(t *testing.T) {
	s, _ := fixture()
	b := ownerFixture(t)
	// The fixture session reserves about $0.08 for a reply and $0.06 for the
	// goodbye (ownerReserveUSD), so a $0.20 cap admits the greeting.
	setOwnerCap(t, &b, "0.20")
	tw, twPeer := wsPair(t)
	az, azPeer := wsPair(t)
	events := make(chan wireEvent, 16)
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { done <- s.ownerLoop(ctx, &b, tw, az, "MZtest", "capability", events) }()
	emit := func(fields map[string]any) { events <- wireEvent{Source: "azure", Data: fields} }
	emit(map[string]any{"type": "session.updated"})
	if readOwnerEvent(t, azPeer)["type"] != "response.create" {
		t.Fatal("no greeting")
	}
	emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "greeting"}})
	// 1000 input and 3000 output tokens with no details: $0.07. Another reply
	// ($0.08) plus the goodbye ($0.06) would pass $0.20, so the goodbye starts now.
	emit(ownerUsageEvent("greeting", float64(1000), float64(3000)).Data)
	goodbye := readOwnerEvent(t, azPeer)
	response := objectAt(goodbye, "response")
	if goodbye["type"] != "response.create" || response["tool_choice"] != "none" || !strings.Contains(stringAt(response, "instructions"), "cost limit") {
		t.Fatalf("no spoken goodbye before the cost cap: %#v", goodbye)
	}
	// Speech during the goodbye neither cancels nor clears it.
	emit(map[string]any{"type": "input_audio_buffer.speech_started"})
	emit(map[string]any{"type": "input_audio_buffer.committed"})
	emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "bye"}})
	audio := base64.StdEncoding.EncodeToString(make([]byte, 800)) // 100 ms of audio
	emit(map[string]any{"type": "response.output_audio.delta", "response_id": "bye", "item_id": "bye-item", "delta": audio})
	if stringAt(objectAt(readOwnerEvent(t, twPeer), "media"), "payload") != audio {
		t.Fatal("goodbye audio not played")
	}
	emit(ownerUsageEvent("bye", float64(100), float64(50)).Data)
	select {
	case reason := <-done:
		if reason != "cost_limit" {
			t.Fatalf("reason=%s", reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call did not end after the goodbye")
	}
	if b.CostUSD > 0.20 {
		t.Fatalf("call cost %f exceeded its $0.20 cap", b.CostUSD)
	}
	azPeer.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var extra map[string]any
	if azPeer.ReadJSON(&extra) == nil {
		t.Fatalf("unexpected Azure message after goodbye: %#v", extra)
	}
}

func TestOwnerLoopDurationLimitSaysGoodbyeBeforeDeadline(t *testing.T) {
	s, _ := fixture()
	b := ownerFixture(t)
	b.Deadline = time.Now().Add(31 * time.Second).Unix()
	tw, _ := wsPair(t)
	az, azPeer := wsPair(t)
	events := make(chan wireEvent, 8)
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { done <- s.ownerLoop(ctx, &b, tw, az, "MZtest", "capability", events) }()
	goodbye := readOwnerEvent(t, azPeer)
	if goodbye["type"] != "response.create" || !strings.Contains(stringAt(objectAt(goodbye, "response"), "instructions"), "time limit") {
		t.Fatalf("no spoken goodbye before the time limit: %#v", goodbye)
	}
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "response.created", "response": map[string]any{"id": "bye"}}}
	events <- ownerUsageEvent("bye", float64(10), float64(10))
	if reason := <-done; reason != "duration_limit" {
		t.Fatalf("reason=%s", reason)
	}
	if ownerCallMaxSeconds != 600 || ownerIdleTimeout != 2*time.Minute {
		t.Fatal("call limits changed")
	}
}

func TestOwnerLoopUsageDeduplicatesCancelledResponses(t *testing.T) {
	first := ownerUsageEvent("cancelled", float64(120), float64(30))
	response := objectAt(first.Data, "response")
	response["status"] = "cancelled"
	usage := objectAt(response, "usage")
	usage["input_token_details"] = map[string]any{"audio_tokens": float64(90)}
	usage["output_token_details"] = map[string]any{"audio_tokens": float64(20)}
	b, reason := runOwnerUsageEvents(t, []wireEvent{first, first, ownerUsageEvent("next", float64(20), float64(10))})
	if reason != "remote_hangup" || b.InputTokens != 140 || b.OutputTokens != 40 || b.AudioTokens != 110 {
		t.Fatalf("duplicate/cancelled accounting: reason=%s usage=%d/%d audio=%d", reason, b.InputTokens, b.OutputTokens, b.AudioTokens)
	}
}

func TestOwnerLoopUsageRejectsMalformedCounters(t *testing.T) {
	for _, test := range []struct {
		name          string
		input, output any
	}{
		{"missing_input", nil, float64(1)},
		{"missing_output", float64(1), nil},
		{"negative_input", float64(-1), float64(1)},
		{"negative_output", float64(1), float64(-1)},
		{"fractional_input", 1.5, float64(1)},
		{"fractional_output", float64(1), 1.5},
		{"string_input", "1", float64(1)},
		{"overflow_input", 1e30, float64(1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, reason := runOwnerUsageEvents(t, []wireEvent{ownerUsageEvent("bad", test.input, test.output)})
			if reason != "missing_usage" || b.InputTokens != 0 || b.OutputTokens != 0 {
				t.Fatalf("malformed counters accepted: reason=%s usage=%d/%d", reason, b.InputTokens, b.OutputTokens)
			}
		})
	}
	t.Run("audio_totals_exceed_total_usage", func(t *testing.T) {
		event := ownerUsageEvent("bad-audio", float64(10), float64(10))
		usage := objectAt(objectAt(event.Data, "response"), "usage")
		usage["input_token_details"] = map[string]any{"audio_tokens": float64(20)}
		usage["output_token_details"] = map[string]any{"audio_tokens": float64(20)}
		_, reason := runOwnerUsageEvents(t, []wireEvent{event})
		if reason != "invalid_usage" {
			t.Fatalf("invalid audio accounting accepted: %s", reason)
		}
	})
}

func TestOwnerLoopSinkFailure(t *testing.T) {
	for _, sink := range []string{"transcript", "tool"} {
		for _, failure := range []string{"http", "network", "oversized"} {
			t.Run(sink+"_"+failure, func(t *testing.T) {
				s, _ := fixture()
				b := ownerFixture(t)
				calls := 0
				s.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if failure == "network" {
						return nil, io.ErrUnexpectedEOF
					}
					status, body := 503, `{}`
					if failure == "oversized" {
						status, body = 200, strings.Repeat("x", 65537)
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
				})}
				event := map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": "turn-one", "transcript": "Saved preference"}
				want := "transcript_failed"
				if sink == "tool" {
					event = map[string]any{"type": "response.function_call_arguments.done", "name": "memory_search", "call_id": "tool-one", "arguments": `{"query":"preference"}`}
					want = "tool_failed"
				}
				tw, _ := wsPair(t)
				az, _ := wsPair(t)
				events := make(chan wireEvent, 2)
				events <- wireEvent{Source: "azure", Data: event}
				events <- wireEvent{Source: "twilio", Data: map[string]any{"event": "stop"}}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				reason := s.ownerLoop(ctx, &b, tw, az, "MZtest", "capability", events)
				if reason != want || calls != 1 {
					t.Fatalf("reason=%s calls=%d; want %s once with no implicit retry", reason, calls, want)
				}
			})
		}
	}
}

func TestOwnerSessionAcceptsTenMinuteDeadlineAndRejectsLonger(t *testing.T) {
	b := ownerFixture(t)
	b.Deadline = time.Now().Add(600 * time.Second).Unix()
	b.BridgeTokenExpiresAt = time.Now().Add(700 * time.Second).UTC().Format(time.RFC3339)
	if _, err := ownerSession(&b); err != nil {
		t.Fatalf("600 s deadline rejected: %v", err)
	}
	for _, seconds := range []int{610, 700} {
		b = ownerFixture(t)
		b.Deadline = time.Now().Add(time.Duration(seconds) * time.Second).Unix()
		b.BridgeTokenExpiresAt = time.Now().Add(time.Duration(seconds+100) * time.Second).UTC().Format(time.RFC3339)
		if _, err := ownerSession(&b); err == nil || err.Error() != "invalid phone deadline" {
			t.Fatalf("%d s deadline accepted: %v", seconds, err)
		}
	}
	b = ownerFixture(t)
	b.CostCapUSD = ""
	if _, err := ownerSession(&b); err == nil || err.Error() != "invalid call cost cap" {
		t.Fatalf("missing cost cap accepted: %v", err)
	}
	if ownerCallMaxSeconds != 600 || ownerCallMaxCostUSD != 10.0 || ownerWrapBeforeLimit != 30 || ownerWrapCostRatio != 0.9 {
		t.Fatal("owner call caps changed from the 2026-10-19 decision")
	}
}

func TestOwnerUsagePricingReadsCachedTokensWithoutDetails(t *testing.T) {
	// Azure reports cache hits as input_token_details.cached_tokens; the split
	// into cached text and cached audio may be missing. Cached tokens of
	// unknown kind are taken from text first, the smaller discount.
	split := map[string]any{"input_token_details": map[string]any{"text_tokens": float64(3000), "audio_tokens": float64(500), "cached_tokens": float64(3200)}}
	got, cached := ownerUsageCostUSD(split, 3500, 0)
	want := (3000*0.06 + 200*0.30 + 300*10.00) / 1e6
	if math.Abs(got-want) > 1e-12 || cached != 3200 {
		t.Fatalf("cached total without split priced %f cached %d want %f / 3200", got, cached, want)
	}
	// A known split of 2000 cached text and 500 cached audio discounts 500
	// audio tokens where the estimate discounted 200, so it is strictly cheaper.
	detailed := map[string]any{"input_token_details": map[string]any{"text_tokens": float64(3000), "audio_tokens": float64(500), "cached_tokens": float64(2500),
		"cached_tokens_details": map[string]any{"text_tokens": float64(2000), "audio_tokens": float64(500)}}}
	d, cachedSplit := ownerUsageCostUSD(detailed, 3500, 0)
	if wantSplit := (2000*0.06 + 1000*0.60 + 500*0.30) / 1e6; math.Abs(d-wantSplit) > 1e-12 || d >= got || cachedSplit != 2500 {
		t.Fatalf("known split priced %f cached %d; want %f below the estimate %f", d, cachedSplit, wantSplit, got)
	}
	noSplit := map[string]any{"input_token_details": map[string]any{"cached_tokens": float64(1000)}}
	if got, cached := ownerUsageCostUSD(noSplit, 1500, 0); math.Abs(got-(1000*0.30+500*10.00)/1e6) > 1e-12 || cached != 1000 {
		t.Fatalf("cached tokens of unknown kind priced %f cached %d", got, cached)
	}
	// An impossible cached count is neither discounted nor reported: the
	// count the finish payload carries is the count the price used.
	tooMany := map[string]any{"input_token_details": map[string]any{"cached_tokens": float64(2000)}}
	if got, cached := ownerUsageCostUSD(tooMany, 1500, 0); math.Abs(got-1500*10.00/1e6) > 1e-12 || cached != 0 {
		t.Fatalf("impossible cached count must not discount or count: %f cached %d", got, cached)
	}
	if _, cached := ownerUsageCostUSD(map[string]any{}, 10, 0); cached != 0 {
		t.Fatal("cached token count misread")
	}
	// The loop accumulates the count so a cache hit is visible in the finish payload.
	first := ownerUsageEvent("a", float64(2000), float64(10))
	objectAt(objectAt(first.Data, "response"), "usage")["input_token_details"] = map[string]any{"cached_tokens": float64(1536)}
	second := ownerUsageEvent("b", float64(2500), float64(10))
	objectAt(objectAt(second.Data, "response"), "usage")["input_token_details"] = map[string]any{"cached_tokens": float64(2048)}
	b, reason := runOwnerUsageEventsWithCap(t, []wireEvent{first, second}, "10.00")
	if reason != "remote_hangup" || b.CachedTokens != 3584 || b.InputTokens != 4500 {
		t.Fatalf("cached accounting: reason=%s cached=%d input=%d", reason, b.CachedTokens, b.InputTokens)
	}
}

func TestOwnerLoopGoodbyeThatNeverCompletesStillEndsCall(t *testing.T) {
	s, _ := fixture()
	b := ownerFixture(t)
	setOwnerCap(t, &b, "0.20")
	tw, _ := wsPair(t)
	az, azPeer := wsPair(t)
	events := make(chan wireEvent, 16)
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	timing := ownerLoopTiming{Idle: ownerIdleTimeout, WrapHard: 300 * time.Millisecond}
	go func() { done <- s.ownerLoopWithTiming(ctx, &b, tw, az, "MZtest", "capability", events, timing) }()
	emit := func(fields map[string]any) { events <- wireEvent{Source: "azure", Data: fields} }
	emit(map[string]any{"type": "session.updated"})
	if readOwnerEvent(t, azPeer)["type"] != "response.create" {
		t.Fatal("no greeting")
	}
	emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "greeting"}})
	emit(ownerUsageEvent("greeting", float64(1000), float64(3000)).Data)
	requested := time.Now()
	if goodbye := readOwnerEvent(t, azPeer); goodbye["type"] != "response.create" || !strings.Contains(stringAt(objectAt(goodbye, "response"), "instructions"), "cost limit") {
		t.Fatalf("no spoken goodbye: %#v", goodbye)
	}
	// The model never answers the goodbye. The call still ends with the cap reason.
	select {
	case reason := <-done:
		if reason != "cost_limit" {
			t.Fatalf("reason=%s", reason)
		}
		if time.Since(requested) < 250*time.Millisecond {
			t.Fatal("call ended before the goodbye had any time to play")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stalled goodbye kept the call open")
	}
	if ownerWrapHardLimit < 10*time.Second || ownerWrapHardLimit >= ownerWrapBeforeLimit*time.Second {
		t.Fatalf("goodbye hard limit %s must fit inside the %d s wrap-up window", ownerWrapHardLimit, ownerWrapBeforeLimit)
	}
}

func TestOwnerReserveKeepsReplyAndGoodbyeUnderSmallCap(t *testing.T) {
	// A real owner session: long instructions, several tools, admitted at the
	// end of a day with $0.20 left. One reply plus the goodbye cannot fit, so
	// the only response is the goodbye and the measured cost stays under cap.
	b := ownerFixture(t)
	b.SessionConfig["instructions"] = strings.Repeat("Owner persona, rules and remembered facts. ", 550) // ~24000 chars
	b.SessionConfig["tools"] = []any{map[string]any{"type": "function", "name": "memory_search"}, map[string]any{"type": "function", "name": "weather"}}
	b.CostCapUSD = "0.20"
	if _, err := ownerSession(&b); err != nil {
		t.Fatal(err)
	}
	reply, goodbye := ownerReserveUSD(b.SessionConfig)
	// 6000 context + ~8000 instruction tokens at $10/M plus 1024 or 160 output tokens at $20/M.
	if reply < 0.14 || reply > 0.20 || goodbye < 0.12 || goodbye > 0.18 || goodbye >= reply || reply+goodbye <= 0.20 {
		t.Fatalf("reserves reply=%f goodbye=%f", reply, goodbye)
	}
	if b.capUSD != 0.20 {
		t.Fatalf("validated cap not carried: %f", b.capUSD)
	}
	s, _ := fixture()
	tw, _ := wsPair(t)
	az, azPeer := wsPair(t)
	events := make(chan wireEvent, 16)
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { done <- s.ownerLoop(ctx, &b, tw, az, "MZtest", "capability", events) }()
	emit := func(fields map[string]any) { events <- wireEvent{Source: "azure", Data: fields} }
	emit(map[string]any{"type": "session.updated"})
	first := readOwnerEvent(t, azPeer)
	response := objectAt(first, "response")
	if first["type"] != "response.create" || response["tool_choice"] != "none" || !strings.Contains(stringAt(response, "instructions"), "cost limit") || response["max_output_tokens"] != float64(ownerGoodbyeTokens) {
		t.Fatalf("a cap below one reply plus the goodbye must get the goodbye first: %#v", first)
	}
	emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "bye"}})
	// The goodbye re-reads the whole context: 14000 input tokens without details at $10/M.
	emit(ownerUsageEvent("bye", float64(14000), float64(120)).Data)
	select {
	case reason := <-done:
		if reason != "cost_limit" {
			t.Fatalf("reason=%s", reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("goodbye-only call did not end")
	}
	if b.CostUSD > 0.20 || b.CostUSD < 0.14 {
		t.Fatalf("goodbye-only call cost %f; want within the $0.20 cap", b.CostUSD)
	}
	azPeer.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var extra map[string]any
	if azPeer.ReadJSON(&extra) == nil {
		t.Fatalf("a second response was requested under a one-response cap: %#v", extra)
	}
}

func TestOwnerLoopWrapDuringPendingReplyWaitsThenSaysGoodbye(t *testing.T) {
	s, _ := fixture()
	b := ownerFixture(t)
	// The wrap-up timer fires about one second in, while the greeting is still in flight.
	b.Deadline = time.Now().Add((ownerWrapBeforeLimit + 1) * time.Second).Unix()
	tw, _ := wsPair(t)
	az, azPeer := wsPair(t)
	events := make(chan wireEvent, 16)
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	go func() { done <- s.ownerLoop(ctx, &b, tw, az, "MZtest", "capability", events) }()
	// A gorilla connection keeps a read timeout as a permanent error, so the
	// model side is read by one goroutine and observed through a channel.
	model := make(chan map[string]any, 8)
	go func() {
		for {
			var v map[string]any
			if azPeer.ReadJSON(&v) != nil {
				return
			}
			model <- v
		}
	}()
	next := func(wait time.Duration) (map[string]any, bool) {
		select {
		case v := <-model:
			return v, true
		case <-time.After(wait):
			return nil, false
		}
	}
	emit := func(fields map[string]any) { events <- wireEvent{Source: "azure", Data: fields} }
	emit(map[string]any{"type": "session.updated"})
	greeting, ok := next(2 * time.Second)
	if !ok || greeting["type"] != "response.create" || objectAt(greeting, "response")["tool_choice"] == "none" {
		t.Fatalf("no greeting: %#v", greeting)
	}
	emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "greeting"}})
	// Nothing is sent to the model while the greeting is still being generated.
	if early, ok := next(1500 * time.Millisecond); ok {
		t.Fatalf("goodbye interrupted the reply in flight: %#v", early)
	}
	select {
	case reason := <-done:
		t.Fatalf("call ended during the reply in flight: %s", reason)
	default:
	}
	emit(ownerUsageEvent("greeting", float64(500), float64(200)).Data)
	goodbye, ok := next(2 * time.Second)
	response := objectAt(goodbye, "response")
	if !ok || goodbye["type"] != "response.create" || response["tool_choice"] != "none" || !strings.Contains(stringAt(response, "instructions"), "time limit") {
		t.Fatalf("goodbye did not follow the completed reply: %#v", goodbye)
	}
	emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "bye"}})
	emit(ownerUsageEvent("bye", float64(100), float64(50)).Data)
	select {
	case reason := <-done:
		if reason != "duration_limit" {
			t.Fatalf("reason=%s", reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call did not end after the goodbye")
	}
	if extra, ok := next(100 * time.Millisecond); ok {
		t.Fatalf("more than one goodbye requested: %#v", extra)
	}
}

func TestOwnerLoopGoodbyeOutranksQueuedRequestAtResponseDone(t *testing.T) {
	s, _ := fixture()
	b := ownerFixture(t)
	setOwnerCap(t, &b, "0.20")
	tw, twPeer := wsPair(t)
	az, azPeer := wsPair(t)
	events := make(chan wireEvent, 16)
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { done <- s.ownerLoop(ctx, &b, tw, az, "MZtest", "capability", events) }()
	emit := func(fields map[string]any) { events <- wireEvent{Source: "azure", Data: fields} }
	emit(map[string]any{"type": "session.updated"})
	if readOwnerEvent(t, azPeer)["type"] != "response.create" {
		t.Fatal("no greeting")
	}
	emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "greeting"}})
	// The owner speaks while the greeting is in flight: a reply is queued.
	emit(map[string]any{"type": "input_audio_buffer.speech_started"})
	if readOwnerEvent(t, azPeer)["type"] != "response.cancel" {
		t.Fatal("cancel missing")
	}
	if readOwnerEvent(t, twPeer)["event"] != "clear" {
		t.Fatal("clear missing")
	}
	emit(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	emit(map[string]any{"type": "input_audio_buffer.committed"})
	// The greeting's usage ($0.07) leaves no room for the queued reply plus a goodbye.
	emit(ownerUsageEvent("greeting", float64(1000), float64(3000)).Data)
	next := readOwnerEvent(t, azPeer)
	response := objectAt(next, "response")
	if next["type"] != "response.create" || response["tool_choice"] != "none" || !strings.Contains(stringAt(response, "instructions"), "cost limit") {
		t.Fatalf("queued reply was sent instead of the goodbye: %#v", next)
	}
	emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "bye"}})
	emit(ownerUsageEvent("bye", float64(100), float64(50)).Data)
	if reason := <-done; reason != "cost_limit" {
		t.Fatalf("reason=%s", reason)
	}
	azPeer.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var extra map[string]any
	if azPeer.ReadJSON(&extra) == nil {
		t.Fatalf("queued reply sent after the goodbye: %#v", extra)
	}
}

func TestOwnerLoopContextDeadlineDuringCostGoodbyeKeepsCostReason(t *testing.T) {
	s, _ := fixture()
	b := ownerFixture(t)
	setOwnerCap(t, &b, "0.01") // below the goodbye reserve: the goodbye is the only response
	tw, _ := wsPair(t)
	az, azPeer := wsPair(t)
	events := make(chan wireEvent, 4)
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	go func() { done <- s.ownerLoop(ctx, &b, tw, az, "MZtest", "capability", events) }()
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "session.updated"}}
	if goodbye := readOwnerEvent(t, azPeer); !strings.Contains(stringAt(objectAt(goodbye, "response"), "instructions"), "cost limit") {
		t.Fatalf("no cost goodbye: %#v", goodbye)
	}
	// The model never answers and the call deadline passes first.
	select {
	case reason := <-done:
		if reason != "cost_limit" {
			t.Fatalf("deadline during a cost goodbye reported %s; want cost_limit", reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not observe the context deadline")
	}
}

func TestOwnerFinishLogsRecorderRejection(t *testing.T) {
	s, _ := fixture()
	s.cfg.Events = "https://recorder.example/events"
	for _, test := range []struct{ name, want string }{{"rejected", "owner finish rejected: status 400 reason cost_limit"}, {"network", "owner finish failed: recorder unavailable reason cost_limit"}} {
		t.Run(test.name, func(t *testing.T) {
			var payload map[string]any
			s.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/inbound-owner/finish") {
					t.Errorf("unexpected request %s", req.URL.Path)
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if test.name == "network" {
					return nil, io.ErrUnexpectedEOF
				}
				return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})}
			var logged strings.Builder
			log.SetOutput(&logged)
			defer log.SetOutput(os.Stderr)
			b := ownerFixture(t)
			b.InputTokens, b.OutputTokens, b.CachedTokens, b.CostUSD = 156000, 9000, 40000, 1.2345678
			s.ownerFinish(strings.Repeat("3", 64), b.CallSID, true, "cost_limit", b)
			if !strings.Contains(logged.String(), test.want) {
				t.Fatalf("finish failure not logged: %q", logged.String())
			}
			if strings.Contains(logged.String(), b.CallSID) || strings.Contains(logged.String(), "333") {
				t.Fatalf("finish log carries identifiers: %q", logged.String())
			}
			if payload["modelCostUSD"] != "1.234568" || payload["inputTokens"] != float64(156000) || payload["cachedInputTokens"] != float64(40000) || payload["reason"] != "cost_limit" {
				t.Fatalf("finish payload: %#v", payload)
			}
		})
	}
}
