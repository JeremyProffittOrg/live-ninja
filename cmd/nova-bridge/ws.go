package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Minimal RFC 6455 server-side WebSocket. The bridge holds exactly one
// client socket per session and one Bedrock HTTP/2 stream; pulling in a
// third-party WebSocket dependency (and the go.mod churn that implies)
// is unwarranted for that single, well-scoped surface, so this implements
// just the server half we need: the upgrade handshake, masked client-frame
// reads with continuation reassembly, unmasked server-frame writes, ping/
// pong, and close. It is deliberately small — no permessage-deflate, no
// client mode.

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket opcodes (RFC 6455 §5.2).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// maxMessageBytes caps a single reassembled application message. Audio
// frames are small (a few KB of base64 PCM); 1 MiB is far above any
// legitimate frame and bounds a hostile client's memory pressure.
const maxMessageBytes = 1 << 20

// ErrWSClosed is returned by ReadMessage once the peer has sent a close
// frame or the transport is gone.
var ErrWSClosed = errors.New("nova-bridge: websocket closed")

// wsConn is a hijacked server WebSocket connection. Reads are single-
// goroutine (the client->nova pump); writes are serialized by a mutex so
// the nova->client pump and inline pong replies never interleave frames.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex

	writeTimeout time.Duration
	readTimeout  time.Duration

	closeOnce sync.Once
}

// isWebSocketUpgrade reports whether r is a valid RFC 6455 upgrade request.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		tokenListContains(r.Header.Get("Connection"), "upgrade") &&
		r.Header.Get("Sec-WebSocket-Key") != "" &&
		r.Header.Get("Sec-WebSocket-Version") == "13"
}

func tokenListContains(header, want string) bool {
	for _, tok := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(tok), want) {
			return true
		}
	}
	return false
}

// upgradeWebSocket completes the handshake on w/r and returns a wsConn. It
// must be called from a normal net/http handler whose ResponseWriter
// supports hijacking (Go's default server does).
func upgradeWebSocket(w http.ResponseWriter, r *http.Request, readTimeout, writeTimeout time.Duration) (*wsConn, error) {
	if !isWebSocketUpgrade(r) {
		return nil, errors.New("nova-bridge: not a websocket upgrade request")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("nova-bridge: response writer does not support hijack")
	}
	accept := computeAcceptKey(r.Header.Get("Sec-WebSocket-Key"))

	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("nova-bridge: hijack: %w", err)
	}

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("nova-bridge: write handshake: %w", err)
	}
	if err := brw.Flush(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("nova-bridge: flush handshake: %w", err)
	}

	return &wsConn{
		conn:         conn,
		br:           brw.Reader,
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
	}, nil
}

func computeAcceptKey(key string) string {
	h := sha1.New() //nolint:gosec // RFC 6455 mandates SHA-1 for the accept key; not a security primitive.
	_, _ = io.WriteString(h, key+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ReadMessage returns the next complete application message (text or binary
// opcode), transparently reassembling continuation frames and answering
// ping/close control frames. It returns ErrWSClosed when the peer closes.
func (c *wsConn) ReadMessage() (opcode byte, payload []byte, err error) {
	var (
		msg       []byte
		msgOpcode byte
		haveMsg   bool
	)
	for {
		if c.readTimeout > 0 {
			_ = c.conn.SetReadDeadline(time.Now().Add(c.readTimeout))
		}
		fin, op, data, ferr := c.readFrame()
		if ferr != nil {
			return 0, nil, ferr
		}

		switch op {
		case opClose:
			_ = c.writeControl(opClose, nil)
			return 0, nil, ErrWSClosed
		case opPing:
			if err := c.writeControl(opPong, data); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opText, opBinary:
			if haveMsg {
				return 0, nil, errors.New("nova-bridge: new data frame before previous finished")
			}
			msgOpcode = op
			haveMsg = true
			msg = append(msg, data...)
		case opContinuation:
			if !haveMsg {
				return 0, nil, errors.New("nova-bridge: continuation frame without start")
			}
			msg = append(msg, data...)
		default:
			return 0, nil, fmt.Errorf("nova-bridge: unknown opcode 0x%x", op)
		}

		if len(msg) > maxMessageBytes {
			return 0, nil, errors.New("nova-bridge: message exceeds size cap")
		}
		if fin && haveMsg {
			return msgOpcode, msg, nil
		}
	}
}

// readFrame reads one raw frame. Per RFC 6455 §5.1 every client->server
// frame MUST be masked; an unmasked frame is a protocol error.
func (c *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return false, 0, nil, closedIfEOF(err)
	}
	fin = h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		return false, 0, nil, errors.New("nova-bridge: reserved bits set")
	}
	opcode = h[0] & 0x0f
	masked := h[1]&0x80 != 0
	length := uint64(h[1] & 0x7f)

	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, closedIfEOF(err)
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, closedIfEOF(err)
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > maxMessageBytes {
		return false, 0, nil, errors.New("nova-bridge: frame exceeds size cap")
	}
	if !masked {
		return false, 0, nil, errors.New("nova-bridge: client frame not masked")
	}

	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return false, 0, nil, closedIfEOF(err)
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, closedIfEOF(err)
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}
	return fin, opcode, payload, nil
}

// WriteText sends a single unfragmented text message.
func (c *wsConn) WriteText(payload []byte) error {
	return c.writeFrame(opText, payload)
}

// writeControl sends a control frame (close/ping/pong). Control payloads
// are capped at 125 bytes by the protocol; longer ones are truncated.
func (c *wsConn) writeControl(opcode byte, payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	return c.writeFrame(opcode, payload)
}

// writeFrame writes one unmasked server frame under the write mutex.
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	if c.writeTimeout > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	}

	var header [10]byte
	header[0] = 0x80 | opcode // FIN + opcode
	n := 2
	l := len(payload)
	switch {
	case l < 126:
		header[1] = byte(l)
	case l < 1<<16:
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:4], uint16(l))
		n = 4
	default:
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:10], uint64(l))
		n = 10
	}
	if _, err := c.conn.Write(header[:n]); err != nil {
		return err
	}
	if l > 0 {
		if _, err := c.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// Close sends a close frame (best-effort) and shuts the transport.
func (c *wsConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		_ = c.writeControl(opClose, nil)
		err = c.conn.Close()
	})
	return err
}

func closedIfEOF(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return ErrWSClosed
	}
	return err
}
