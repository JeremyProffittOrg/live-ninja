// Command realtime-broker is the direct-invoke Lambda (called by the web
// function via lambda:Invoke — never HTTP-exposed) that is the SOLE
// holder of the OpenAI API key (SSM /live-ninja/prod/openai/api_key,
// isolated IAM). It serves five modes on one event seam:
//
//	"session-mint" (default): pre-spend quota gate (token bucket 1/5s
//	  burst 3, daily-minutes cap, monthly-token cap — contracts/
//	  metering.md), server-side persona/voice resolution, then a
//	  config-bound OpenAI Realtime ephemeral token mint
//	  (POST /v1/realtime/client_secrets, ~60s TTL).
//	"fallback-turn": text-only degraded turn via gpt-4o-mini.
//	"fallback-stt":  audio -> gpt-4o-transcribe transcript.
//	"fallback-tts":  text -> gpt-4o-mini-tts MP3 audio.
//	"extract-topics": post-session topic extraction (M11, FR-TOP-01) —
//	  gpt-4o-mini strict-JSON tagging of a finished transcript against the
//	  user's topic taxonomy, invoked by cmd/topics-extract (never by an
//	  end client).
//
// Quota/rate rejections come back as structured {error, code} payloads
// (code 402/429) that the web function maps straight onto the HTTP
// contract in contracts/metering.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

const metricsNamespace = "LiveNinja/RealtimeBroker"

// Request is the broker's invoke-event shape (shared spec M2): identity
// fields always come from the web function's verified authorizer context
// — never from an end client — plus a mode selector and a mode-specific
// payload.
type Request struct {
	Mode          string          `json:"mode,omitempty"` // "", "session-mint", "fallback-turn", "fallback-stt", "fallback-tts", "extract-topics"
	UserID        string          `json:"userId"`
	Surface       string          `json:"surface"`
	DeviceID      string          `json:"deviceId,omitempty"`
	Persona       string          `json:"persona,omitempty"`
	VoiceOverride string          `json:"voiceOverride,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

type turnPayload struct {
	Text string `json:"text"`
}

type sttPayload struct {
	AudioBase64 string `json:"audioBase64"`
	ContentType string `json:"contentType,omitempty"`
	Filename    string `json:"filename,omitempty"`
}

type ttsPayload struct {
	Text  string `json:"text"`
	Voice string `json:"voice,omitempty"`
}

// extractTopicsPayload is the "extract-topics" mode payload: the flattened
// transcript plus the caller's existing (active) topic taxonomy.
type extractTopicsPayload struct {
	Transcript     string                 `json:"transcript"`
	ExistingTopics []realtime.TopicOption `json:"existingTopics"`
}

// Response is the broker's reply for every mode. Exactly one of the
// success shapes or the error shape is populated; Code carries the HTTP
// status the web function should surface (402/429/400/502).
type Response struct {
	// Error shape (contracts/metering.md 402/429 bodies + generic errors).
	Error             string  `json:"error,omitempty"`
	Code              int     `json:"code,omitempty"`
	Kind              string  `json:"kind,omitempty"`
	Message           string  `json:"message,omitempty"`
	Used              float64 `json:"used,omitempty"`
	Limit             float64 `json:"limit,omitempty"`
	ResetAt           string  `json:"resetAt,omitempty"`
	RetryAfterSeconds int     `json:"retryAfterSeconds,omitempty"`

	// Session-mint success shape.
	// Mode is the session-bootstrap transport (FR-VE-03): "openai-direct"
	// (client-direct WebRTC/WSS to OpenAI; ClientSecret populated) or
	// "nova-bridge" (backend media bridge; WSURL+BridgeToken populated).
	Mode          string                 `json:"mode,omitempty"`
	Engine        string                 `json:"engine,omitempty"`
	ClientSecret  *realtime.ClientSecret `json:"clientSecret,omitempty"`
	Model         string                 `json:"model,omitempty"`
	Voice         string                 `json:"voice,omitempty"`
	SessionConfig json.RawMessage        `json:"sessionConfig,omitempty"`
	ToolManifest  json.RawMessage        `json:"toolManifest,omitempty"`
	SessionID     string                 `json:"sessionId,omitempty"`
	// Nova-bridge success fields (Mode == "nova-bridge" only): the WSS URL
	// to open and the short-lived per-session first-party token (also
	// embedded in WSURL) the bridge verifies before opening Bedrock.
	WSURL                string `json:"wsUrl,omitempty"`
	BridgeToken          string `json:"bridgeToken,omitempty"`
	BridgeTokenExpiresAt string `json:"bridgeTokenExpiresAt,omitempty"`
	// QuotaWarning is the ready-to-emit X-LN-Quota-Warning header value
	// (e.g. "daily_minutes=83%"); empty when below the 80% threshold.
	QuotaWarning string `json:"quotaWarning,omitempty"`

	// Fallback success shapes: Text for turn/stt; audio for tts.
	Text        string `json:"text,omitempty"`
	AudioBase64 string `json:"audioBase64,omitempty"`
	ContentType string `json:"contentType,omitempty"`

	// Extract-topics success shape: ids of existing topics the
	// conversation matched, plus proposed brand-new topic names (the
	// caller creates those and assigns their stable ids).
	TopicIDs  []string `json:"topicIds,omitempty"`
	NewTopics []string `json:"newTopics,omitempty"`
}

var validSurfaces = map[string]bool{
	"web":     true,
	"android": true,
	"device":  true,
}

type broker struct {
	log      *slog.Logger
	gate     *realtime.Gate
	minter   *realtime.Minter
	fallback *realtime.FallbackClient

	// ddb/table back the per-mint Guide Entity injection (guides.go): the
	// broker Queries the caller's GUIDE# prefix and appends enabled guides
	// to the persona instructions (FR-MEM-07).
	ddb   realtime.GuideQuerier
	table string

	// settings reads the caller's voiceEngine pin at mint (FR-VE-03); the
	// same *dynamodb.Client as ddb (it satisfies both Query and GetItem).
	settings realtime.SettingsGetter
	// novaMint mints the short-lived per-session bridge token for
	// nova-pinned devices (auth.Signer-backed); nil when JWT_KMS_KEY_ID is
	// unset, in which case a nova mint returns a "bridge unavailable" error.
	novaMint realtime.NovaTokenMinter
	// bridgeBaseURL is the Nova bridge WSS base (NOVA_BRIDGE_URL); empty
	// falls back to realtime.DefaultBridgeBaseURL.
	bridgeBaseURL string
}

func (b *broker) Handle(ctx context.Context, req Request) (Response, error) {
	mode := req.Mode
	if mode == "" {
		mode = "session-mint"
	}
	l := observ.WithRequest(b.log, "", req.UserID, req.Surface).With(slog.String("mode", mode))

	if req.UserID == "" {
		return badRequest("userId is required"), nil
	}
	if !validSurfaces[req.Surface] {
		return badRequest("surface must be one of: web, android, device"), nil
	}

	switch mode {
	case "session-mint":
		return b.handleMint(ctx, l, req), nil
	case "fallback-turn":
		return b.handleFallbackTurn(ctx, l, req), nil
	case "fallback-stt":
		return b.handleFallbackSTT(ctx, l, req), nil
	case "fallback-tts":
		return b.handleFallbackTTS(ctx, l, req), nil
	case "extract-topics":
		return b.handleExtractTopics(ctx, l, req), nil
	default:
		return badRequest("mode must be one of: session-mint, fallback-turn, fallback-stt, fallback-tts, extract-topics"), nil
	}
}

func (b *broker) handleMint(ctx context.Context, l *slog.Logger, req Request) Response {
	// Resolve the device's voiceEngine pin FIRST (FR-VE-03):
	// devices[deviceId] ?? default ?? openai-realtime. Fail open to the
	// openai-realtime default on any read error — a settings-read hiccup must
	// not take voice down.
	engine, err := realtime.ResolveEngine(ctx, b.settings, b.table, req.UserID, req.DeviceID)
	if err != nil {
		l.Warn("realtime-broker: voiceEngine pin resolve failed; defaulting to openai-realtime",
			slog.String("error", err.Error()))
		engine = voiceengine.EngineOpenAIRealtime
	}

	// OpenAI-direct engines validate the requested voice up front; nova-sonic
	// uses its own voice set (resolved by the bridge) so it skips this check.
	var voice string
	if engine.IsClientDirect() {
		v, ok := realtime.ResolveVoice(req.VoiceOverride)
		if !ok {
			return badRequest("voiceOverride is not a supported realtime voice")
		}
		voice = v
	}

	// Pre-spend gate: bucket -> daily -> monthly. Runs and settles before
	// any OpenAI/Bedrock (or even SSM key) touch, so a rejection costs
	// nothing — and gates both engines identically at session start.
	warnings, err := b.gate.CheckMint(ctx, req.UserID)
	if err != nil {
		if resp, handled := gateErrResponse(l, err, "mint"); handled {
			return resp
		}
		l.Error("realtime-broker: quota gate failed", slog.String("error", err.Error()))
		return internalError("quota gate unavailable")
	}

	sessionID, err := newSessionID()
	if err != nil {
		l.Error("realtime-broker: session id generation failed", slog.String("error", err.Error()))
		return internalError("session id generation failed")
	}

	// Nova-pinned device: return a backend-bridge WebSocket bootstrap rather
	// than an OpenAI ephemeral token (the sole path where AWS is in the media
	// path — PRD N-6 exception).
	if engine == voiceengine.EngineNovaSonic {
		return b.handleNovaBridge(ctx, l, req, sessionID, warnings)
	}

	// Guide Entity injection (FR-MEM-07): append the user's enabled guides
	// to the persona instructions, priority order. Best-effort — a guide
	// read failure is logged but must not take voice down with it.
	guideSuffix := ""
	if guides, gerr := realtime.LoadEnabledGuides(ctx, b.ddb, b.table, req.UserID); gerr != nil {
		l.Warn("realtime-broker: guide load failed; minting without guides",
			slog.String("error", gerr.Error()))
	} else {
		guideSuffix = realtime.GuideInstructions(guides)
	}

	start := time.Now()
	res, err := b.minter.Mint(ctx, req.Persona, voice, guideSuffix)
	observ.EmitMetric(metricsNamespace, "EphemeralTokenMintLatency",
		float64(time.Since(start).Milliseconds()), "Milliseconds",
		map[string]string{"Surface": req.Surface})
	if err != nil {
		l.Error("realtime-broker: ephemeral token mint failed", slog.String("error", err.Error()))
		observ.EmitMetric(metricsNamespace, "MintErrors", 1, "Count",
			map[string]string{"Surface": req.Surface})
		return Response{Error: "mint_failed", Code: http.StatusBadGateway,
			Message: "Could not mint a realtime session token; use the fallback cascade."}
	}

	// Post-spend bookkeeping (session ledger LOG# seq-0 marker + dayMints
	// bump). Best-effort: the token is already minted and burning its
	// 60s TTL, so a bookkeeping failure is logged, not fatal.
	if err := b.gate.RecordMint(ctx, req.UserID, sessionID, req.Surface); err != nil {
		l.Warn("realtime-broker: mint bookkeeping failed", slog.String("error", err.Error()),
			slog.String("sessionId", sessionID))
	}

	observ.EmitMetric(metricsNamespace, "SessionsBrokered", 1, "Count",
		map[string]string{"Surface": req.Surface, "Engine": string(engine)})
	l.Info("realtime-broker: session minted",
		slog.String("sessionId", sessionID),
		slog.String("engine", string(engine)),
		slog.String("model", res.Model),
		slog.String("voice", res.Voice))

	return Response{
		Mode:          "openai-direct",
		Engine:        string(engine),
		ClientSecret:  &res.ClientSecret,
		Model:         res.Model,
		Voice:         res.Voice,
		SessionConfig: res.SessionConfig,
		ToolManifest:  res.ToolManifest,
		SessionID:     sessionID,
		QuotaWarning:  strings.Join(warnings, ","),
	}
}

// handleNovaBridge issues the nova-bridge session bootstrap (FR-VE-03) for a
// device pinned to nova-sonic: it mints a short-lived first-party token scoped
// to the bridge (scope "nova", bound to sessionID) and returns the WSS URL the
// client opens instead of an OpenAI ephemeral token. The quota gate has already
// passed (caller); persona/voice/guide resolution and the Bedrock bidirectional
// stream itself are the bridge's responsibility. warnings carries the same
// X-LN-Quota-Warning payload the OpenAI path returns.
func (b *broker) handleNovaBridge(ctx context.Context, l *slog.Logger, req Request, sessionID string, warnings []string) Response {
	if b.novaMint == nil {
		l.Error("realtime-broker: nova-sonic pinned but bridge token minter unavailable (JWT_KMS_KEY_ID unset)",
			slog.String("sessionId", sessionID))
		observ.EmitMetric(metricsNamespace, "MintErrors", 1, "Count",
			map[string]string{"Surface": req.Surface, "Engine": string(voiceengine.EngineNovaSonic)})
		return Response{Error: "nova_bridge_unavailable", Code: http.StatusBadGateway,
			Message: "The Nova Sonic bridge is not configured; use the fallback cascade."}
	}

	bs, err := realtime.BuildBridgeSession(ctx, b.novaMint, b.bridgeBaseURL,
		req.UserID, req.DeviceID, req.Surface, sessionID)
	if err != nil {
		l.Error("realtime-broker: nova bridge session build failed", slog.String("error", err.Error()),
			slog.String("sessionId", sessionID))
		observ.EmitMetric(metricsNamespace, "MintErrors", 1, "Count",
			map[string]string{"Surface": req.Surface, "Engine": string(voiceengine.EngineNovaSonic)})
		return Response{Error: "nova_bridge_failed", Code: http.StatusBadGateway,
			Message: "Could not establish a Nova Sonic bridge session; use the fallback cascade."}
	}

	// Post-spend bookkeeping (same ledger marker + dayMints bump as the OpenAI
	// path). Best-effort: the bridge session is already issued.
	if err := b.gate.RecordMint(ctx, req.UserID, sessionID, req.Surface); err != nil {
		l.Warn("realtime-broker: nova bridge bookkeeping failed", slog.String("error", err.Error()),
			slog.String("sessionId", sessionID))
	}

	observ.EmitMetric(metricsNamespace, "SessionsBrokered", 1, "Count",
		map[string]string{"Surface": req.Surface, "Engine": string(voiceengine.EngineNovaSonic)})
	l.Info("realtime-broker: nova bridge session issued",
		slog.String("sessionId", sessionID),
		slog.String("engine", string(voiceengine.EngineNovaSonic)),
		slog.String("model", realtime.NovaModel))

	return Response{
		Mode:                 "nova-bridge",
		Engine:               string(voiceengine.EngineNovaSonic),
		Model:                realtime.NovaModel,
		WSURL:                bs.WSURL,
		BridgeToken:          bs.Token,
		BridgeTokenExpiresAt: bs.ExpiresAt.UTC().Format(time.RFC3339),
		ToolManifest:         realtime.ToolManifestJSON(),
		SessionID:            sessionID,
		QuotaWarning:         strings.Join(warnings, ","),
	}
}

func (b *broker) handleFallbackTurn(ctx context.Context, l *slog.Logger, req Request) Response {
	var p turnPayload
	if err := json.Unmarshal(orEmptyObject(req.Payload), &p); err != nil || strings.TrimSpace(p.Text) == "" {
		return badRequest("payload.text is required")
	}
	if resp, rejected := b.gateFallback(ctx, l, req); rejected {
		return resp
	}

	text, err := b.fallback.Turn(ctx, req.Persona, p.Text)
	if err != nil {
		return b.fallbackError(l, req, "turn", err)
	}
	b.countFallback(req, "turn")
	return Response{Text: text}
}

func (b *broker) handleFallbackSTT(ctx context.Context, l *slog.Logger, req Request) Response {
	var p sttPayload
	if err := json.Unmarshal(orEmptyObject(req.Payload), &p); err != nil || p.AudioBase64 == "" {
		return badRequest("payload.audioBase64 is required")
	}
	audio, err := base64.StdEncoding.DecodeString(p.AudioBase64)
	if err != nil || len(audio) == 0 {
		return badRequest("payload.audioBase64 must be non-empty standard base64")
	}
	if resp, rejected := b.gateFallback(ctx, l, req); rejected {
		return resp
	}

	text, err := b.fallback.Transcribe(ctx, audio, p.Filename, p.ContentType)
	if err != nil {
		return b.fallbackError(l, req, "stt", err)
	}
	b.countFallback(req, "stt")
	return Response{Text: text}
}

func (b *broker) handleFallbackTTS(ctx context.Context, l *slog.Logger, req Request) Response {
	var p ttsPayload
	if err := json.Unmarshal(orEmptyObject(req.Payload), &p); err != nil || strings.TrimSpace(p.Text) == "" {
		return badRequest("payload.text is required")
	}
	if resp, rejected := b.gateFallback(ctx, l, req); rejected {
		return resp
	}

	audio, err := b.fallback.Speak(ctx, p.Text, p.Voice)
	if err != nil {
		return b.fallbackError(l, req, "tts", err)
	}
	b.countFallback(req, "tts")
	return Response{AudioBase64: base64.StdEncoding.EncodeToString(audio), ContentType: "audio/mpeg"}
}

// handleExtractTopics runs the post-session topic extraction (M11).
// Deliberately NOT behind the quota gate: it fires at most once per
// finished session (each of which already passed the mint gate), it is
// invoked only by the topics-extract Lambda (never a client-reachable
// path), and a token-bucket rejection here would silently drop tagging
// for a session the user already paid for.
func (b *broker) handleExtractTopics(ctx context.Context, l *slog.Logger, req Request) Response {
	var p extractTopicsPayload
	if err := json.Unmarshal(orEmptyObject(req.Payload), &p); err != nil || strings.TrimSpace(p.Transcript) == "" {
		return badRequest("payload.transcript is required")
	}

	res, err := b.fallback.ExtractTopics(ctx, p.Transcript, p.ExistingTopics)
	if err != nil {
		l.Error("realtime-broker: topic extraction failed", slog.String("error", err.Error()))
		observ.EmitMetric(metricsNamespace, "TopicExtractionErrors", 1, "Count",
			map[string]string{"Surface": req.Surface})
		return Response{Error: "extract_failed", Code: http.StatusBadGateway,
			Message: "The topic extraction request failed after retries."}
	}

	observ.EmitMetric(metricsNamespace, "TopicExtractions", 1, "Count",
		map[string]string{"Surface": req.Surface})
	l.Info("realtime-broker: topics extracted",
		slog.Int("existingMatched", len(res.TopicIDs)),
		slog.Int("newProposed", len(res.NewTopics)))
	return Response{TopicIDs: res.TopicIDs, NewTopics: res.NewTopics}
}

// gateFallback runs the fallback-mode quota gate (token bucket + monthly
// ceiling; the daily-minutes cap is realtime-audio-specific). Returns the
// rejection response and true when the request must not proceed.
func (b *broker) gateFallback(ctx context.Context, l *slog.Logger, req Request) (Response, bool) {
	if err := b.gate.CheckFallback(ctx, req.UserID); err != nil {
		if resp, handled := gateErrResponse(l, err, "fallback"); handled {
			return resp, true
		}
		l.Error("realtime-broker: fallback quota gate failed", slog.String("error", err.Error()))
		return internalError("quota gate unavailable"), true
	}
	return Response{}, false
}

func (b *broker) fallbackError(l *slog.Logger, req Request, leg string, err error) Response {
	l.Error("realtime-broker: fallback leg failed",
		slog.String("leg", leg), slog.String("error", err.Error()))
	observ.EmitMetric(metricsNamespace, "FallbackErrors", 1, "Count",
		map[string]string{"Surface": req.Surface, "Leg": leg})
	return Response{Error: "fallback_failed", Code: http.StatusBadGateway,
		Message: "The fallback " + leg + " request failed after retries."}
}

func (b *broker) countFallback(req Request, leg string) {
	observ.EmitMetric(metricsNamespace, "FallbackInvocations", 1, "Count",
		map[string]string{"Surface": req.Surface, "Leg": leg})
}

// gateErrResponse maps the gate's typed rejections onto the
// contracts/metering.md 402/429 bodies. Returns handled=false for
// unexpected (infrastructure) errors.
func gateErrResponse(l *slog.Logger, err error, op string) (Response, bool) {
	var qe *realtime.QuotaExceededError
	if errors.As(err, &qe) {
		observ.EmitMetric(metricsNamespace, "QuotaRejections", 1, "Count",
			map[string]string{"Kind": qe.Kind})
		l.Warn("realtime-broker: quota exceeded",
			slog.String("op", op), slog.String("kind", qe.Kind))
		msg := "Monthly usage limit reached. Resets at " + qe.ResetAt.Format(time.RFC3339) + "."
		if qe.Kind == "daily_minutes" {
			msg = "Daily realtime-audio limit reached. Resets at " + qe.ResetAt.Format(time.RFC3339) + "."
		}
		return Response{
			Error:   "quota_exceeded",
			Code:    http.StatusPaymentRequired,
			Kind:    qe.Kind,
			Message: msg,
			Used:    qe.Used,
			Limit:   qe.Limit,
			ResetAt: qe.ResetAt.Format(time.RFC3339),
		}, true
	}

	var rl *realtime.RateLimitedError
	if errors.As(err, &rl) {
		observ.EmitMetric(metricsNamespace, "QuotaRejections", 1, "Count",
			map[string]string{"Kind": "rate_limited"})
		l.Warn("realtime-broker: rate limited", slog.String("op", op))
		return Response{
			Error:             "rate_limited",
			Code:              http.StatusTooManyRequests,
			Message:           "Too many session requests in a short period. Retry shortly.",
			RetryAfterSeconds: rl.RetryAfterSeconds,
		}, true
	}

	// M7 hardening rejections: suspension (403) and the concurrent-session
	// cap (surfaced as the standard 429 rate_limited shape so every client
	// reuses its existing Retry-After backoff — the message and the EMF
	// dimension distinguish it for humans/ops).
	var se *realtime.SuspendedError
	if errors.As(err, &se) {
		observ.EmitMetric(metricsNamespace, "QuotaRejections", 1, "Count",
			map[string]string{"Kind": "suspended"})
		l.Warn("realtime-broker: account suspended",
			slog.String("op", op), slog.String("reason", se.Reason))
		return Response{
			Error:   "account_suspended",
			Code:    http.StatusForbidden,
			Message: "This account is suspended after unusual usage was detected. Contact the owner to restore access.",
		}, true
	}

	var cl *realtime.ConcurrentLimitError
	if errors.As(err, &cl) {
		observ.EmitMetric(metricsNamespace, "QuotaRejections", 1, "Count",
			map[string]string{"Kind": "concurrent_sessions"})
		l.Warn("realtime-broker: concurrent session limit reached",
			slog.String("op", op), slog.Int("limit", cl.Limit))
		return Response{
			Error:             "rate_limited",
			Code:              http.StatusTooManyRequests,
			Message:           fmt.Sprintf("Concurrent session limit (%d) reached. Retry when a session ends.", cl.Limit),
			RetryAfterSeconds: cl.RetryAfterSeconds,
		}, true
	}

	return Response{}, false
}

func badRequest(msg string) Response {
	return Response{Error: "invalid_request", Code: http.StatusBadRequest, Message: msg}
}

func internalError(msg string) Response {
	return Response{Error: "internal_error", Code: http.StatusInternalServerError, Message: msg}
}

func orEmptyObject(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}

// newSessionID returns a 32-hex-char random session ID for the LOG#
// ledger (crypto/rand; no external deps so go.mod stays untouched).
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func main() {
	ctx := context.Background()
	appCfg := config.FromEnv()
	logger := observ.NewLogger(os.Stdout, appCfg.LogLevel)

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		logger.Error("realtime-broker: load aws config failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	loader := config.NewLoaderWithClient(ssm.NewFromConfig(awsCfg))
	ddb := dynamodb.NewFromConfig(awsCfg)

	model := os.Getenv("OPENAI_REALTIME_MODEL")
	if model == "" {
		model = realtime.DefaultRealtimeModel
	}

	gate := realtime.NewGate(ddb, appCfg.TableName)
	wireSuspendAlerts(gate, logger, awsCfg, appCfg.EmailQueueURL, os.Getenv("OWNER_EMAIL"))

	b := &broker{
		log:           logger,
		gate:          gate,
		minter:        realtime.NewMinter(loader, model),
		fallback:      realtime.NewFallbackClient(loader),
		ddb:           ddb,
		table:         appCfg.TableName,
		settings:      ddb, // *dynamodb.Client satisfies SettingsGetter (GetItem)
		bridgeBaseURL: os.Getenv("NOVA_BRIDGE_URL"),
	}
	wireNovaBridge(b, logger, ctx, appCfg.JWTKmsKeyID)
	lambda.Start(b.Handle)
}

// wireNovaBridge installs the Nova Sonic bridge token minter (M12, FR-VE-03):
// an auth.Signer-backed closure that mints a short-lived first-party JWT scoped
// to the bridge (scope "nova", sid=sessionID) for each nova-pinned session
// bootstrap. Requires JWT_KMS_KEY_ID (the same KMS signing key the web function
// uses) plus kms:Sign on this function's role. When JWT_KMS_KEY_ID is unset (or
// signer init fails) the minter stays nil and nova-pinned devices receive a
// nova_bridge_unavailable error rather than a broken session — OpenAI-pinned
// devices are entirely unaffected.
func wireNovaBridge(b *broker, logger *slog.Logger, ctx context.Context, kmsKeyID string) {
	if kmsKeyID == "" {
		logger.Warn("realtime-broker: JWT_KMS_KEY_ID unset; Nova Sonic bridge disabled (nova-pinned devices get nova_bridge_unavailable)")
		return
	}
	signer, err := auth.NewSigner(ctx, kmsKeyID)
	if err != nil {
		logger.Error("realtime-broker: nova bridge signer init failed; nova mints unavailable",
			slog.String("error", err.Error()))
		return
	}
	b.novaMint = func(ctx context.Context, userID, deviceID, surface, sessionID string) (string, time.Time, error) {
		tok, err := signer.SignAccessToken(ctx, auth.Claims{
			Sub:     userID,
			Sid:     sessionID,
			Did:     deviceID,
			Surface: surface,
			Scope:   realtime.NovaScope,
		})
		if err != nil {
			return "", time.Time{}, err
		}
		return tok, time.Now().Add(auth.AccessTokenTTL), nil
	}
}

// wireSuspendAlerts installs the auto-suspension owner notification: an
// EmailQueue SQS message ({template,to,subject,text} — the exact shape
// cmd/email-dispatch consumes, which sends via SES from jeremy@jeremy.ninja).
// Requires EMAIL_QUEUE_URL + OWNER_EMAIL on this function (plus
// sqs:SendMessage on the queue); when either is unset the alert hook stays
// nil — suspension enforcement and the UserAutoSuspended EMF metric are
// independent of it and always active.
func wireSuspendAlerts(gate *realtime.Gate, logger *slog.Logger, awsCfg aws.Config, queueURL, ownerEmail string) {
	if queueURL == "" || ownerEmail == "" {
		logger.Warn("realtime-broker: suspend email alerts disabled (EMAIL_QUEUE_URL / OWNER_EMAIL not set); EMF metric still emitted")
		return
	}
	sqsClient := sqs.NewFromConfig(awsCfg)
	gate.SetAlerter(func(ctx context.Context, a realtime.SuspendAlert) {
		body, err := json.Marshal(map[string]string{
			"template": "quota-suspend",
			"to":       ownerEmail,
			"subject":  "Live Ninja: user auto-suspended (" + a.Reason + ")",
			"text": fmt.Sprintf(
				"User %s was automatically suspended at %s.\n\n"+
					"Reason: %s\n"+
					"Observed burn: %.0f tokens this UTC hour (threshold %.0f, env QUOTA_HOURLY_BURN_TOKENS).\n\n"+
					"All outstanding access tokens were invalidated (tokensValidAfter bumped).\n"+
					"To reinstate after review: set USER#%s / PROFILE status back to \"active\" (store.ReinstateUser).",
				a.UserID, a.At.Format(time.RFC3339), a.Reason, a.BurnTokens, a.Threshold, a.UserID),
		})
		if err != nil {
			logger.Error("realtime-broker: marshal suspend alert failed", slog.String("error", err.Error()))
			return
		}
		if _, err := sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl:    aws.String(queueURL),
			MessageBody: aws.String(string(body)),
		}); err != nil {
			logger.Error("realtime-broker: suspend alert enqueue failed",
				slog.String("error", err.Error()), slog.String("userId", a.UserID))
		}
	})
}
