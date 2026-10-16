// Transcribe a bounded saved recording channel without generating model replies.
package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/gorilla/websocket"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const maxAudioBytes = 180 * 8000

type result struct {
	Text       string `json:"text"`
	Status     string `json:"status"`
	ItemID     string `json:"itemId"`
	AudioBytes int    `json:"audioBytes"`
}

func configEvent() map[string]any {
	return map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime", "output_modalities": []string{"text"}, "max_output_tokens": 1,
		"tools": []any{}, "tool_choice": "none",
		"audio": map[string]any{"input": map[string]any{
			"format":         map[string]any{"type": "audio/pcmu"},
			"transcription":  map[string]any{"model": "whisper-1", "language": "en"},
			"turn_detection": nil,
		}},
	}}
}
func object(m map[string]any, k string) map[string]any { v, _ := m[k].(map[string]any); return v }
func text(m map[string]any, k string) string           { v, _ := m[k].(string); return v }
func safeCode(v string) string {
	if len(v) > 80 || !regexp.MustCompile("^[a-zA-Z0-9_.-]*$").MatchString(v) {
		return "redacted"
	}
	return v
}
func transcribe(ws *websocket.Conn, audio []byte) (result, error) {
	if len(audio) < 800 || len(audio) > maxAudioBytes {
		return result{}, errors.New("audio duration outside 0.1..180 seconds")
	}
	ws.SetReadLimit(2 << 20)
	ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	write := func(v any) error { ws.SetWriteDeadline(time.Now().Add(10 * time.Second)); return ws.WriteJSON(v) }
	if write(configEvent()) != nil {
		return result{}, errors.New("session configuration failed")
	}
	sent := false
	itemID := ""
	pending := map[string]map[string]any{}
	for n := 0; n < 1000; n++ {
		var m map[string]any
		if ws.ReadJSON(&m) != nil {
			return result{}, errors.New("transcription disconnected or timed out")
		}
		kind := text(m, "type")
		if kind == "error" || kind == "conversation.item.input_audio_transcription.failed" {
			detail := object(m, "error")
			return result{}, fmt.Errorf("Azure transcription rejected code=%s type=%s param=%s", safeCode(text(detail, "code")), safeCode(text(detail, "type")), safeCode(text(detail, "param")))
		}
		if strings.HasPrefix(kind, "response.") {
			return result{}, errors.New("unsolicited model response rejected")
		}
		switch kind {
		case "session.updated":
			if sent {
				continue
			}
			input := object(object(object(m, "session"), "audio"), "input")
			if text(object(input, "format"), "type") != "audio/pcmu" || text(object(input, "transcription"), "model") != "whisper-1" {
				return result{}, errors.New("transcription format not confirmed")
			}
			vad, present := input["turn_detection"]
			if !present || vad != nil {
				return result{}, errors.New("manual turn detection not confirmed")
			}
			for offset := 0; offset < len(audio); offset += 8000 {
				end := offset + 8000
				if end > len(audio) {
					end = len(audio)
				}
				if write(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(audio[offset:end])}) != nil {
					return result{}, errors.New("audio append failed")
				}
			}
			if write(map[string]any{"type": "input_audio_buffer.commit"}) != nil {
				return result{}, errors.New("audio commit failed")
			}
			sent = true
		case "input_audio_buffer.committed":
			if !sent || text(m, "item_id") == "" {
				return result{}, errors.New("unexpected audio commit")
			}
			itemID = text(m, "item_id")
		case "conversation.item.input_audio_transcription.completed":
			if !sent || text(m, "item_id") == "" {
				return result{}, errors.New("unexpected transcript")
			}
			pending[text(m, "item_id")] = m
		}
		if itemID != "" && pending[itemID] != nil {
			transcript := strings.TrimSpace(text(pending[itemID], "transcript"))
			status := "completed"
			if transcript == "" {
				status = "no_speech"
			}
			return result{transcript, status, itemID, len(audio)}, nil
		}
	}
	return result{}, errors.New("event ceiling reached")
}
func pcm16ToULaw(pcm []byte) ([]byte, error) {
	if len(pcm)%2 != 0 || len(pcm) < 1600 || len(pcm) > maxAudioBytes*2 {
		return nil, errors.New("PCM16 duration or sample alignment rejected")
	}
	out := make([]byte, len(pcm)/2)
	for i := range out {
		value := int(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		sign := 0
		if value < 0 {
			sign = 0x80
			value = -value
		}
		if value > 32635 {
			value = 32635
		}
		value += 0x84
		exponent := 7
		for mask := 0x4000; exponent > 0 && value&mask == 0; mask >>= 1 {
			exponent--
		}
		mantissa := (value >> (exponent + 3)) & 15
		out[i] = byte(^(sign | (exponent << 4) | mantissa))
	}
	return out, nil
}

func run() error {
	input := flag.String("input", "", "mono 8kHz raw PCMU path")
	output := flag.String("output", "", "private JSON result path")
	pcm16 := flag.Bool("pcm16", false, "input is mono 8kHz signed PCM16 little endian")
	flag.Parse()
	if *input == "" || *output == "" || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("EXECUTE_TRANSCRIPTION") != "hotel-vpn-ten-2026-10-16" {
		return errors.New("dated GitHub transcription authorization required")
	}
	info, e := os.Stat(*input)
	limit := int64(maxAudioBytes)
	if *pcm16 {
		limit *= 2
	}
	if e != nil || info.Size() < 800 || info.Size() > limit {
		return errors.New("input size outside bound")
	}
	audio, e := os.ReadFile(*input)
	if e != nil {
		return errors.New("input read failed")
	}
	if *pcm16 {
		audio, e = pcm16ToULaw(audio)
		if e != nil {
			return e
		}
	}
	endpoint := strings.TrimRight(os.Getenv("AZURE_OPENAI_ENDPOINT"), "/")
	deployment := os.Getenv("AZURE_OPENAI_DEPLOYMENT")
	key := os.Getenv("AZURE_OPENAI_API_KEY")
	if endpoint != "https://ln-aoai-eastus2.openai.azure.com" || deployment != "gpt-realtime-2-1-mini" || key == "" {
		return errors.New("expected Azure deployment configuration required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	dial := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, resp, e := dial.DialContext(ctx, strings.Replace(endpoint, "https://", "wss://", 1)+"/openai/v1/realtime?model="+url.QueryEscape(deployment), http.Header{"Api-Key": []string{key}})
	if e != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return errors.New("Azure transcription connection failed")
	}
	defer ws.Close()
	r, e := transcribe(ws, audio)
	if e != nil {
		return e
	}
	b, e := json.Marshal(r)
	if e != nil {
		return errors.New("result encoding failed")
	}
	if os.WriteFile(*output, b, 0600) != nil {
		return errors.New("private result write failed")
	}
	fmt.Printf("Transcription status=%s audioBytes=%d characters=%d\n", r.Status, r.AudioBytes, len(r.Text))
	return nil
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
