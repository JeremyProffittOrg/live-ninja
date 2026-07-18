package main

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestComputeAcceptKey(t *testing.T) {
	// RFC 6455 §1.3 worked example.
	if got, want := computeAcceptKey("dGhlIHNhbXBsZSBub25jZQ=="), "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="; got != want {
		t.Fatalf("computeAcceptKey = %q, want %q", got, want)
	}
}

// clientFrame builds a masked client->server frame (payload < 126 bytes).
func clientFrame(opcode byte, fin bool, payload []byte) []byte {
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	mask := []byte{0x1, 0x2, 0x3, 0x4}
	out := []byte{b0, byte(0x80 | len(payload))}
	out = append(out, mask...)
	for i, c := range payload {
		out = append(out, c^mask[i&3])
	}
	return out
}

// readServerFrame reads one unmasked server->client frame (len < 126).
func readServerFrame(t *testing.T, r *bufio.Reader) (opcode byte, payload []byte) {
	t.Helper()
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		t.Fatalf("read server frame header: %v", err)
	}
	opcode = h[0] & 0x0f
	if h[1]&0x80 != 0 {
		t.Fatal("server frame must not be masked")
	}
	length := int(h[1] & 0x7f)
	if length == 126 {
		var ext [2]byte
		_, _ = io.ReadFull(r, ext[:])
		length = int(binary.BigEndian.Uint16(ext[:]))
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatalf("read server frame payload: %v", err)
	}
	return opcode, payload
}

func newServerConn(c net.Conn) *wsConn {
	return &wsConn{conn: c, br: bufio.NewReader(c), readTimeout: 2 * time.Second, writeTimeout: 2 * time.Second}
}

func TestWSReadMessage_SingleFrame(t *testing.T) {
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()
	server := newServerConn(c1)

	go func() { _, _ = c2.Write(clientFrame(opText, true, []byte("hello"))) }()

	op, payload, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != opText || string(payload) != "hello" {
		t.Fatalf("got op=%d payload=%q", op, payload)
	}
}

func TestWSReadMessage_Continuation(t *testing.T) {
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()
	server := newServerConn(c1)

	go func() {
		_, _ = c2.Write(clientFrame(opText, false, []byte("he")))
		_, _ = c2.Write(clientFrame(opContinuation, true, []byte("llo")))
	}()

	op, payload, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != opText || string(payload) != "hello" {
		t.Fatalf("reassembly failed: op=%d payload=%q", op, payload)
	}
}

func TestWSReadMessage_PingAnsweredWithPong(t *testing.T) {
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()
	server := newServerConn(c1)
	client := bufio.NewReader(c2)

	go func() {
		_, _ = c2.Write(clientFrame(opPing, true, []byte("hi")))
		_, _ = c2.Write(clientFrame(opText, true, []byte("data")))
	}()

	// The pong is written from inside ReadMessage; read it off the wire
	// concurrently so the synchronous pipe write can complete.
	pongCh := make(chan []byte, 1)
	go func() {
		op, payload := readServerFrame(t, client)
		if op != opPong {
			t.Errorf("expected pong opcode, got %d", op)
		}
		pongCh <- payload
	}()

	op, payload, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != opText || string(payload) != "data" {
		t.Fatalf("got op=%d payload=%q", op, payload)
	}
	select {
	case p := <-pongCh:
		if string(p) != "hi" {
			t.Fatalf("pong payload = %q, want hi", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no pong observed")
	}
}

func TestWSWriteText_RoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()
	server := newServerConn(c1)
	client := bufio.NewReader(c2)

	got := make(chan string, 1)
	go func() {
		op, payload := readServerFrame(t, client)
		if op != opText {
			t.Errorf("op = %d, want text", op)
		}
		got <- string(payload)
	}()

	if err := server.WriteText([]byte("world")); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if v := <-got; v != "world" {
		t.Fatalf("round trip = %q, want world", v)
	}
}

func TestWSReadMessage_RejectsUnmaskedClientFrame(t *testing.T) {
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()
	server := newServerConn(c1)

	go func() {
		// Unmasked text frame (mask bit clear) — a protocol violation.
		_, _ = c2.Write([]byte{0x80 | opText, byte(len("x")), 'x'})
	}()

	if _, _, err := server.ReadMessage(); err == nil {
		t.Fatal("expected error for unmasked client frame, got nil")
	}
}
