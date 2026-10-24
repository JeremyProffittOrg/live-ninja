package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/conversation"
	"github.com/gorilla/websocket"
)

func azureURL(c config) string {
	return strings.Replace(c.Azure, "https://", "wss://", 1) + "/openai/v1/realtime?model=" + url.QueryEscape(c.Deployment)
}
func safeCode(v string) string {
	if len(v) > 80 || !regexp.MustCompile("^[a-zA-Z0-9_.-]*$").MatchString(v) {
		return "redacted"
	}
	return v
}
func probeAzure(ctx context.Context, c config) error {
	for _, cancelled := range []bool{false, true} {
		if err := probeAzureConversation(ctx, c, cancelled); err != nil {
			return err
		}
	}
	return nil
}

func probeAzureConversation(ctx context.Context, c config, cancelled bool) error {
	dial := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, resp, e := dial.DialContext(ctx, azureURL(c), http.Header{"Api-Key": []string{c.AzureKey}})
	if e != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			if resp.Body != nil {
				resp.Body.Close()
			}
		}
		return fmt.Errorf("Azure probe handshake failed (HTTP %d)", status)
	}
	defer ws.Close()
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	ws.SetReadDeadline(deadline)
	ws.SetReadLimit(2 << 20)
	return probeSession(ws, cancelled)
}

// Synthetic lookup history reproduces a completed AI lookup interrupted by an
// additional world-news request. It never calls tools or uses owner content.
func probeConversation(cancelled bool) []map[string]any {
	message := func(role, text string) map[string]any {
		kind := "input_text"
		if role == "assistant" {
			kind = "output_text"
		}
		return map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": kind, "text": text}}}
	}
	request := "And grab me world news in the last 24 hours as well."
	if cancelled {
		request = "Cancel the AI news request. Only give me world news in the last 24 hours."
	}
	return []map[string]any{
		message("user", "Can you look for AI news in the last 24 hours?"),
		message("assistant", "Sure, let me check for the latest AI news from the past day."),
		{"type": "function_call", "name": "web_research", "call_id": "probe_ai", "arguments": `{"query":"AI news in the last 24 hours"}`},
		{"type": "function_call_output", "call_id": "probe_ai", "output": `{"summary":"A robot planted an orchard. This is a synthetic AI-news test result, not real news."}`},
		message("user", request),
		message("assistant", "Checking recent world news."),
		{"type": "function_call", "name": "web_research", "call_id": "probe_world", "arguments": `{"query":"world news in the last 24 hours"}`},
		{"type": "function_call_output", "call_id": "probe_world", "output": `{"summary":"A ferry route opened in Bristol. This is a synthetic world-news test result, not real news."}`},
	}
}

func probeSession(ws *websocket.Conn, cancelled bool) error {
	cfg := sessionConfig()
	sess := objectAt(cfg, "session")
	// Match the real call budget: the reasoning model can consume a 32-token
	// probe allowance before it reaches audio generation.
	sess["max_output_tokens"] = 512
	sess["instructions"] = "You are Live Ninja. Speak briefly in English. Use the supplied lookup results. " + conversation.RequestContinuityInstructions
	sess["tools"] = []any{}
	sess["tool_choice"] = "none"
	if sendWS(ws, cfg) != nil {
		return errors.New("Azure probe configuration write failed")
	}
	configured, nonSilent := false, false
	samples := 0
	transcript := ""
	for {
		var m map[string]any
		if ws.ReadJSON(&m) != nil {
			return errors.New("Azure probe timed out or disconnected")
		}
		switch stringAt(m, "type") {
		case "session.updated":
			audio := objectAt(objectAt(m, "session"), "audio")
			if stringAt(objectAt(objectAt(audio, "input"), "format"), "type") != "audio/pcmu" || stringAt(objectAt(objectAt(audio, "output"), "format"), "type") != "audio/pcmu" {
				return errors.New("Azure probe did not confirm PCMU input and output")
			}
			if !configured {
				configured = true
				for _, item := range probeConversation(cancelled) {
					if sendWS(ws, map[string]any{"type": "conversation.item.create", "item": item}) != nil {
						return errors.New("Azure probe history write failed")
					}
				}
				if sendWS(ws, map[string]any{"type": "response.create", "response": map[string]any{"max_output_tokens": 512}}) != nil {
					return errors.New("Azure probe response write failed")
				}
			}
		case "response.output_audio.delta":
			raw, e := base64.StdEncoding.DecodeString(stringAt(m, "delta"))
			if e != nil {
				return errors.New("Azure probe invalid audio")
			}
			samples += len(raw)
			for _, sample := range raw {
				// PCMU has positive and negative zero encodings.
				nonSilent = nonSilent || (sample != 0xff && sample != 0x7f)
			}
		case "response.output_audio_transcript.done":
			transcript = strings.TrimSpace(stringAt(m, "transcript"))
		case "response.done":
			response := objectAt(m, "response")
			if configured && nonSilent && samples >= 160 && transcript != "" && stringAt(response, "status") == "completed" {
				lower := strings.ToLower(transcript)
				if !strings.Contains(lower, "ferry") || strings.Contains(lower, "orchard") == cancelled {
					return fmt.Errorf("Azure request replay failed (cancelled=%t): %s", cancelled, transcript)
				}
				fmt.Printf("Azure request replay passed (cancelled=%t): %s\n", cancelled, transcript)
				return nil
			}
			return fmt.Errorf("Azure probe did not complete audible speech (status=%s reason=%s)", safeCode(stringAt(response, "status")), safeCode(stringAt(objectAt(response, "status_details"), "reason")))
		case "error":
			err := objectAt(m, "error")
			return fmt.Errorf("Azure probe API error code=%s type=%s param=%s", safeCode(stringAt(err, "code")), safeCode(stringAt(err, "type")), safeCode(stringAt(err, "param")))
		}
	}
}
