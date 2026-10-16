package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fixture(t *testing.T, handler func(*websocket.Conn)) *websocket.Conn {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer c.Close()
		handler(c)
	}))
	t.Cleanup(s.Close)
	c, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http"), nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func configured(c *websocket.Conn) bool {
	var m map[string]any
	if c.ReadJSON(&m) != nil {
		return false
	}
	sess := object(m, "session")
	input := object(object(sess, "audio"), "input")
	if text(m, "type") != "session.update" || input["turn_detection"] != nil {
		return false
	}
	return c.WriteJSON(map[string]any{"type": "session.updated", "session": sess}) == nil
}
func TestManualCommitTranscribesOnlyMatchingItem(t *testing.T) {
	audio := bytes.Repeat([]byte{0xff}, 16000)
	c := fixture(t, func(c *websocket.Conn) {
		if !configured(c) {
			return
		}
		var received []byte
		for i := 0; i < 2; i++ {
			var m map[string]any
			if c.ReadJSON(&m) != nil {
				return
			}
			if text(m, "type") != "input_audio_buffer.append" {
				return
			}
			b, _ := base64.StdEncoding.DecodeString(text(m, "audio"))
			received = append(received, b...)
		}
		var m map[string]any
		c.ReadJSON(&m)
		if text(m, "type") != "input_audio_buffer.commit" || !bytes.Equal(received, audio) {
			return
		}
		c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": "wrong", "transcript": "wrong"})
		c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": "expected", "transcript": "synthetic test phrase"})
		c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "expected"})
	})
	r, e := transcribe(c, audio)
	if e != nil {
		t.Fatal(e)
	}
	if r.Text != "synthetic test phrase" || r.Status != "completed" || r.AudioBytes != len(audio) {
		t.Fatalf("incorrect result metadata")
	}
}
func TestRejectsAudioOutsideBoundBeforeNetwork(t *testing.T) {
	for _, b := range [][]byte{nil, make([]byte, 799), make([]byte, maxAudioBytes+1)} {
		if _, e := transcribe(nil, b); e == nil {
			t.Fatal("expected bound rejection")
		}
	}
}
func TestRejectsAutomaticGeneration(t *testing.T) {
	c := fixture(t, func(c *websocket.Conn) {
		var m map[string]any
		c.ReadJSON(&m)
		c.WriteJSON(map[string]any{"type": "response.created"})
	})
	if _, e := transcribe(c, make([]byte, 800)); e == nil {
		t.Fatal("unexpected generation accepted")
	}
}
func TestRejectsUnconfirmedManualSession(t *testing.T) {
	c := fixture(t, func(c *websocket.Conn) {
		var m map[string]any
		c.ReadJSON(&m)
		sess := object(m, "session")
		object(object(sess, "audio"), "input")["turn_detection"] = map[string]any{"type": "server_vad"}
		c.WriteJSON(map[string]any{"type": "session.updated", "session": sess})
	})
	if _, e := transcribe(c, make([]byte, 800)); e == nil {
		t.Fatal("automatic VAD accepted")
	}
}
func TestEmptyTranscriptIsNotInventedSpeech(t *testing.T) {
	c := fixture(t, func(c *websocket.Conn) {
		if !configured(c) {
			return
		}
		var m map[string]any
		c.ReadJSON(&m)
		c.ReadJSON(&m)
		c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "empty"})
		c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": "empty", "transcript": ""})
	})
	r, e := transcribe(c, make([]byte, 800))
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != "no_speech" || r.Text != "" {
		t.Fatal("empty result not labeled")
	}
}

func TestPCM16ConversionAndBounds(t *testing.T) {
	pcm := make([]byte, 1600)
	samples := []int16{0, 1, -1, 32124, -32124, 32767, -32768}
	expected := []byte{255, 255, 127, 128, 0, 128, 0}
	for i, v := range samples {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
	}
	ulaw, e := pcm16ToULaw(pcm)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(ulaw[:len(expected)], expected) {
		t.Fatalf("unexpected companding: %v", ulaw[:len(expected)])
	}
	for _, invalid := range [][]byte{nil, make([]byte, 1599), make([]byte, maxAudioBytes*2+2)} {
		if _, e := pcm16ToULaw(invalid); e == nil {
			t.Fatal("invalid PCM16 accepted")
		}
	}
}

func TestSafeDiagnosticOmitsProviderText(t *testing.T) {
	if safeCode("https://secret.example/token") != "redacted" || safeCode("invalid_value") != "invalid_value" {
		t.Fatal("diagnostic sanitization failed")
	}
}
