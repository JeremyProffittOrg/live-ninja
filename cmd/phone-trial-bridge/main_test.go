package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixture() (*server, *call) {
	t := target{ID: "hotel", Name: "Hotel & <Inn>", Phone: "+14155550123"}
	c := &call{SID: "CA" + strings.Repeat("a", 32), Token: "token", Target: t, Deadline: time.Now().Add(time.Minute)}
	s := &server{cfg: config{Base: "https://bridge.example", Account: "ACtest", Auth: "test-secret", KeyID: "SKtest", KeySecret: "test-key", Run: "trial", Max: time.Minute, Targets: map[string]target{"hotel": t}, Events: "https://recorder.example/events", Recording: "https://recorder.example/recordings"}, calls: map[string]*call{c.SID: c}}
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	return s, c
}
func request(s *server, c *call, path, speech string) *http.Request {
	p := url.Values{"AccountSid": {s.cfg.Account}, "CallSid": {c.SID}, "To": {c.Target.Phone}, "SpeechResult": {speech}}
	r := httptest.NewRequest("POST", path, strings.NewReader(p.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Twilio-Signature", signature(s.cfg.Auth, s.cfg.Base+path, p))
	return r
}
func TestSignatureKnownTwilioVector(t *testing.T) {
	p := url.Values{"CallSid": {"CA1234567890ABCDE"}, "Caller": {"+14158675310"}, "Digits": {"1234"}, "From": {"+14158675310"}, "To": {"+18005551212"}}
	if got := signature("12345", "https://example.com/myapp.php?foo=1&bar=2", p); got != "L/OH5YylLD5NRKLltdqwSvS0BnU=" {
		t.Fatalf("signature %s", got)
	}
}
func TestAuthorizeRejectsAlterations(t *testing.T) {
	for _, change := range []string{"signature", "account", "call", "target", "to", "run", "method"} {
		t.Run(change, func(t *testing.T) {
			s, c := fixture()
			path := "/voice?run=trial&target=hotel"
			r := request(s, c, path, "yes")
			switch change {
			case "signature":
				r.Header.Set("X-Twilio-Signature", "bad")
			case "method":
				r.Method = "GET"
			default:
				r.ParseForm()
				switch change {
				case "account":
					r.PostForm.Set("AccountSid", "ACother")
				case "call":
					r.PostForm.Set("CallSid", "../Calls")
				case "target":
					r.URL.RawQuery = "run=trial&target=other"
				case "to":
					r.PostForm.Set("To", "+14155559999")
				case "run":
					r.URL.RawQuery = "run=other&target=hotel"
				}
				r.Body = io.NopCloser(strings.NewReader(r.PostForm.Encode()))
				r.Header.Set("X-Twilio-Signature", signature(s.cfg.Auth, s.cfg.Base+r.URL.RequestURI(), r.PostForm))
			}
			w := httptest.NewRecorder()
			_, ok := s.authorize(w, r)
			if ok || w.Code < 400 {
				t.Fatal("accepted unauthorized request")
			}
		})
	}
}
func TestConsentClassification(t *testing.T) {
	for _, x := range []struct{ speech, digit, want string }{{"Yes.", "", "yes"}, {"no", "", "no"}, {"yes but do not record", "", "unknown"}, {"", "", "unknown"}, {"maybe", "", "unknown"}, {"", "1", "yes"}, {"", "2", "no"}} {
		if got := consentAnswer(x.speech, x.digit); got != x.want {
			t.Errorf("%q = %s", x.speech, got)
		}
	}
}
func TestConsentRecordingOnlyAfterYesAndOnce(t *testing.T) {
	s, c := fixture()
	recordings := 0
	events := []string{}
	outcomes := []string{}
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "Recordings.json") {
			if len(outcomes) != 1 || outcomes[0] != "attempted" {
				t.Fatal("recording request preceded durable intent")
			}
			recordings++
			v, _ := url.ParseQuery(string(b))
			if v.Get("RecordingTrack") != "both" || v.Get("RecordingStatusCallback") != s.cfg.Recording {
				t.Fatal("recording configuration")
			}
		} else {
			var ev map[string]any
			json.Unmarshal(b, &ev)
			events = append(events, ev["type"].(string))
			if ev["type"] == "recording_requested" {
				outcomes = append(outcomes, stringAt(objectAt(ev, "data"), "outcome"))
			}
			mac := hmac.New(sha256.New, []byte(s.cfg.Auth))
			mac.Write(b)
			if r.Header.Get("X-LN-Signature") != hex.EncodeToString(mac.Sum(nil)) {
				t.Fatal("unsigned event")
			}
		}
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		s.consent(w, request(s, c, "/consent?run=trial&target=hotel", "yes"))
		if i == 0 && !strings.Contains(w.Body.String(), "<Connect") {
			t.Fatal(w.Body.String())
		}
	}
	if recordings != 1 || len(events) != 3 || events[0] != "consent" || len(outcomes) != 2 || outcomes[1] != "confirmed" {
		t.Fatalf("recordings=%d events=%v", recordings, events)
	}
}
func TestConsentAmbiguousClarifiesOnceThenEnds(t *testing.T) {
	s, c := fixture()
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		s.consent(w, request(s, c, "/consent?run=trial&target=hotel", "what"))
		if strings.Contains(w.Body.String(), "<Gather") != (i == 0) {
			t.Fatal(w.Body.String())
		}
		if strings.Contains(w.Body.String(), "<Connect") {
			t.Fatal("unconsented connection")
		}
	}
	if !c.Ended || c.Consented {
		t.Fatal("consent state")
	}
}
func TestConsentNoDoesNotRecord(t *testing.T) {
	s, c := fixture()
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/Recordings.json") {
			t.Fatal("recording without consent")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	w := httptest.NewRecorder()
	s.consent(w, request(s, c, "/consent?run=trial&target=hotel", "no"))
	if !c.Ended || strings.Contains(w.Body.String(), "<Connect") {
		t.Fatal("decline not respected")
	}
}
func TestRecordingFailureEndsCall(t *testing.T) {
	s, c := fixture()
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := 200
		if r.URL.Host == "api.twilio.com" {
			status = 500
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	w := httptest.NewRecorder()
	s.consent(w, request(s, c, "/consent?run=trial&target=hotel", "yes"))
	if strings.Contains(w.Body.String(), "<Connect") {
		t.Fatal("continued without recording")
	}
}

func TestRecordingRejectedOutcomeFollowsAttemptedIntent(t *testing.T) {
	s, c := fixture()
	outcomes := []string{}
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := 200
		if strings.HasSuffix(r.URL.Path, "/Recordings.json") {
			if len(outcomes) != 1 || outcomes[0] != "attempted" {
				t.Fatal("provider request before intent")
			}
			status = 400
		} else if r.URL.Host != "api.twilio.com" {
			var event map[string]any
			if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
				t.Fatal(err)
			}
			if event["type"] == "recording_requested" {
				outcomes = append(outcomes, stringAt(objectAt(event, "data"), "outcome"))
			}
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	w := httptest.NewRecorder()
	s.consent(w, request(s, c, "/consent?run=trial&target=hotel", "yes"))
	if !c.Ended || len(outcomes) != 2 || outcomes[1] != "rejected" || strings.Contains(w.Body.String(), "<Connect") {
		t.Fatalf("rejected recording not terminated: outcomes=%v", outcomes)
	}
}

func TestDeletedJournalNeverAcknowledgesRecordingAdmission(t *testing.T) {
	for _, deletedEvent := range []string{"consent", "recording_requested"} {
		t.Run(deletedEvent, func(t *testing.T) {
			s, c := fixture()
			recordings := 0
			s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status := 200
				if strings.HasSuffix(r.URL.Path, "/Recordings.json") {
					recordings++
				}
				if r.URL.Host != "api.twilio.com" {
					var event map[string]any
					if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
						t.Fatal(err)
					}
					if event["type"] == deletedEvent {
						status = 410
					}
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"recordingStatus":"deleted"}`))}, nil
			})
			w := httptest.NewRecorder()
			s.consent(w, request(s, c, "/consent?run=trial&target=hotel", "yes"))
			if recordings != 0 || !c.Ended || strings.Contains(w.Body.String(), "<Connect") {
				t.Fatalf("deleted journal admitted recording: requests=%d ended=%v", recordings, c.Ended)
			}
		})
	}
}
func TestTwiMLEscaping(t *testing.T) {
	s, c := fixture()
	s.cfg.Run = "a&b<bad>"
	w := httptest.NewRecorder()
	twiml(w, s.gather(c.Target, false))
	d := xml.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	for {
		_, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
	}
}
func TestMediaRejectsUnsignedOrUnconsented(t *testing.T) {
	for _, signed := range []bool{false, true} {
		s, _ := fixture()
		r := httptest.NewRequest("GET", "/media/token", nil)
		if signed {
			r.Header.Set("X-Twilio-Signature", signature(s.cfg.Auth, s.cfg.Base+"/media/token", nil))
		}
		w := httptest.NewRecorder()
		s.media(w, r)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
}
func TestBridgeDeadline(t *testing.T) {
	s, c := fixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.bridgeLoop(ctx, c, nil, nil, "", make(chan wireEvent)); got != "duration_limit" {
		t.Fatal(got)
	}
}
func TestGAConfiguration(t *testing.T) {
	v := sessionConfig()
	sess := v["session"].(map[string]any)
	audio := sess["audio"].(map[string]any)
	for _, k := range []string{"input", "output"} {
		f := audio[k].(map[string]any)["format"].(map[string]any)
		if f["type"] != "audio/pcmu" {
			t.Fatal(f)
		}
	}
	if sess["type"] != "realtime" || sess["tools"] == nil {
		t.Fatal("GA session missing")
	}
}
func TestFinishOnce(t *testing.T) {
	s, c := fixture()
	n := 0
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	s.finish(c, "test")
	s.finish(c, "test")
	if n != 2 {
		t.Fatalf("%d requests", n)
	}
}

func wsPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	got := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := websocket.Upgrader{}
		c, e := u.Upgrade(w, r, nil)
		if e != nil {
			return
		}
		got <- c
	}))
	t.Cleanup(srv.Close)
	peer, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if e != nil {
		t.Fatal(e)
	}
	conn := <-got
	t.Cleanup(func() { conn.Close(); peer.Close() })
	return conn, peer
}
func receive(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(time.Second))
	var v map[string]any
	if e := c.ReadJSON(&v); e != nil {
		t.Fatal(e)
	}
	return v
}
func TestAudioBridgeInterruptionAndCompletion(t *testing.T) {
	s, c := fixture()
	tw, twPeer := wsPair(t)
	az, azPeer := wsPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	events := make(chan wireEvent, 10)
	done := make(chan string, 1)
	go func() { done <- s.bridgeLoop(ctx, c, tw, az, "MZtest", events) }()
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "session.updated"}}
	if v := receive(t, azPeer); v["type"] != "response.create" {
		t.Fatal(v)
	}
	events <- wireEvent{Source: "twilio", Data: map[string]any{"event": "media", "media": map[string]any{"track": "inbound", "payload": "////"}}}
	if v := receive(t, azPeer); v["type"] != "input_audio_buffer.append" || v["audio"] != "////" {
		t.Fatal(v)
	}
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "response.output_audio.delta", "item_id": "item1", "delta": "//////////8="}}
	if v := receive(t, twPeer); v["event"] != "media" || v["streamSid"] != "MZtest" {
		t.Fatal(v)
	}
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "input_audio_buffer.speech_started"}}
	if v := receive(t, twPeer); v["event"] != "clear" {
		t.Fatal(v)
	}
	if v := receive(t, azPeer); v["type"] != "conversation.item.truncate" || v["item_id"] != "item1" {
		t.Fatal(v)
	}
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "response.function_call_arguments.done", "name": "end_call", "arguments": "{\"reason\":\"complete\",\"summary\":\"Password only, staff reported.\"}"}}
	mark := receive(t, twPeer)
	if mark["event"] != "mark" {
		t.Fatal(mark)
	}
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "response.done", "response": map[string]any{"id": "resp1", "status": "completed", "usage": map[string]any{"input_tokens": float64(100), "output_tokens": float64(30)}}}}
	events <- wireEvent{Source: "twilio", Data: map[string]any{"event": "mark", "mark": map[string]any{"name": "final-audio"}}}
	if reason := <-done; reason != "research_finished" {
		t.Fatal(reason)
	}
}
func TestExpiredConsentDenied(t *testing.T) {
	s, c := fixture()
	c.Deadline = time.Now().Add(-time.Second)
	w := httptest.NewRecorder()
	s.consent(w, request(s, c, "/consent?run=trial&target=hotel", "yes"))
	if c.Consented || strings.Contains(w.Body.String(), "<Connect") {
		t.Fatal("accepted expired consent")
	}
}

func TestRecorderEventContract(t *testing.T) {
	s, c := fixture()
	gotConsent, gotFinal := false, false
	allowed := map[string]bool{"call_registered": true, "consent": true, "transcript": true, "summary": true, "usage": true, "error": true, "call_ended": true}
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "recorder.example" {
			var ev map[string]any
			if e := json.NewDecoder(r.Body).Decode(&ev); e != nil {
				t.Fatal(e)
			}
			kind := stringAt(ev, "type")
			if !allowed[kind] {
				t.Fatalf("recorder rejects %s", kind)
			}
			if stringAt(ev, "runId") != "trial" || stringAt(ev, "targetId") != "hotel" || stringAt(ev, "callSid") != c.SID {
				t.Fatal("identity contract")
			}
			data := objectAt(ev, "data")
			if kind == "consent" {
				gotConsent = true
				if data["result"] != "declined" {
					t.Fatal(data)
				}
			}
			if kind == "call_ended" {
				gotFinal = true
				if data["usageComplete"] != true || data["inputTokens"] != float64(0) {
					t.Fatal(data)
				}
				if _, ok := data["status"]; ok {
					t.Fatal("unverified provider status")
				}
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	s.consent(httptest.NewRecorder(), request(s, c, "/consent?run=trial&target=hotel", "no"))
	if !gotConsent || !gotFinal {
		t.Fatal("missing consent/final event")
	}
}
func TestTokenBudgetStopsBeforeNextResponse(t *testing.T) {
	s, c := fixture()
	c.ModelStarted = true
	tw, _ := wsPair(t)
	az, azPeer := wsPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	events := make(chan wireEvent, 10)
	done := make(chan string, 1)
	go func() { done <- s.bridgeLoop(ctx, c, tw, az, "MZtest", events) }()
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "session.updated"}}
	for i := 0; i < 4; i++ {
		if v := receive(t, azPeer); v["type"] != "response.create" {
			t.Fatal(v)
		}
		events <- wireEvent{Source: "azure", Data: map[string]any{"type": "response.done", "response": map[string]any{"id": fmt.Sprintf("r%d", i), "status": "completed", "usage": map[string]any{"input_tokens": float64(10000), "output_tokens": float64(100)}}}}
		if i < 3 {
			item := fmt.Sprintf("in%d", i)
			events <- wireEvent{Source: "azure", Data: map[string]any{"type": "input_audio_buffer.committed", "item_id": item}}
			events <- wireEvent{Source: "azure", Data: map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": item, "transcript": "Just the password."}}
		}
	}
	if reason := <-done; reason != "token_budget_limit" {
		t.Fatal(reason)
	}
	if c.InputTokens != 40000 || c.OutputTokens != 400 || !c.UsageComplete {
		t.Fatalf("totals %d/%d complete=%v", c.InputTokens, c.OutputTokens, c.UsageComplete)
	}
}
func TestDisconnectUsageIncomplete(t *testing.T) {
	s, c := fixture()
	c.ModelStarted = true
	tw, _ := wsPair(t)
	az, peer := wsPair(t)
	events := make(chan wireEvent, 2)
	done := make(chan string, 1)
	go func() { done <- s.bridgeLoop(context.Background(), c, tw, az, "MZtest", events) }()
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "session.updated"}}
	receive(t, peer)
	events <- wireEvent{Source: "twilio", Err: true}
	<-done
	if c.UsageComplete {
		t.Fatal("unaccounted response called complete")
	}
}
func TestFinalEventBeforeHangup(t *testing.T) {
	s, c := fixture()
	c.ModelStarted = true
	c.InputTokens = 123
	c.OutputTokens = 45
	c.UsageComplete = true
	order := []string{}
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		order = append(order, r.URL.Host)
		if r.URL.Host == "recorder.example" {
			var ev map[string]any
			json.NewDecoder(r.Body).Decode(&ev)
			d := objectAt(ev, "data")
			if d["usageComplete"] != true || d["inputTokens"] != float64(123) || d["outputTokens"] != float64(45) {
				t.Fatal(d)
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	s.finish(c, "research_finished")
	if len(order) != 2 || order[0] != "recorder.example" || order[1] != "api.twilio.com" {
		t.Fatal(order)
	}
}
func TestProbeRequiresPCMUAndGeneratedAudio(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		for _, audio := range []bool{false, true} {
			t.Run(fmt.Sprint(cancelled, audio), func(t *testing.T) {
				c, peer := wsPair(t)
				c.SetReadDeadline(time.Now().Add(time.Second))
				done := make(chan error, 1)
				go func() { done <- probeSession(c, cancelled) }()
				cfg := receive(t, peer)
				sess := objectAt(cfg, "session")
				if sess["max_output_tokens"] != float64(512) {
					t.Fatal(sess)
				}
				peer.WriteJSON(map[string]any{"type": "session.updated", "session": sess})
				for range probeConversation(cancelled) {
					receive(t, peer)
				}
				receive(t, peer)
				if audio {
					peer.WriteJSON(map[string]any{"type": "response.output_audio.delta", "delta": strings.Repeat("AAAA", 100)})
					transcript := "A robot planted an orchard, and a ferry route opened."
					if cancelled {
						transcript = "A ferry route opened."
					}
					peer.WriteJSON(map[string]any{"type": "response.output_audio_transcript.done", "transcript": transcript})
				}
				status := "incomplete"
				if audio {
					status = "completed"
				}
				peer.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"status": status, "status_details": map[string]any{"reason": "max_output_tokens"}}})
				if e := <-done; (e == nil) != audio {
					t.Fatalf("audio=%v error=%v", audio, e)
				} else if !audio && !strings.Contains(e.Error(), "max_output_tokens") {
					t.Fatalf("missing safe diagnostic: %v", e)
				}
			})
		}
	}
}

func TestProbeRejectsSilenceIncompleteAndMissingTranscript(t *testing.T) {
	for _, tc := range []struct {
		name, payload, transcript, status string
		cancelled                         bool
	}{
		{"silence", strings.Repeat("////", 100), "orchard and ferry", "completed", false},
		{"incomplete", strings.Repeat("AAAA", 100), "orchard and ferry", "incomplete", false},
		{"no-transcript", strings.Repeat("AAAA", 100), "", "completed", false},
		{"lost-earlier-answer", strings.Repeat("AAAA", 100), "A ferry route opened.", "completed", false},
		{"ignored-cancellation", strings.Repeat("AAAA", 100), "An orchard was planted and a ferry route opened.", "completed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, peer := wsPair(t)
			c.SetReadDeadline(time.Now().Add(time.Second))
			done := make(chan error, 1)
			go func() { done <- probeSession(c, tc.cancelled) }()
			sess := objectAt(receive(t, peer), "session")
			peer.WriteJSON(map[string]any{"type": "session.updated", "session": sess})
			for range probeConversation(tc.cancelled) {
				receive(t, peer)
			}
			receive(t, peer)
			peer.WriteJSON(map[string]any{"type": "response.output_audio.delta", "delta": tc.payload})
			peer.WriteJSON(map[string]any{"type": "response.output_audio_transcript.done", "transcript": tc.transcript})
			peer.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"status": tc.status}})
			if err := <-done; err == nil {
				t.Fatal("invalid speech proof accepted")
			}
		})
	}
}

func TestOutputBudgetPreventsFurtherGeneration(t *testing.T) {
	s, c := fixture()
	c.ModelStarted = true
	c.OutputTokens = 24900
	tw, _ := wsPair(t)
	az, _ := wsPair(t)
	events := make(chan wireEvent, 1)
	events <- wireEvent{Source: "azure", Data: map[string]any{"type": "session.updated"}}
	if reason := s.bridgeLoop(context.Background(), c, tw, az, "MZtest", events); reason != "token_budget_limit" {
		t.Fatal(reason)
	}
	if c.OutputTokens != 24900 {
		t.Fatal("token count changed without request")
	}
}
func TestConfiguredGenerationBounds(t *testing.T) {
	sess := objectAt(sessionConfig(), "session")
	if sess["max_output_tokens"] != 512 {
		t.Fatal("unbounded output")
	}
	trunc := objectAt(sess, "truncation")
	if objectAt(trunc, "token_limits")["post_instructions"] != 6000 {
		t.Fatal("unbounded context")
	}
	turn := objectAt(objectAt(objectAt(sess, "audio"), "input"), "turn_detection")
	if turn["create_response"] != false {
		t.Fatal("automatic responses bypass budget")
	}
}
func TestProbeAPIFailure(t *testing.T) {
	c, peer := wsPair(t)
	c.SetReadDeadline(time.Now().Add(time.Second))
	done := make(chan error, 1)
	go func() { done <- probeSession(c, false) }()
	receive(t, peer)
	peer.WriteJSON(map[string]any{"type": "error", "error": map[string]any{"code": "invalid_value", "type": "invalid_request_error", "param": "session.audio"}})
	if e := <-done; e == nil || !strings.Contains(e.Error(), "invalid_value") {
		t.Fatalf("error %v", e)
	}
}

func TestNavigationChoices(t *testing.T) {
	for _, tc := range []struct {
		speech, kind, value string
		requested           bool
	}{
		{"For reservations press one. For the front desk press two.", "digits", "2", false},
		{"Press 1 for reservations, press 3 for customer service.", "digits", "3", false},
		{"For reception, dial zero.", "digits", "0", false},
		{"Press one for reservations press two for front desk", "digits", "2", false},
		{"For English press one. Para espanol press two.", "digits", "1", false},
		{"Please say the department you want.", "request", "", false},
		{"Please hold for the next available representative.", "wait", "", false},
		{"Connecting you to the front desk.", "wait", "", false},
		{"Hi, this is Amy at the front desk.", "person", "", false},
		{"Front desk", "person", "", true},
		{"How can I help you?", "request", "", false},
		{"How can I help you?", "person", "", true},
		{"Leave a message after the beep.", "voicemail", "", false},
		{"To cancel a booking press nine.", "unsupported", "", false},
		{"Enter your credit card number.", "request", "", false},
		{"", "wait", "", false},
	} {
		t.Run(tc.speech, func(t *testing.T) {
			got := navigationAction(tc.speech, tc.requested)
			if got.kind != tc.kind || got.value != tc.value {
				t.Fatalf("%+v", got)
			}
		})
	}
}
func TestNavigationMenuHoldHumanConsent(t *testing.T) {
	s, c := fixture()
	c.Navigating = true
	c.NavDeadline = time.Now().Add(navigationLimit)
	records := 0
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/Recordings.json") {
			records++
		}
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	initial := s.navigationGather(c, "")
	if strings.Contains(initial, "<Say>Hi!") || strings.Contains(initial, "<Connect") {
		t.Fatal(initial)
	}
	w := httptest.NewRecorder()
	s.consent(w, request(s, c, "/consent?run=trial&target=hotel", "yes"))
	if c.Consented || records != 0 {
		t.Fatal("navigation bypassed")
	}
	inputs := []string{"For reservations press 1. For front desk press 2.", "Please hold. Transferring you.", "Hello, this is Amy speaking."}
	for step, speech := range inputs {
		path := fmt.Sprintf("/navigate?run=trial&target=hotel&step=%d", step)
		w = httptest.NewRecorder()
		s.navigate(w, request(s, c, path, speech))
		body := w.Body.String()
		if step == 0 && (!strings.Contains(body, "<Play digits=\"ww2\"/>") || strings.Index(body, "<Play") > strings.Index(body, "<Gather")) {
			t.Fatal(body)
		}
		if step < 2 && strings.Contains(body, "/consent?") {
			t.Fatal("consent asked of IVR")
		}
		if step == 2 && !strings.Contains(body, "/consent?") {
			t.Fatal(body)
		}
		replay := httptest.NewRecorder()
		s.navigate(replay, request(s, c, path, speech))
		if replay.Body.String() != body || c.NavStep != step+1 {
			t.Fatal("callback retry changed state")
		}
	}
	if records != 0 || c.Consented || c.Navigating {
		t.Fatal("recorded before human consent")
	}
	w = httptest.NewRecorder()
	s.consent(w, request(s, c, "/consent?run=trial&target=hotel", "yes"))
	if records != 1 || !strings.Contains(w.Body.String(), "<Connect") {
		t.Fatal(w.Body.String())
	}
}
func TestNavigationLimitsAndUnsafeCallbacks(t *testing.T) {
	for _, mode := range []string{"expired", "turns", "selections", "voicemail", "unsupported", "unsigned", "out-of-order"} {
		t.Run(mode, func(t *testing.T) {
			s, c := fixture()
			c.Navigating = true
			c.NavDeadline = time.Now().Add(navigationLimit)
			speech := "Please hold"
			switch mode {
			case "expired":
				c.NavDeadline = time.Now().Add(-time.Second)
			case "turns":
				c.NavStep = navigationTurns
			case "selections":
				c.NavActions = navigationSelections
				speech = "For the front desk press 2."
			case "voicemail":
				speech = "Please leave a message after the tone"
			case "unsupported":
				speech = "Press 8 to make a payment"
			}
			step := c.NavStep
			if mode == "out-of-order" {
				step++
			}
			req := request(s, c, fmt.Sprintf("/navigate?run=trial&target=hotel&step=%d", step), speech)
			if mode == "unsigned" {
				req.Header.Del("X-Twilio-Signature")
			}
			w := httptest.NewRecorder()
			s.navigate(w, req)
			if c.Consented || strings.Contains(w.Body.String(), "<Connect") {
				t.Fatal("recording without consent")
			}
			if mode == "unsigned" || mode == "out-of-order" {
				if w.Code < 400 || c.Ended {
					t.Fatal("bad callback mutated call")
				}
			} else if !c.Ended {
				t.Fatal("navigation limit not enforced")
			}
		})
	}
}
func TestVoiceStartsWithNavigation(t *testing.T) {
	s, c := fixture()
	delete(s.calls, c.SID)
	w := httptest.NewRecorder()
	s.voice(w, request(s, c, "/voice?run=trial&target=hotel", ""))
	if !strings.Contains(w.Body.String(), "/navigate?") || strings.Contains(w.Body.String(), "/consent?") {
		t.Fatal(w.Body.String())
	}
	s.mu.Lock()
	active := s.calls[c.SID]
	active.Navigating = false
	s.mu.Unlock()
	s.finish(active, "test_finished")
}

const marriottTransferAnnouncement = "Honor Marriott wonderful. Hospitality always in order to best serve you we ask that you please select from the following options. To make a new reservation or modify an existing reservation, please press 1 for a copy of a guest room folio or accounting related. Questions, please press 2 to make a restaurant reservation at Finn and field. Please press 3 for all other requests and to speak with a member of our front office team. Please press zero, or remain on the line. Please stay on the line, while your call is transferred to the operator."
const hiltonVoiceAnnouncement = "Thank you for calling Hilton, Santa Barbara for better service. You can ask me questions about our hotel or services."

func TestNavigationActualHotelAnnouncements(t *testing.T) {
	for _, tc := range []struct{ name, speech, kind string }{
		{"marriott_transfer", marriottTransferAnnouncement, "wait"},
		{"hilton_voice_menu", hiltonVoiceAnnouncement, "request"},
		{"transfer_before_known_digit", "For the front desk press 2. Please stay on the line, while your call is transferred to the operator.", "wait"},
		{"transfer_in_progress", "For reception dial 0. Please remain on the line. Your call is being transferred.", "wait"},
		{"unresolved_front_office_menu", strings.Split(marriottTransferAnnouncement, " Please stay on the line,")[0], "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			choice := navigationAction(tc.speech, false)
			if choice.kind != tc.kind || choice.value != "" {
				t.Fatalf("unexpected navigation: %+v", choice)
			}
		})
	}
}
func TestNavigationTransferWaitDoesNotInterruptOrAskConsent(t *testing.T) {
	s, c := fixture()
	c.Navigating = true
	c.NavDeadline = time.Now().Add(navigationLimit)
	w := httptest.NewRecorder()
	s.navigate(w, request(s, c, "/navigate?run=trial&target=hotel&step=0", marriottTransferAnnouncement))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "<Gather") || strings.Contains(body, "<Play") || strings.Contains(body, "/consent?") || strings.Contains(body, "Front desk or customer service") {
		t.Fatalf("transfer was interrupted: %s", body)
	}
	if !c.Navigating || c.Ended || c.NavActions != 0 || c.NavStep != 1 {
		t.Fatal("transfer did not remain in bounded navigation")
	}
}

func TestMediaAcceptsConfiguredWSSSignatureGolden(t *testing.T) {
	// Independent Node crypto HMAC-SHA1 vectors for test-secret and the exact
	// configured TwiML wss://bridge.example/media/token URL (with/without slash).
	// These intentionally do not call the server's signature() helper.
	for _, sig := range []string{"e/UMP2ImipZHPP256fWE2dmkrkE=", "8TNCnR3B4B4ZpPV5hp0mIj5OfYw="} {
		t.Run(sig, func(t *testing.T) {
			s, c := fixture()
			c.Consented = true
			srv := httptest.NewServer(http.HandlerFunc(s.media))
			defer srv.Close()
			ws, resp, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/media/token", http.Header{"X-Twilio-Signature": []string{sig}})
			if e != nil {
				status := 0
				if resp != nil {
					status = resp.StatusCode
					resp.Body.Close()
				}
				t.Fatalf("configured WSS signature rejected: HTTP %d", status)
			}
			ws.Close()
		})
	}
}

func TestMediaSignaturesIgnoreForwardedOriginAndRejectTampering(t *testing.T) {
	s, _ := fixture()
	for _, tc := range []struct {
		path, sig string
		want      bool
	}{
		{"/media/token", "e/UMP2ImipZHPP256fWE2dmkrkE=", true},
		{"/media/token", "8TNCnR3B4B4ZpPV5hp0mIj5OfYw=", true},
		{"/media/token", "vln6/tV4NZQbklh2EYA3+qZRdAo=", true},
		{"/media/other", "e/UMP2ImipZHPP256fWE2dmkrkE=", false},
		{"/media/token?extra=1", "e/UMP2ImipZHPP256fWE2dmkrkE=", false},
		{"/media/token", "invalid", false},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Host = "untrusted.example"
		r.Header.Set("X-Forwarded-Host", "untrusted.example")
		r.Header.Set("X-Forwarded-Proto", "http")
		r.Header.Set("X-Twilio-Signature", tc.sig)
		if got := s.signedMedia(r); got != tc.want {
			t.Fatalf("%s valid=%v", tc.path, got)
		}
	}
}
func TestMediaEndedSettlesFailedHandshakeOnce(t *testing.T) {
	s, c := fixture()
	c.Consented = true
	types := []string{}
	reason := ""
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "recorder.example" {
			var ev map[string]any
			json.NewDecoder(r.Body).Decode(&ev)
			types = append(types, stringAt(ev, "type"))
			if ev["type"] == "call_ended" {
				reason = stringAt(objectAt(ev, "data"), "reason")
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		s.mediaEnded(w, request(s, c, "/media-ended?run=trial&target=hotel&stream=token", ""))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if !c.Ended || reason != "media_ended_before_model" || len(types) != 2 || types[0] != "error" || types[1] != "call_ended" {
		t.Fatalf("ended=%v reason=%s events=%v", c.Ended, reason, types)
	}
}
func TestMediaEndedRejectsUnboundCallbacks(t *testing.T) {
	for _, kind := range []string{"unsigned", "wrong_stream", "wrong_call", "unconsented"} {
		t.Run(kind, func(t *testing.T) {
			s, c := fixture()
			c.Consented = true
			path := "/media-ended?run=trial&target=hotel&stream=token"
			if kind == "wrong_stream" {
				path = "/media-ended?run=trial&target=hotel&stream=other"
			}
			if kind == "unconsented" {
				c.Consented = false
			}
			r := request(s, c, path, "")
			if kind == "unsigned" {
				r.Header.Set("X-Twilio-Signature", "invalid")
			}
			if kind == "wrong_call" {
				other := *c
				other.SID = "CA" + strings.Repeat("b", 32)
				r = request(s, &other, path, "")
			}
			w := httptest.NewRecorder()
			s.mediaEnded(w, r)
			if w.Code != 403 || c.Ended {
				t.Fatal("unbound callback changed call state")
			}
		})
	}
}
func TestMediaInvalidStartEmitsFailureImmediately(t *testing.T) {
	s, c := fixture()
	c.Consented = true
	done := make(chan string, 1)
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "recorder.example" {
			var ev map[string]any
			json.NewDecoder(r.Body).Decode(&ev)
			if ev["type"] == "call_ended" {
				done <- stringAt(objectAt(ev, "data"), "reason")
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	srv := httptest.NewServer(http.HandlerFunc(s.media))
	defer srv.Close()
	ws, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/media/token", http.Header{"X-Twilio-Signature": []string{"e/UMP2ImipZHPP256fWE2dmkrkE="}})
	if e != nil {
		t.Fatal(e)
	}
	defer ws.Close()
	ws.WriteJSON(map[string]any{"event": "connected"})
	ws.WriteJSON(map[string]any{"event": "start", "start": map[string]any{"accountSid": "wrong", "callSid": c.SID}})
	select {
	case reason := <-done:
		if reason != "stream_rejected" {
			t.Fatal(reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream failure remained pending")
	}
}

// Exercise actual handlers as well as text escaping: every pre-model prompt
// must select the same neural voice without changing consent or recording.
func TestTelephonePromptVoice(t *testing.T) {
	for _, scenario := range []string{"introduction", "clarification", "navigation", "consented", "declined", "goodbye", "escaped"} {
		t.Run(scenario, func(t *testing.T) {
			s, c := fixture()
			recordings := 0
			s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/Recordings.json") {
					recordings++
				}
				return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			w := httptest.NewRecorder()
			switch scenario {
			case "introduction":
				twiml(w, s.gather(c.Target, false))
			case "clarification":
				twiml(w, s.gather(c.Target, true))
			case "navigation":
				c.Navigating = true
				c.NavDeadline = time.Now().Add(time.Minute)
				s.navigate(w, request(s, c, "/navigate?run=trial&target=hotel&step=0", "Please say the department"))
			case "consented":
				s.consent(w, request(s, c, "/consent?run=trial&target=hotel", "yes"))
			case "declined":
				s.consent(w, request(s, c, "/consent?run=trial&target=hotel", "no"))
			case "goodbye":
				twiml(w, goodbye)
			case "escaped":
				twiml(w, sayPrompt("A & B <Record/> \"quote\""))
			}
			wantRecordings := 0
			if scenario == "consented" {
				wantRecordings = 1
			}
			if recordings != wantRecordings {
				t.Fatalf("recordings=%d, want %d", recordings, wantRecordings)
			}
			d := xml.NewDecoder(strings.NewReader(w.Body.String()))
			says := 0
			for {
				token, err := d.Token()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if start, ok := token.(xml.StartElement); ok {
					if start.Name.Local == "Record" {
						t.Fatal("unintended recording markup")
					}
					if start.Name.Local != "Say" {
						continue
					}
					says++
					attrs := map[string]string{}
					for _, a := range start.Attr {
						attrs[a.Name.Local] = a.Value
					}
					if attrs["voice"] != "Polly.Joanna-Neural" || attrs["language"] != "en-US" {
						t.Fatalf("prompt attributes: %v", attrs)
					}
					if scenario == "escaped" {
						var text string
						if err := d.DecodeElement(&text, &start); err != nil {
							t.Fatal(err)
						}
						if text != "A & B <Record/> \"quote\"" {
							t.Fatalf("escaped text: %q", text)
						}
					}
				}
			}
			if says == 0 {
				t.Fatal("missing spoken prompt")
			}
		})
	}
}
