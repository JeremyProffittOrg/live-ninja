package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

// clientConn is the client side of a Nova session as the pump sees it: a
// stream of engine-neutral [voiceengine.Event]s in and out. The real
// implementation wraps a wsConn; tests supply a fake.
type clientConn interface {
	ReadEvent() (voiceengine.Event, error)
	WriteEvent(voiceengine.Event) error
	Close() error
}

// Nova inference defaults. Conservative and overridable only in code — the
// broker owns per-user tuning; the bridge just needs sane values.
const (
	novaMaxTokens   = 1024
	novaTemperature = 0.7
	novaTopP        = 0.9

	defaultNovaVoice = "matthew"

	// transcriptFlushEvery bounds how many buffered final turns accumulate
	// before a sink flush, so a long turn-free stretch still persists.
	transcriptFlushEvery = 8
)

// session drives one client<->Nova conversation end to end.
type session struct {
	log       *slog.Logger
	client    clientConn
	openNova  func(ctx context.Context) (novaStream, error)
	sink      *sinkClient
	sessionID string
	surface   string

	// Nova protocol identifiers, stable for the session's life.
	promptName   string
	audioContent string

	// nova is set once the stream is open; sends are serialized because the
	// audio pump and the tool-result path both write to it.
	nova   novaStream
	novaMu sync.Mutex

	norm *voiceengine.NovaNormalizer

	// transcript buffering.
	tmu     sync.Mutex
	seq     int
	pending []transcriptTurn
}

func newSession(log *slog.Logger, client clientConn, sink *sinkClient, sessionID, surface string,
	open func(ctx context.Context) (novaStream, error)) *session {
	return &session{
		log:          log,
		client:       client,
		openNova:     open,
		sink:         sink,
		sessionID:    sessionID,
		surface:      surface,
		promptName:   uuid.NewString(),
		audioContent: uuid.NewString(),
		norm:         voiceengine.NewNovaNormalizer(),
	}
}

// Run bootstraps the session and pumps until either side ends, then closes
// cleanly (Nova contentEnd/promptEnd/sessionEnd, final transcript flush).
func (s *session) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The first client event configures the session. If the client leads
	// with audio instead, we proceed on defaults and process that event
	// normally after init.
	first, err := s.client.ReadEvent()
	if err != nil {
		return err
	}
	cfg := voiceengine.Config{}
	var deferred *voiceengine.Event
	if first.Type == voiceengine.TypeSessionStart && first.Config != nil {
		cfg = *first.Config
	} else {
		deferred = &first
	}

	nova, err := s.openNova(ctx)
	if err != nil {
		_ = s.client.WriteEvent(voiceengine.Event{Type: voiceengine.TypeError, Code: "nova_open", Message: "could not open the Nova session"})
		return err
	}
	s.nova = nova
	defer func() {
		s.closeNovaGracefully(context.WithoutCancel(ctx))
		_ = nova.Close()
	}()

	if err := s.initNova(ctx, cfg); err != nil {
		_ = s.client.WriteEvent(voiceengine.Event{Type: voiceengine.TypeError, Code: "nova_init", Message: "could not start the Nova session"})
		return err
	}

	// Process a deferred non-config first event (e.g. leading audio.in).
	if deferred != nil {
		if err := s.handleClientEvent(ctx, *deferred); err != nil {
			return err
		}
	}

	var wg sync.WaitGroup
	var once sync.Once
	var runErr error
	fail := func(err error) {
		once.Do(func() { runErr = err })
		cancel()
	}

	wg.Add(2)
	go func() { defer wg.Done(); fail(s.pumpClientToNova(ctx)) }()
	go func() { defer wg.Done(); fail(s.pumpNovaToClient(ctx)) }()
	wg.Wait()

	// Final transcript flush (best-effort — the session is ending anyway).
	if ferr := s.flush(context.WithoutCancel(ctx), true); ferr != nil {
		s.log.Warn("nova-bridge: final transcript flush failed", slog.String("error", ferr.Error()))
	}

	if runErr != nil && !isBenignEnd(runErr) {
		return runErr
	}
	return nil
}

// initNova sends sessionStart, promptStart, an optional SYSTEM prompt, and
// opens the long-lived USER audio content.
func (s *session) initNova(ctx context.Context, cfg voiceengine.Config) error {
	voice := cfg.Voice
	if voice == "" {
		voice = defaultNovaVoice
	}
	if err := s.sendNovaBuilt(ctx, func() ([]byte, error) {
		return voiceengine.NovaSessionStart(novaMaxTokens, novaTemperature, novaTopP)
	}); err != nil {
		return err
	}
	if err := s.sendNovaBuilt(ctx, func() ([]byte, error) {
		return voiceengine.NovaPromptStart(s.promptName, voice, cfg.Tools)
	}); err != nil {
		return err
	}
	if cfg.SystemPrompt != "" {
		docs, err := voiceengine.NovaTextContent(s.promptName, uuid.NewString(), "SYSTEM", cfg.SystemPrompt)
		if err != nil {
			return err
		}
		if err := s.sendNovaAll(ctx, docs); err != nil {
			return err
		}
	}
	return s.sendNovaBuilt(ctx, func() ([]byte, error) {
		return voiceengine.NovaAudioContentStart(s.promptName, s.audioContent)
	})
}

// pumpClientToNova forwards client audio into Nova until the client closes.
func (s *session) pumpClientToNova(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ev, err := s.client.ReadEvent()
		if err != nil {
			return err
		}
		if err := s.handleClientEvent(ctx, ev); err != nil {
			return err
		}
	}
}

func (s *session) handleClientEvent(ctx context.Context, ev voiceengine.Event) error {
	switch ev.Type {
	case voiceengine.TypeAudioIn:
		if ev.Audio == "" {
			return nil
		}
		return s.sendNovaBuilt(ctx, func() ([]byte, error) {
			return voiceengine.NovaAudioInput(s.promptName, s.audioContent, ev.Audio)
		})
	case voiceengine.TypeSessionStart:
		// Re-configuration mid-session is not supported; ignore.
		return nil
	default:
		// turn.*/transcript/etc. from the client are not meaningful inputs.
		return nil
	}
}

// pumpNovaToClient reads Nova output, normalizes it, forwards it to the
// client, mirrors final transcript turns to the sink, and services tool
// calls, until the stream ends.
func (s *session) pumpNovaToClient(ctx context.Context) error {
	for {
		raw, err := s.nova.Recv(ctx)
		if err != nil {
			return err
		}
		for _, ev := range s.norm.Push(raw) {
			if err := s.dispatch(ctx, ev); err != nil {
				return err
			}
		}
	}
}

func (s *session) dispatch(ctx context.Context, ev voiceengine.Event) error {
	switch ev.Type {
	case voiceengine.TypeToolCall:
		// Surface the call to the client for UI, then execute and feed the
		// result back to Nova.
		_ = s.client.WriteEvent(ev)
		return s.handleToolCall(ctx, ev)
	case voiceengine.TypeTranscript:
		if ev.Final {
			s.bufferTurn(ctx, ev)
		}
		return s.client.WriteEvent(ev)
	case voiceengine.TypeTurnEnd:
		// A settled turn is a good moment to persist accumulated transcript.
		if err := s.flush(ctx, false); err != nil {
			s.log.Warn("nova-bridge: transcript flush failed", slog.String("error", err.Error()))
		}
		return s.client.WriteEvent(ev)
	default:
		return s.client.WriteEvent(ev)
	}
}

// handleToolCall executes a model tool call and returns its result to Nova,
// then echoes the result to the client.
func (s *session) handleToolCall(ctx context.Context, call voiceengine.Event) error {
	result, status, err := s.sink.InvokeTool(ctx, call)
	if err != nil {
		s.log.Error("nova-bridge: tool invoke failed",
			slog.String("tool", call.ToolName), slog.String("error", err.Error()))
		result = []byte(`{"error":"tool_invoke_failed"}`)
		status = 502
	}
	docs, berr := voiceengine.NovaToolResult(s.promptName, uuid.NewString(), call.ToolCallID, string(result))
	if berr != nil {
		return berr
	}
	if err := s.sendNovaAll(ctx, docs); err != nil {
		return err
	}
	_ = s.client.WriteEvent(voiceengine.Event{
		Type:       voiceengine.TypeToolResult,
		ToolCallID: call.ToolCallID,
		ToolName:   call.ToolName,
		ToolResult: result,
	})
	s.log.Info("nova-bridge: tool executed",
		slog.String("tool", call.ToolName), slog.Int("status", status))
	return nil
}

// bufferTurn appends a final transcript turn under the next sequence number.
func (s *session) bufferTurn(_ context.Context, ev voiceengine.Event) {
	s.tmu.Lock()
	turn := transcriptTurn{Seq: s.seq, Role: ev.Role, Text: ev.Text, Engine: "nova-sonic"}
	s.seq++
	s.pending = append(s.pending, turn)
	overflow := len(s.pending) >= transcriptFlushEvery
	s.tmu.Unlock()
	if overflow {
		if err := s.flush(context.Background(), false); err != nil {
			s.log.Warn("nova-bridge: transcript overflow flush failed", slog.String("error", err.Error()))
		}
	}
}

// flush posts buffered turns (and, when final, the session-end marker).
func (s *session) flush(ctx context.Context, final bool) error {
	s.tmu.Lock()
	turns := s.pending
	s.pending = nil
	s.tmu.Unlock()
	if len(turns) == 0 && !final {
		return nil
	}
	return s.sink.FlushTranscript(ctx, s.sessionID, turns, final)
}

// closeNovaGracefully closes the audio content, prompt, and session so
// Bedrock finalizes cleanly. Best-effort with a short deadline.
func (s *session) closeNovaGracefully(ctx context.Context) {
	if s.nova == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, build := range []func() ([]byte, error){
		func() ([]byte, error) { return voiceengine.NovaContentEnd(s.promptName, s.audioContent) },
		func() ([]byte, error) { return voiceengine.NovaPromptEnd(s.promptName) },
		func() ([]byte, error) { return voiceengine.NovaSessionEnd() },
	} {
		if err := s.sendNovaBuilt(ctx, build); err != nil {
			return // stream already gone; nothing more to do
		}
	}
}

// sendNovaBuilt builds one Nova document and sends it under the send mutex.
func (s *session) sendNovaBuilt(ctx context.Context, build func() ([]byte, error)) error {
	doc, err := build()
	if err != nil {
		return err
	}
	s.novaMu.Lock()
	defer s.novaMu.Unlock()
	return s.nova.Send(ctx, doc)
}

// sendNovaAll sends a sequence of prebuilt documents atomically w.r.t. the
// send mutex, so a multi-part content (start/body/end) is never interleaved
// with an audio frame.
func (s *session) sendNovaAll(ctx context.Context, docs [][]byte) error {
	s.novaMu.Lock()
	defer s.novaMu.Unlock()
	for _, d := range docs {
		if err := s.nova.Send(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

// isBenignEnd reports whether an end-of-pump error is a normal shutdown
// (peer closed, stream drained, context cancelled) rather than a fault.
func isBenignEnd(err error) bool {
	return errors.Is(err, ErrWSClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}
