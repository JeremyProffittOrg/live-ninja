package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		"bridgeToken": "test-phone-token", "bridgeTokenExpiresAt": time.Now().Add(240 * time.Second).UTC().Format(time.RFC3339),
		"callSid": "CA" + strings.Repeat("1", 32), "deadline": time.Now().Add(180 * time.Second).Unix(),
		"sessionConfig": map[string]any{"type": "realtime", "instructions": "Saved owner persona, rules and memory.", "tool_choice": "auto",
			"tools": []any{map[string]any{"type": "function", "name": "memory_search"}}, "audio": map[string]any{"output": map[string]any{"voice": "marin"}}}})
	var b ownerBootstrap
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOwnerSessionUsesBrokerContextAndRejectsExpiredCapability(t *testing.T) {
	b := ownerFixture(t)
	config, err := ownerSession(b)
	if err != nil {
		t.Fatal(err)
	}
	session := objectAt(config, "session")
	audio := objectAt(session, "audio")
	if stringAt(session, "instructions") != "Saved owner persona, rules and memory." || len(session["tools"].([]any)) != 1 {
		t.Fatal("owner context lost")
	}
	if stringAt(objectAt(objectAt(audio, "input"), "format"), "type") != "audio/pcmu" || stringAt(objectAt(audio, "output"), "voice") != "marin" {
		t.Fatal("wrong owner media configuration")
	}
	b.BridgeTokenExpiresAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	if _, err := ownerSession(b); err == nil {
		t.Fatal("expired capability accepted")
	}
	b = ownerFixture(t)
	b.Deadline = time.Now().Add(10 * time.Minute).Unix()
	if _, err := ownerSession(b); err == nil {
		t.Fatal("unbounded deadline accepted")
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
	s.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{}`
		if strings.HasSuffix(req.URL.Path, "/redeem") {
			body = string(bootstrap)
		}
		if req.URL.Path == "/api/v1/transcript" {
			close(finalStarted)
			<-release
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
		_ = conn.WriteJSON(map[string]any{"type": "error"})
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
	s, _ := fixture()
	b := ownerFixture(t)
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

func TestOwnerLoopUsageLimits(t *testing.T) {
	for _, test := range []struct {
		name          string
		count         int
		input, output float64
		requestNext   bool
	}{
		{"input_response_cap", 1, 12001, 0, false},
		{"output_response_cap", 1, 0, 513, false},
		{"input_reservation_stops_next_response", 4, 12000, 1, true},
		{"output_reservation_stops_next_response", 48, 0, 512, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []wireEvent{}
			for i := 0; i < test.count; i++ {
				events = append(events, ownerUsageEvent("response-"+strings.Repeat("x", i+1), test.input, test.output))
			}
			if test.requestNext {
				events = append(events, wireEvent{Source: "azure", Data: map[string]any{"type": "input_audio_buffer.committed"}})
			}
			b, reason := runOwnerUsageEvents(t, events)
			if reason != "budget_limit" || b.InputTokens != int64(test.input)*int64(test.count) || b.OutputTokens != int64(test.output)*int64(test.count) {
				t.Fatalf("reason=%s usage=%d/%d", reason, b.InputTokens, b.OutputTokens)
			}
		})
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
