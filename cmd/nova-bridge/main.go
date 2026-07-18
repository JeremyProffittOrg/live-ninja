// Command nova-bridge is the Nova Sonic backend media bridge (M12,
// FR-VE-02): a long-lived Go WebSocket server that sits between a device
// pinned to the `nova-sonic` voice engine and Amazon Nova Sonic on Bedrock.
//
// Why a standalone service (not a Lambda): Bedrock's
// InvokeModelWithBidirectionalStream is an HTTP/2 + SigV4 stream the server
// must HOLD open for the whole conversation, pumping audio both ways. Lambda
// (and API Gateway WebSocket, which is request/response per frame) cannot
// hold that stream, so the bridge runs as a single small arm64 Fargate task
// behind an ALB on nova.live.jeremy.ninja (infra wiring lives in the deploy
// workstream's template.yaml; this program only needs a TCP port and the
// task role). It is the ONLY place AWS sits in the audio media path, and
// only for devices explicitly pinned to Nova — every OpenAI engine stays
// client-direct (PRD N-6 / FR-VE-04).
//
// Per connection the bridge:
//  1. verifies the client's first-party session JWT (query param, reusing
//     internal/auth's JWKS verifier — no AWS call),
//  2. runs the pre-spend quota gate (internal/realtime.Gate.CheckMint),
//  3. upgrades to WebSocket and opens the Bedrock bidirectional stream,
//  4. pumps audio/tool/transcript events, normalizing Nova's protocol to and
//     from the engine-neutral internal/voiceengine schema so topics, memory,
//     tools, and the transcript sink behave identically to the OpenAI path.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

const metricsNamespace = "LiveNinja/NovaBridge"

// server holds the bridge's process-wide dependencies.
type server struct {
	log      *slog.Logger
	gate     *realtime.Gate
	bedrock  bedrockClient
	jwks     *jwksProvider
	http     *http.Client
	apiBase  string
	idleRead time.Duration
	maxSess  time.Duration
}

func main() {
	ctx := context.Background()
	appCfg := config.FromEnv()
	log := observ.NewLogger(os.Stdout, appCfg.LogLevel)

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Error("nova-bridge: load aws config failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	jwksURL := os.Getenv("JWKS_URL")
	if jwksURL == "" && os.Getenv("JWKS_JSON") == "" {
		log.Error("nova-bridge: JWKS_URL or JWKS_JSON is required to verify session tokens")
		os.Exit(1)
	}

	httpClient := &http.Client{Timeout: 20 * time.Second}

	srv := &server{
		log:      log,
		gate:     realtime.NewGate(dynamodb.NewFromConfig(awsCfg), appCfg.TableName),
		bedrock:  bedrockruntime.NewFromConfig(awsCfg),
		jwks:     newJWKSProvider(httpClient, jwksURL, os.Getenv("JWKS_JSON")),
		http:     httpClient,
		apiBase:  strings.TrimRight(os.Getenv("API_BASE_URL"), "/"),
		idleRead: durationEnv("WS_IDLE_TIMEOUT_SECONDS", 60*time.Second),
		maxSess:  durationEnv("SESSION_MAX_SECONDS", 8*time.Minute),
	}
	if srv.apiBase == "" {
		log.Warn("nova-bridge: API_BASE_URL unset; transcript sink and tool routing are disabled")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/session", srv.handleSession)
	// CloudFront's /nova/* behavior forwards the full request path (no prefix
	// strip is possible without an edge function), so the public routes arrive
	// prefixed; the bare paths above stay for the ALB target-group health check.
	mux.HandleFunc("/nova/healthz", srv.handleHealth)
	mux.HandleFunc("/nova/session", srv.handleSession)

	addr := ":" + getenv("PORT", "8080")
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout/IdleTimeout: /session hijacks the connection and
		// manages its own deadlines (server.idleRead / maxSess).
	}

	// Graceful shutdown on SIGTERM (Fargate task stop) / SIGINT.
	idleConns := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Info("nova-bridge: shutdown signal received; draining")
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(sctx); err != nil {
			log.Error("nova-bridge: graceful shutdown failed", slog.String("error", err.Error()))
		}
		close(idleConns)
	}()

	log.Info("nova-bridge: listening", slog.String("addr", addr), slog.String("model", NovaModelID))
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("nova-bridge: server error", slog.String("error", err.Error()))
		os.Exit(1)
	}
	<-idleConns
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleSession authenticates and quota-gates the request, then upgrades to
// WebSocket and runs the bridge pump. Auth and quota MUST precede the
// upgrade so failures return a plain HTTP status the client can read.
func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	token := bridgeToken(r)
	if token == "" {
		http.Error(w, "missing session token", http.StatusUnauthorized)
		return
	}
	jwks, err := s.jwks.Get(r.Context())
	if err != nil {
		s.log.Error("nova-bridge: jwks fetch failed", slog.String("error", err.Error()))
		http.Error(w, "auth unavailable", http.StatusServiceUnavailable)
		return
	}
	claims, err := auth.VerifyJWT(token, jwks)
	if err != nil {
		observ.EmitMetric(metricsNamespace, "AuthRejections", 1, "Count", nil)
		// Log the verification failure (claims detail only — never the token)
		// so rejected connects are visible in the bridge logs.
		s.log.Warn("nova-bridge: session token rejected", slog.String("error", err.Error()))
		http.Error(w, "invalid session token", http.StatusUnauthorized)
		return
	}
	// Only tokens the broker minted for the bridge (scope "nova",
	// realtime.NovaScope) may open a Bedrock stream — an ordinary web/session
	// JWT for the same user must not pass, even though it verifies.
	if claims.Scope != realtime.NovaScope {
		observ.EmitMetric(metricsNamespace, "AuthRejections", 1, "Count", nil)
		s.log.Warn("nova-bridge: token not scoped for the bridge",
			slog.String("scope", claims.Scope), slog.String("userId", claims.Sub))
		http.Error(w, "token not scoped for the nova bridge", http.StatusForbidden)
		return
	}

	// The session id comes from the token's sid claim (BuildBridgeSession
	// binds the token to the ledger session id the broker generated); the
	// url's sid query param is informational only and never trusted.
	sessionID := claims.Sid

	// Redeem the session the broker already minted. The broker ran the full
	// pre-spend gate (Gate.CheckMint) and recorded this session
	// (Gate.RecordMint) BEFORE minting the token — re-running CheckMint here
	// counted this session's own concurrency slot and rejected every
	// legitimate connect with 429 "concurrent session limit" (prod
	// 2026-07-18: client stuck at the door, nothing in the bridge logs).
	// CheckSession keeps the enforcement that must hold at redemption time:
	// fresh suspension gate + the RecordMint slot must exist and be inside
	// the hard session cap (bounds replay of a leaked token).
	if err := s.gate.CheckSession(r.Context(), claims.Sub, sessionID); err != nil {
		s.rejectSession(w, err, claims.Sub, sessionID)
		return
	}

	surface := claims.Surface
	l := observ.WithRequest(s.log, "", claims.Sub, surface).With(slog.String("sessionId", sessionID))

	wsc, err := upgradeWebSocket(w, r, s.idleRead, 10*time.Second)
	if err != nil {
		l.Error("nova-bridge: websocket upgrade failed", slog.String("error", err.Error()))
		return
	}
	defer func() { _ = wsc.Close() }()

	observ.EmitMetric(metricsNamespace, "SessionsOpened", 1, "Count", map[string]string{"Surface": surface})
	l.Info("nova-bridge: session opened")
	start := time.Now()

	ctx, cancel := context.WithTimeout(r.Context(), s.maxSess)
	defer cancel()

	sink := newSinkClient(s.http, s.apiBase, token)
	sess := newSession(l, &wsClientConn{ws: wsc}, sink, sessionID, surface,
		func(ctx context.Context) (novaStream, error) { return openNovaStream(ctx, s.bedrock) })

	if err := sess.Run(ctx); err != nil {
		observ.EmitMetric(metricsNamespace, "SessionErrors", 1, "Count", map[string]string{"Surface": surface})
		l.Error("nova-bridge: session ended with error", slog.String("error", err.Error()))
	}
	observ.EmitMetric(metricsNamespace, "SessionDurationMs", float64(time.Since(start).Milliseconds()),
		"Milliseconds", map[string]string{"Surface": surface})
	l.Info("nova-bridge: session closed", slog.Duration("duration", time.Since(start)))
}

// rejectSession maps CheckSession's typed rejections onto HTTP statuses.
// The bridge is pre-upgrade here, so a plain status is all the client
// needs. Every rejection is logged (token never included) — silent 4xxs
// here are exactly what made the concurrent-limit bug undiagnosable.
func (s *server) rejectSession(w http.ResponseWriter, err error, userID, sessionID string) {
	var se *realtime.SuspendedError
	var su *realtime.SessionUnknownError
	switch {
	case errors.As(err, &se):
		observ.EmitMetric(metricsNamespace, "QuotaRejections", 1, "Count", map[string]string{"Kind": "suspended"})
		s.log.Warn("nova-bridge: session rejected: account suspended",
			slog.String("userId", userID), slog.String("sessionId", sessionID))
		http.Error(w, "account suspended", http.StatusForbidden)
	case errors.As(err, &su):
		observ.EmitMetric(metricsNamespace, "QuotaRejections", 1, "Count", map[string]string{"Kind": "session_unknown"})
		s.log.Warn("nova-bridge: session rejected: unknown or expired session",
			slog.String("userId", userID), slog.String("sessionId", sessionID))
		http.Error(w, "unknown or expired session", http.StatusUnauthorized)
	default:
		s.log.Error("nova-bridge: session gate failed", slog.String("error", err.Error()),
			slog.String("userId", userID), slog.String("sessionId", sessionID))
		http.Error(w, "session gate unavailable", http.StatusServiceUnavailable)
	}
}

// bridgeToken extracts the session JWT from the connect request. WebSocket
// handshakes can't reliably carry an Authorization header on every client
// stack (browser WebSocket, ESP-IDF), so the token rides a query param
// (contracts/api.md) — with a Sec-WebSocket-Protocol subprotocol fallback
// and, for non-browser clients that can, a Bearer header.
func bridgeToken(r *http.Request) string {
	q := r.URL.Query()
	if t := q.Get("token"); t != "" {
		return t
	}
	if t := q.Get("access_token"); t != "" {
		return t
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	// Subprotocol form: "bearer, <token>".
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		p = strings.TrimSpace(p)
		if p != "" && !strings.EqualFold(p, "bearer") {
			return p
		}
	}
	return ""
}

// wsClientConn adapts a wsConn to the clientConn the pump consumes,
// (de)serializing the engine-neutral event schema over text frames.
type wsClientConn struct {
	ws *wsConn
}

func (c *wsClientConn) ReadEvent() (voiceengine.Event, error) {
	for {
		op, payload, err := c.ws.ReadMessage()
		if err != nil {
			return voiceengine.Event{}, err
		}
		if op != opText {
			continue // ignore binary frames; the protocol is JSON text
		}
		return voiceengine.ParseEvent(payload)
	}
}

func (c *wsClientConn) WriteEvent(ev voiceengine.Event) error {
	b, err := ev.Marshal()
	if err != nil {
		return err
	}
	return c.ws.WriteText(b)
}

func (c *wsClientConn) Close() error { return c.ws.Close() }

// --- JWKS provider -------------------------------------------------------

// jwksProvider serves the JWKS document auth.VerifyJWT needs, fetched from
// the web app's /.well-known/jwks.json (cached) or pinned via JWKS_JSON.
type jwksProvider struct {
	http   *http.Client
	url    string
	static []byte

	mu        sync.Mutex
	cached    []byte
	expiresAt time.Time
}

const jwksTTL = time.Hour

func newJWKSProvider(httpClient *http.Client, url, static string) *jwksProvider {
	return &jwksProvider{http: httpClient, url: url, static: []byte(static)}
}

func (p *jwksProvider) Get(ctx context.Context) ([]byte, error) {
	if len(p.static) > 0 {
		return p.static, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached != nil && time.Now().Before(p.expiresAt) {
		return p.cached, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		if p.cached != nil {
			return p.cached, nil // serve stale rather than fail auth on a blip
		}
		return nil, err
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		if p.cached != nil {
			return p.cached, nil
		}
		return nil, errors.New("nova-bridge: jwks endpoint returned " + resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessageBytes))
	if err != nil {
		if p.cached != nil {
			return p.cached, nil
		}
		return nil, err
	}
	p.cached = body
	p.expiresAt = time.Now().Add(jwksTTL)
	return body, nil
}

// --- small env helpers ---------------------------------------------------

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func durationEnv(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	secs, err := time.ParseDuration(v + "s")
	if err != nil || secs <= 0 {
		return def
	}
	return secs
}

