package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
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
	return probeSession(ws)
}
func probeSession(ws *websocket.Conn) error {
	cfg := sessionConfig()
	sess := objectAt(cfg, "session")
	sess["max_output_tokens"] = 32
	sess["instructions"] = "Say hello briefly."
	sess["tools"] = []any{}
	sess["tool_choice"] = "none"
	if sendWS(ws, cfg) != nil {
		return errors.New("Azure probe configuration write failed")
	}
	configured, heard := false, false
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
				if sendWS(ws, map[string]any{"type": "response.create", "response": map[string]any{"max_output_tokens": 32, "instructions": "Say hello."}}) != nil {
					return errors.New("Azure probe response write failed")
				}
			}
		case "response.output_audio.delta":
			raw, e := base64.StdEncoding.DecodeString(stringAt(m, "delta"))
			if e != nil {
				return errors.New("Azure probe invalid audio")
			}
			heard = heard || len(raw) > 0
		case "response.done":
			if configured && heard {
				return nil
			}
			return errors.New("Azure probe returned no audio")
		case "error":
			err := objectAt(m, "error")
			return fmt.Errorf("Azure probe API error code=%s type=%s param=%s", safeCode(stringAt(err, "code")), safeCode(stringAt(err, "type")), safeCode(stringAt(err, "param")))
		}
	}
}
