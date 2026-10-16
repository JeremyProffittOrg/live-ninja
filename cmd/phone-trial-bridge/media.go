package main

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"github.com/gorilla/websocket"
	"log"
	"net/http"
	"strings"
	"time"
)

func sessionConfig() map[string]any {
	return map[string]any{
		"type": "session.update", "session": map[string]any{
			"type": "realtime", "output_modalities": []string{"audio"}, "max_output_tokens": 512,
			"truncation":   map[string]any{"type": "retention_ratio", "retention_ratio": 0.8, "token_limits": map[string]any{"post_instructions": 6000}},
			// Every response.create carries per-turn instructions from dialog.go; these
			// session instructions hold only the standing rules, never the checklist.
			"instructions": dialogRules,
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "audio/pcmu"}, "transcription": map[string]any{"model": "whisper-1"}, "turn_detection": map[string]any{"type": "server_vad", "threshold": 0.5, "prefix_padding_ms": 300, "silence_duration_ms": 650, "create_response": false, "interrupt_response": true}},
				"output": map[string]any{"format": map[string]any{"type": "audio/pcmu"}, "voice": "marin"}},
			"tools":       []any{map[string]any{"type": "function", "name": "end_call", "description": "End the call once research is complete, consent is withdrawn, or the person cannot help. Include only staff-reported findings, never verified VPN compatibility.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"reason": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string"}}, "required": []string{"reason", "summary"}, "additionalProperties": false}}},
			"tool_choice": "auto",
		}}
}

type wireEvent struct {
	Source string
	Data   map[string]any
	Err    bool
}

func stringAt(m map[string]any, k string) string         { v, _ := m[k].(string); return v }
func objectAt(m map[string]any, k string) map[string]any { v, _ := m[k].(map[string]any); return v }
func sendWS(c *websocket.Conn, v any) error {
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.WriteJSON(v)
}
func readWS(ctx context.Context, c *websocket.Conn, source string, ch chan<- wireEvent) {
	for {
		var m map[string]any
		e := c.ReadJSON(&m)
		select {
		case ch <- wireEvent{source, m, e != nil}:
		case <-ctx.Done():
			return
		}
		if e != nil {
			return
		}
	}
}

// Validate the URL configured in <Stream>, not the HTTP URL reconstructed by
// Azure's TLS-terminating proxy. Both forms use the trusted configured origin;
// neither Host nor Forwarded headers can choose what gets authenticated.
func (s *server) signedMedia(r *http.Request) bool {
	supplied := r.Header.Get("X-Twilio-Signature")
	if supplied == "" {
		return false
	}
	uri := strings.Replace(s.cfg.Base, "https://", "wss://", 1) + r.URL.RequestURI()
	if hmac.Equal([]byte(signature(s.cfg.Auth, uri, nil)), []byte(supplied)) || hmac.Equal([]byte(signature(s.cfg.Auth, uri+"/", nil)), []byte(supplied)) {
		return true
	}
	return s.signed(r, true)
}
func (s *server) mediaFailure(c *call, code string) {
	log.Printf("media failure: %s", code)
	s.emit(c, "error", map[string]any{"code": code})
	s.finish(c, code)
}
func (s *server) media(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" || !s.signedMedia(r) {
		log.Print("media entry rejected: method_or_signature")
		http.Error(w, "forbidden", 403)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/media/")
	s.mu.Lock()
	var c *call
	for _, v := range s.calls {
		if v.Token == token && v.Consented && !v.Connected && !v.Ended && time.Now().Before(v.Deadline) {
			c = v
			v.Connected = true
			break
		}
	}
	s.mu.Unlock()
	if c == nil {
		log.Print("media entry rejected: call_state")
		http.Error(w, "forbidden", 403)
		return
	}
	up := websocket.Upgrader{HandshakeTimeout: 5 * time.Second, CheckOrigin: func(*http.Request) bool { return true }}
	tw, e := up.Upgrade(w, r, nil)
	if e != nil {
		s.mediaFailure(c, "upgrade_failed")
		return
	}
	s.mu.Lock()
	c.Close = func() { tw.Close() }
	s.mu.Unlock()
	defer tw.Close()
	defer s.finish(c, "stream_closed")
	tw.SetReadLimit(65536)
	tw.SetReadDeadline(time.Now().Add(10 * time.Second))
	var first map[string]any
	// Twilio sends connected, then start. Bind the actual stream to the signed call.
	if tw.ReadJSON(&first) != nil {
		s.mediaFailure(c, "media_start_missing")
		return
	}
	if stringAt(first, "event") == "connected" {
		if tw.ReadJSON(&first) != nil {
			s.mediaFailure(c, "media_start_missing")
			return
		}
	}
	start := objectAt(first, "start")
	format := objectAt(start, "mediaFormat")
	if stringAt(first, "event") != "start" || stringAt(start, "accountSid") != s.cfg.Account || stringAt(start, "callSid") != c.SID || stringAt(format, "encoding") != "audio/x-mulaw" || format["sampleRate"] != float64(8000) || format["channels"] != float64(1) {
		s.mediaFailure(c, "stream_rejected")
		return
	}
	stream := stringAt(start, "streamSid")
	if stream == "" {
		s.mediaFailure(c, "stream_id_missing")
		return
	}
	if !s.emit(c, "summary", map[string]any{"phase": "media", "action": "stream_authenticated"}) {
		s.finish(c, "event_delivery_failed")
		return
	}
	tw.SetReadDeadline(c.Deadline)
	ctx, cancel := context.WithDeadline(context.Background(), c.Deadline)
	defer cancel()
	uri := azureURL(s.cfg)
	dial := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	az, resp, e := dial.DialContext(ctx, uri, http.Header{"Api-Key": []string{s.cfg.AzureKey}})
	if e != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		s.emit(c, "error", map[string]any{"code": "azure_connect_failed"})
		s.finish(c, "azure_connect_failed")
		return
	}
	defer az.Close()
	s.mu.Lock()
	c.ModelStarted = true
	c.Close = func() { az.Close(); tw.Close() }
	s.mu.Unlock()
	az.SetReadLimit(2 << 20)
	az.SetReadDeadline(c.Deadline)
	if sendWS(az, sessionConfig()) != nil {
		s.finish(c, "azure_config_failed")
		return
	}
	events := make(chan wireEvent, 64)
	go readWS(ctx, tw, "twilio", events)
	go readWS(ctx, az, "azure", events)
	reason := s.bridgeLoop(ctx, c, tw, az, stream, events)
	s.finish(c, reason)
}
func (s *server) bridgeLoop(ctx context.Context, c *call, tw, az *websocket.Conn, stream string, events <-chan wireEvent) (result string) {
	pending := 0
	invalid := false
	markHeard := false
	d := newDialog(c.Target.Name)
	silenceWait, transcriptLimit := s.dialogTimers()
	// A staff turn is answered only after its transcript arrives (or transcriptWait
	// passes), so each reply carries the answer and the next single step.
	queued, hasQueued := "", false
	awaiting := map[string]bool{}
	var heardText []string
	untranscribed := false
	var transcriptDue, silence <-chan time.Time
	var playEnd time.Time
	seen := map[string]bool{}
	defer func() {
		s.mu.Lock()
		c.UsageComplete = (result == "research_finished" || result == "token_budget_limit") && pending == 0 && !invalid
		s.mu.Unlock()
	}()
	requestResponse := func(instructions string) string {
		s.mu.Lock()
		allowed := c.InputTokens+12000 <= 50000 && c.OutputTokens+512 <= 25000
		s.mu.Unlock()
		if !allowed {
			return "token_budget_limit"
		}
		response := map[string]any{"max_output_tokens": 512}
		if instructions != "" {
			response["instructions"] = instructions
		}
		pending++
		if sendWS(az, map[string]any{"type": "response.create", "response": response}) != nil {
			return "azure_write_failed"
		}
		return ""
	}
	respond := func(instructions string, ok bool) string {
		if !ok {
			return ""
		}
		if pending > 0 {
			queued, hasQueued = instructions, true
			return ""
		}
		return requestResponse(instructions)
	}
	// settle answers a staff turn once every committed segment is transcribed
	// or the transcript wait expired.
	settle := func() string {
		text := strings.Join(heardText, " ")
		transcribed := !untranscribed || text != ""
		heardText, untranscribed, transcriptDue = nil, false, nil
		for k := range awaiting {
			delete(awaiting, k)
		}
		return respond(d.reply(text, transcribed))
	}
	idle := time.NewTimer(40 * time.Second)
	defer idle.Stop()
	touch := func() {
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(40 * time.Second)
	}
	var item string
	var playbackStart time.Time
	var audioMS int64
	configured := false
	ending := false
	endReason := "complete"
	endMark := ""
	var drain <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return "duration_limit"
		case <-idle.C:
			return "conversation_idle"
		case <-drain:
			return endReason
		case <-transcriptDue:
			untranscribed = true
			if reason := settle(); reason != "" {
				return reason
			}
		case <-silence:
			silence = nil
			if !ending && pending == 0 && len(awaiting) == 0 {
				if reason := respond(d.silence()); reason != "" {
					return reason
				}
			}
		case ev := <-events:
			if ev.Err {
				return ev.Source + "_disconnected"
			}
			m := ev.Data
			if ev.Source == "twilio" {
				switch stringAt(m, "event") {
				case "stop":
					return "remote_hangup"
				case "mark":
					if ending && stringAt(objectAt(m, "mark"), "name") == endMark {
						markHeard = true
						if pending == 0 {
							return endReason
						}
					}
				case "media":
					if configured && !ending && stringAt(objectAt(m, "media"), "track") == "inbound" {
						payload := stringAt(objectAt(m, "media"), "payload")
						if payload != "" {
							if sendWS(az, map[string]any{"type": "input_audio_buffer.append", "audio": payload}) != nil {
								return "azure_write_failed"
							}
						}
					}
				}
				continue
			}
			switch stringAt(m, "type") {
			case "session.updated":
				if !configured {
					configured = true
					if reason := requestResponse(d.opening()); reason != "" {
						return reason
					}
				}
			case "input_audio_buffer.committed":
				if !ending && !d.closing {
					silence = nil
					awaiting[stringAt(m, "item_id")] = true
					transcriptDue = time.After(transcriptLimit)
				}
			case "response.output_audio.delta":
				touch()
				if ending {
					continue
				}
				payload := stringAt(m, "delta")
				raw, e := base64.StdEncoding.DecodeString(payload)
				if e != nil {
					return "invalid_audio"
				}
				nextItem := stringAt(m, "item_id")
				if nextItem != item {
					item = nextItem
					playbackStart = time.Now()
					audioMS = 0
				}
				audioMS += int64(len(raw)) / 8
				if now := time.Now(); playEnd.Before(now) {
					playEnd = now
				}
				playEnd = playEnd.Add(time.Duration(len(raw)/8) * time.Millisecond)
				if sendWS(tw, map[string]any{"event": "media", "streamSid": stream, "media": map[string]any{"payload": payload}}) != nil {
					return "twilio_write_failed"
				}
			case "input_audio_buffer.speech_started":
				touch()
				if ending {
					continue
				}
				silence = nil
				playEnd = time.Now()
				if sendWS(tw, map[string]any{"event": "clear", "streamSid": stream}) != nil {
					return "twilio_write_failed"
				}
				// Server VAD cancels the active response. Remove only audio not yet heard
				// using wall-clock playback duration capped to the actual PCMU samples sent.
				if item != "" {
					heard := time.Since(playbackStart).Milliseconds()
					if heard > audioMS {
						heard = audioMS
					}
					if heard < 0 {
						heard = 0
					}
					if sendWS(az, map[string]any{"type": "conversation.item.truncate", "item_id": item, "content_index": 0, "audio_end_ms": heard}) != nil {
						return "azure_write_failed"
					}
					item = ""
					audioMS = 0
				}
			case "conversation.item.input_audio_transcription.completed":
				touch()
				if !s.emit(c, "transcript", map[string]any{"speaker": "hotel", "text": stringAt(m, "transcript"), "itemId": stringAt(m, "item_id")}) {
					return "event_delivery_failed"
				}
				if id := stringAt(m, "item_id"); awaiting[id] {
					delete(awaiting, id)
					if text := strings.TrimSpace(stringAt(m, "transcript")); text != "" {
						heardText = append(heardText, text)
					}
					if len(awaiting) == 0 {
						if reason := settle(); reason != "" {
							return reason
						}
					}
				}
			case "conversation.item.input_audio_transcription.failed":
				if id := stringAt(m, "item_id"); awaiting[id] {
					delete(awaiting, id)
					untranscribed = true
					if len(awaiting) == 0 {
						if reason := settle(); reason != "" {
							return reason
						}
					}
				}
			case "response.output_audio_transcript.done":
				if !s.emit(c, "transcript", map[string]any{"speaker": "assistant", "text": stringAt(m, "transcript"), "itemId": stringAt(m, "item_id")}) {
					return "event_delivery_failed"
				}
			case "response.done":
				response := objectAt(m, "response")
				id := stringAt(response, "id")
				if id == "" {
					invalid = true
					return "missing_response_id"
				}
				if seen[id] {
					continue
				}
				seen[id] = true
				if pending == 0 {
					invalid = true
					return "unsolicited_response"
				}
				pending--
				usage := objectAt(response, "usage")
				input, okIn := usage["input_tokens"].(float64)
				output, okOut := usage["output_tokens"].(float64)
				if !okIn || !okOut || input < 0 || output < 0 || input != float64(int64(input)) || output != float64(int64(output)) {
					invalid = true
					return "missing_usage"
				}
				if stringAt(response, "status") != "completed" {
					invalid = true
				}
				s.mu.Lock()
				c.InputTokens += int64(input)
				c.OutputTokens += int64(output)
				inputTotal, outputTotal := c.InputTokens, c.OutputTokens
				s.mu.Unlock()
				if !s.emit(c, "usage", map[string]any{"responseId": id, "status": stringAt(response, "status"), "usage": usage, "inputTokens": inputTotal, "outputTokens": outputTotal}) {
					return "event_delivery_failed"
				}
				if input > 12000 || output > 512 {
					invalid = true
					return "response_budget_exceeded"
				}
				if inputTotal+12000 > 50000 || outputTotal+512 > 25000 {
					return "token_budget_limit"
				}
				if ending && markHeard {
					return endReason
				}
				if hasQueued && !ending {
					hasQueued = false
					if reason := requestResponse(queued); reason != "" {
						return reason
					}
				} else if pending == 0 && !ending && !d.closing && len(awaiting) == 0 {
					// No reply after the question finishes playing is a non-answer.
					wait := time.Until(playEnd)
					if wait < 0 {
						wait = 0
					}
					silence = time.After(wait + silenceWait)
				}
			case "response.function_call_arguments.done":
				if stringAt(m, "name") != "end_call" {
					return "unexpected_tool"
				}
				var args struct{ Reason, Summary string }
				if json.Unmarshal([]byte(stringAt(m, "arguments")), &args) != nil {
					return "invalid_tool"
				}
				if !s.emit(c, "summary", map[string]any{"reason": args.Reason, "summary": args.Summary, "evidence": "staff_report_only"}) {
					return "event_delivery_failed"
				}
				if args.Reason == "consent_withdrawn" {
					return "consent_withdrawn"
				}
				ending = true
				endReason = "research_finished"
				endMark = "final-audio"
				if sendWS(tw, map[string]any{"event": "mark", "streamSid": stream, "mark": map[string]any{"name": endMark}}) != nil {
					return "twilio_write_failed"
				}
				drain = time.After(15 * time.Second)
			case "error":
				err := objectAt(m, "error")
				if !s.emit(c, "error", map[string]any{"code": stringAt(err, "code"), "type": stringAt(err, "type")}) {
					return "event_delivery_failed"
				}
				return "azure_error"
			}
		}
	}
}

// mediaEnded is the signed Connect action callback. It settles a call even when
// the WebSocket never upgrades, instead of waiting for the outer call watchdog.
func (s *server) mediaEnded(w http.ResponseWriter, r *http.Request) {
	t, ok := s.authorize(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	c := s.calls[r.PostForm.Get("CallSid")]
	if c == nil || c.Target.ID != t.ID || !c.Consented || !hmac.Equal([]byte(c.Token), []byte(r.URL.Query().Get("stream"))) {
		s.mu.Unlock()
		http.Error(w, "forbidden", 403)
		return
	}
	ended, started := c.Ended, c.ModelStarted
	s.mu.Unlock()
	if !ended {
		if !started {
			s.mediaFailure(c, "media_ended_before_model")
		} else {
			s.finish(c, "media_ended")
		}
	}
	twiml(w, goodbye)
}
