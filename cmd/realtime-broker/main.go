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
//	"fallback-turn": text-only degraded turn via gpt-4o-mini. Legacy
//	  payload {text} runs a plain completion; payload {messages} runs a
//	  tool-capable completion bound to the server-executable subset of the
//	  realtime tool catalog, returning either the final text or the model's
//	  tool_calls verbatim — the WEB function executes tools (it holds the
//	  tool-side IAM; this function holds only the OpenAI key) and
//	  re-invokes with the results appended.
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
	"github.com/JeremyProffittOrg/live-ninja/internal/clientver"
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

const metricsNamespace = "LiveNinja/RealtimeBroker"

// Request is the broker's invoke-event shape (shared spec M2): identity
// fields always come from the web function's verified authorizer context
// — never from an end client — plus a mode selector and a mode-specific
// payload.
type Request struct {
	Mode string `json:"mode,omitempty"` // "", "session-mint", "fallback-turn", "fallback-stt", "fallback-tts", "extract-topics"
	// TxID is the caller-supplied transaction correlation id (the web
	// function forwards the ingress txId so a single user action correlates
	// across the web fn and this broker in CloudWatch). Generated here when
	// absent so a direct/system invoke is still traceable.
	TxID          string `json:"txId,omitempty"`
	UserID        string `json:"userId"`
	Surface       string `json:"surface"`
	DeviceID      string `json:"deviceId,omitempty"`
	Persona       string `json:"persona,omitempty"`
	VoiceOverride string `json:"voiceOverride,omitempty"`
	// MicEagerness maps to semantic VAD's eagerness (low|medium|high|auto);
	// empty means auto.
	MicEagerness string          `json:"micEagerness,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	// ClientVersion is the caller's raw X-LN-Client header value, forwarded
	// verbatim by the web function. This broker is invoked with a marshaled
	// struct, NOT a forwarded HTTP request, so it sees no headers of its own
	// — without this field there is no way to tell a current client from a
	// two-year-old one, and the Azure gate below would fail closed for every
	// session (azure-voice-plan.md WS-D M1, gap register W3).
	ClientVersion string `json:"clientVersion,omitempty"`
	// Capabilities is the set of session-bootstrap modes the calling client
	// understands (e.g. "azure-direct", "voice-live-direct"). A client that
	// does not send it gets no Azure engine, which is the fail-closed
	// property the version gate alone cannot provide: two of the three
	// surfaces do not send a parseable X-LN-Client today.
	Capabilities []string `json:"capabilities,omitempty"`
}

type turnPayload struct {
	Text string `json:"text"`
	// Messages selects the tool-capable turn: the conversation so far
	// (user text, assistant tool requests, executed tool results). When
	// present it wins over Text.
	Messages []realtime.ChatMessage `json:"messages,omitempty"`
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
	// TxID echoes the transaction correlation id on every reply (success
	// and error). The web function reads it to stamp the X-LN-Txn response
	// header and to fill the canonical error envelope's txId so a
	// user-reported "Ref: <txId>" pins this exact invocation in CloudWatch.
	TxID string `json:"txId,omitempty"`

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
	Mode         string                 `json:"mode,omitempty"`
	Engine       string                 `json:"engine,omitempty"`
	ClientSecret *realtime.ClientSecret `json:"clientSecret,omitempty"`
	Model        string                 `json:"model,omitempty"`
	// CallsURL is the SDP POST target for the WebRTC bootstrap. Emitted on
	// openai-direct as well as azure-direct, so the default path exercises
	// the field from day one and it cannot rot unnoticed. A client that does
	// not know the field falls back to its compiled-in OpenAI constant.
	CallsURL      string          `json:"callsUrl,omitempty"`
	Voice         string          `json:"voice,omitempty"`
	SessionConfig json.RawMessage `json:"sessionConfig,omitempty"`
	ToolManifest  json.RawMessage `json:"toolManifest,omitempty"`
	SessionID     string          `json:"sessionId,omitempty"`
	// Nova-bridge success fields (Mode == "nova-bridge" only): the WSS URL
	// to open and the short-lived per-session first-party token (also
	// embedded in WSURL) the bridge verifies before opening Bedrock.
	WSURL                string `json:"wsUrl,omitempty"`
	BridgeToken          string `json:"bridgeToken,omitempty"`
	BridgeTokenExpiresAt string `json:"bridgeTokenExpiresAt,omitempty"`
	// Gemini-direct success fields (Mode == "gemini-direct" only, M13): the
	// client-direct Gemini Live WSS endpoint and the single-use ephemeral
	// token. NEVER rename these into the wsUrl/bridgeUrl family — pre-M12
	// firmware detects Nova by field presence (gemini-plan.md §3.4).
	GeminiEndpoint string                      `json:"geminiEndpoint,omitempty"`
	AccessToken    *realtime.GeminiAccessToken `json:"accessToken,omitempty"`
	// QuotaWarning is the ready-to-emit X-LN-Quota-Warning header value
	// (e.g. "daily_minutes=83%"); empty when below the 80% threshold.
	QuotaWarning string `json:"quotaWarning,omitempty"`

	// Fallback success shapes: Text for turn/stt; audio for tts.
	// ToolCalls (tool-capable fallback-turn only) carries the model's
	// requested function calls verbatim — this function never executes
	// them; the web function runs each through internal/tools and
	// re-invokes with the results.
	Text        string                  `json:"text,omitempty"`
	ToolCalls   []realtime.ChatToolCall `json:"toolCalls,omitempty"`
	AudioBase64 string                  `json:"audioBase64,omitempty"`
	ContentType string                  `json:"contentType,omitempty"`

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

// fallbackAPI is the FallbackClient surface the broker dispatches to —
// an interface so tests can fake the OpenAI legs without HTTP.
// *realtime.FallbackClient is the production implementation.
type fallbackAPI interface {
	TurnForSurface(ctx context.Context, personaID, surface, text, extraSystem string) (string, error)
	TurnWithToolsForSurface(ctx context.Context, personaID, surface string, messages []realtime.ChatMessage, extraSystem string) (*realtime.TurnResult, error)
	Transcribe(ctx context.Context, audio []byte, filename, contentType string) (string, error)
	Speak(ctx context.Context, text, voice string) ([]byte, error)
	ExtractTopics(ctx context.Context, transcript string, existing []realtime.TopicOption) (*realtime.ExtractResult, error)
}

// realtimeMintAPI is the client-direct OpenAI minter surface. Keeping the
// broker on this narrow seam lets the Gemini->OpenAI cascade be tested without
// a network call.
type realtimeMintAPI interface {
	Mint(ctx context.Context, personaID, voice, eagerness, instructionsSuffix, surface string) (*realtime.MintResult, error)
	// CallsURL is the SDP POST target for the secrets this minter issues.
	// The broker puts it on the wire rather than letting each client infer
	// the host from the engine name — that inference is exactly what would
	// send an Azure credential to api.openai.com.
	CallsURL() string
}

type broker struct {
	log        *slog.Logger
	gate       *realtime.Gate
	minter     realtimeMintAPI
	miniMinter realtimeMintAPI
	// azureMinter/azureMiniMinter serve the gpt-live-azure pins. They are nil
	// when the Azure endpoint is not configured, which makes an Azure pin
	// cascade to openai-realtime through the existing fallback rather than
	// failing the session.
	azureMinter     realtimeMintAPI
	azureMiniMinter realtimeMintAPI
	fallback        fallbackAPI

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
	// geminiMint mints the single-use Gemini Live ephemeral token for
	// gemini-flash-live-pinned devices (M13). An interface so tests can fake
	// the Google leg; *realtime.GeminiMinter is the production implementation.
	geminiMint geminiMintAPI
}

// geminiMintAPI is the GeminiMinter surface the broker dispatches to.
type geminiMintAPI interface {
	MintForSurface(ctx context.Context, voice, instructions, surface string) (*realtime.GeminiMintResult, error)
}

func (b *broker) Handle(ctx context.Context, req Request) (resp Response, _ error) {
	mode := req.Mode
	if mode == "" {
		mode = "session-mint"
	}

	// Resolve the transaction id: reuse the caller-forwarded txId (the web
	// function's ingress id) when present, else mint a fresh UUID v4 so a
	// direct/system invoke is still correlatable. Threaded into every slog
	// line via WithTxn and echoed on the response.
	txID := req.TxID
	if txID == "" {
		txID = observ.NewTxnID()
	}
	l := observ.WithTxn(
		observ.WithRequest(b.log, "", req.UserID, req.Surface).With(slog.String("mode", mode)),
		txID,
	)

	// Verbose request/response logging: one line at ingress, one at egress
	// with outcome + latency. No payload values, tokens, or transcript
	// content are logged — only mode/identity/outcome.
	start := time.Now()
	l.Info("realtime-broker: invoke start")
	defer func() {
		resp.TxID = txID
		outcome := "ok"
		if resp.Error != "" {
			outcome = "error"
		}
		l.Info("realtime-broker: invoke done",
			slog.String("outcome", outcome),
			slog.String("errorCode", resp.Error),
			slog.Int("status", resp.Code),
			slog.Int64("latencyMs", time.Since(start).Milliseconds()))
	}()

	if req.UserID == "" {
		resp = badRequest("userId is required")
		return
	}
	if !validSurfaces[req.Surface] {
		resp = badRequest("surface must be one of: web, android, device")
		return
	}

	switch mode {
	case "session-mint":
		resp = b.handleMint(ctx, l, req)
	case "fallback-turn":
		resp = b.handleFallbackTurn(ctx, l, req)
	case "fallback-stt":
		resp = b.handleFallbackSTT(ctx, l, req)
	case "fallback-tts":
		resp = b.handleFallbackTTS(ctx, l, req)
	case "extract-topics":
		resp = b.handleExtractTopics(ctx, l, req)
	default:
		resp = badRequest("mode must be one of: session-mint, fallback-turn, fallback-stt, fallback-tts, extract-topics")
	}
	return
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

	// Client-capability gate (azure-voice-plan.md WS-D M1). An Azure engine
	// hands the client a credential it must POST to an Azure host; a client
	// built before this release has that host compiled in as api.openai.com
	// and would send the Azure credential to OpenAI. So an Azure pin is
	// honoured ONLY for a client that has proved it can handle one.
	//
	// It fails closed by construction: no declared capability and no
	// parseable version means "old client", and an old client gets the
	// platform default. This runs BEFORE the quota gate and before any mint,
	// so a rejected client costs nothing.
	if engine.IsAzure() && !clientSupportsAzure(req, engine) {
		l.Warn("realtime-broker: client cannot handle the pinned Azure engine; falling back",
			slog.String("engine", string(engine)),
			slog.String("surface", req.Surface),
			slog.String("clientVersion", req.ClientVersion),
			slog.String("reason", "client_too_old"))
		observ.EmitMetric(metricsNamespace, "EngineFallback", 1, "Count",
			map[string]string{"Surface": req.Surface, "From": string(engine), "Reason": "client_too_old"})
		engine = voiceengine.EngineOpenAIRealtime
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
		resp := b.handleNovaBridge(ctx, l, req, sessionID, warnings)
		if resp.Error == "" || b.minter == nil {
			return resp
		}
		l.Warn("realtime-broker: pinned engine could not mint; falling back",
			slog.String("engine", string(voiceengine.EngineNovaSonic)),
			slog.String("error", resp.Error),
			slog.String("sessionId", sessionID))
		observ.EmitMetric(metricsNamespace, "EngineFallback", 1, "Count",
			map[string]string{"Surface": req.Surface, "From": string(voiceengine.EngineNovaSonic)})
		warnings = append(warnings, "Nova Sonic is unavailable right now; using the default voice engine for this conversation.")
		engine = voiceengine.EngineOpenAIRealtime
	}

	// Gemini-pinned device (M13): client-direct WSS to Gemini Live with a
	// single-use config-constrained ephemeral token — no bridge, no new infra.
	//
	// The engines below tell the caller to "use the fallback cascade" when they
	// cannot mint — but no client has ever implemented one, so a pinned engine
	// that is down was a hard 502 with no way out except changing the setting.
	// That is exactly what a device pinned to Gemini hit while its token mint
	// contract was wrong, so every mint 502'd and the surface simply could not
	// start a conversation.
	//
	// The cascade belongs here rather than in each client: one implementation
	// covers web, Android and anything added later, and the broker is the only
	// place that knows which engines are actually configured. A pinned engine is
	// a preference, not a constraint worth failing the user over.
	if engine == voiceengine.EngineGeminiFlashLive {
		resp := b.handleGeminiDirect(ctx, l, req, sessionID, warnings)
		// Nothing to fall back TO if the default engine is not configured — return
		// the original error rather than pretending, or dereferencing a nil minter.
		if resp.Error == "" || b.minter == nil {
			return resp
		}
		l.Warn("realtime-broker: pinned engine could not mint; falling back",
			slog.String("engine", string(voiceengine.EngineGeminiFlashLive)),
			slog.String("error", resp.Error),
			slog.String("sessionId", sessionID))
		observ.EmitMetric(metricsNamespace, "EngineFallback", 1, "Count",
			map[string]string{"Surface": req.Surface, "From": string(voiceengine.EngineGeminiFlashLive)})
		// Tell the user once, plainly, rather than silently changing engines on
		// them — a voice that sounds different with no explanation is its own bug.
		warnings = append(warnings, "Gemini Live is unavailable right now; using the default voice engine for this conversation.")
		engine = voiceengine.EngineOpenAIRealtime
	}

	// Persona-embedded voice identity (personas are the unit of voice
	// identity): one settings read resolves the locked precedence chain
	//
	//	voice  = personaPrefs[persona].voice ?? persona's suggested voice
	//	         ?? top-level voice/voiceOverride ?? cedar
	//	accent = personaPrefs[persona].accent ?? top-level voiceAccent
	//
	// Lenient end-to-end (internal/realtime/voiceprefs.go): unknown values
	// and read failures fall through the chain — the old voiceOverride 400
	// is gone; a stale/unknown stored voice now mints on the next candidate
	// instead of failing the session. The accent directive composes after
	// the memory directive and before the guide suffix (realtime.Mint
	// appends the combined suffix after memoryUsageDirective).
	sv := realtime.ResolveSessionVoiceForDevice(ctx, b.settings, b.table, req.UserID, req.DeviceID, req.Persona, req.VoiceOverride)
	voice := sv.Voice
	accentDirective := realtime.AccentDirective(sv.AccentID)

	// Base Knowledge block (M15): the stable facts about this user — name,
	// home coordinates, local date/time, units — rendered server-side and
	// appended to every session. One projected GetItem, same lenient posture
	// as the voice read: an empty or unreadable profile yields "" and mints
	// exactly as it did pre-M15.
	baseKnowledge := realtime.BuildBaseKnowledge(
		store.LoadProfileForDevice(ctx, b.settings, b.table, req.UserID, req.DeviceID), time.Now())

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

	// The Azure OpenAI pins reuse this whole path unchanged: same session
	// config, same ephemeral-secret shape, same client WebRTC transport. Only
	// the minter differs, and the client learns the different SDP host from
	// the callsUrl this handler returns.
	mode := "openai-direct"
	openAIMinter := b.minter
	switch engine {
	case voiceengine.EngineOpenAIRealtimeMini:
		openAIMinter = b.miniMinter
	case voiceengine.EngineGPTLiveAzure:
		openAIMinter, mode = b.azureMinter, "azure-direct"
	case voiceengine.EngineGPTLiveAzureMini:
		openAIMinter, mode = b.azureMiniMinter, "azure-direct"
	}

	// An Azure pin with no Azure minter configured cascades to the platform
	// default, the same way a downed Nova bridge or Gemini mint does. It must
	// never 502 a session that openai-realtime could have served.
	if openAIMinter == nil && engine.IsAzure() {
		l.Warn("realtime-broker: azure engine pinned but no azure minter configured; falling back",
			slog.String("engine", string(engine)))
		observ.EmitMetric(metricsNamespace, "EngineFallback", 1, "Count",
			map[string]string{"Surface": req.Surface, "From": string(engine), "Reason": "not_configured"})
		warnings = append(warnings, "The Azure voice engine is unavailable right now; using the default voice engine for this conversation.")
		engine, mode, openAIMinter = voiceengine.EngineOpenAIRealtime, "openai-direct", b.minter
	}
	metricDimensions := map[string]string{
		"Surface": req.Surface,
		"Engine":  string(engine),
	}
	if openAIMinter == nil {
		l.Error("realtime-broker: OpenAI minter unavailable",
			slog.String("engine", string(engine)))
		observ.EmitMetric(metricsNamespace, "MintErrors", 1, "Count", metricDimensions)
		return Response{Error: "mint_failed", Code: http.StatusBadGateway,
			Message: "Could not mint a realtime session token; use the fallback cascade."}
	}

	start := time.Now()
	res, err := openAIMinter.Mint(ctx, req.Persona, voice, req.MicEagerness, baseKnowledge+accentDirective+guideSuffix, req.Surface)
	observ.EmitMetric(metricsNamespace, "EphemeralTokenMintLatency",
		float64(time.Since(start).Milliseconds()), "Milliseconds", metricDimensions)
	if err != nil {
		l.Error("realtime-broker: ephemeral token mint failed",
			slog.String("error", err.Error()),
			slog.String("engine", string(engine)))
		observ.EmitMetric(metricsNamespace, "MintErrors", 1, "Count", metricDimensions)
		return Response{Error: "mint_failed", Code: http.StatusBadGateway,
			Message: "Could not mint a realtime session token; use the fallback cascade."}
	}

	// Post-spend bookkeeping (session ledger LOG# seq-0 marker + dayMints
	// bump). Best-effort: the token is already minted and burning its
	// 60s TTL, so a bookkeeping failure is logged, not fatal.
	if err := b.gate.RecordMint(ctx, req.UserID, sessionID, req.Surface, engine); err != nil {
		l.Warn("realtime-broker: mint bookkeeping failed", slog.String("error", err.Error()),
			slog.String("sessionId", sessionID),
			slog.String("engine", string(engine)))
	}

	observ.EmitMetric(metricsNamespace, "SessionsBrokered", 1, "Count",
		map[string]string{"Surface": req.Surface, "Engine": string(engine)})
	l.Info("realtime-broker: session minted",
		slog.String("sessionId", sessionID),
		slog.String("engine", string(engine)),
		slog.String("model", res.Model),
		slog.String("voice", res.Voice))

	return Response{
		Mode:          mode,
		Engine:        string(engine),
		ClientSecret:  &res.ClientSecret,
		Model:         res.Model,
		CallsURL:      openAIMinter.CallsURL(),
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
// passed (caller). Persona, guides, base knowledge, accent, and the
// server-executable tool manifest are resolved here into a signed-digest-bound
// config that the client relays to the bridge. warnings carries the same
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

	sv := realtime.ResolveSessionVoiceForDevice(ctx, b.settings, b.table, req.UserID, req.DeviceID, req.Persona, req.VoiceOverride)
	baseKnowledge := realtime.BuildBaseKnowledge(
		store.LoadProfileForDevice(ctx, b.settings, b.table, req.UserID, req.DeviceID), time.Now())
	guideSuffix := ""
	if guides, gerr := realtime.LoadEnabledGuides(ctx, b.ddb, b.table, req.UserID); gerr != nil {
		l.Warn("realtime-broker: guide load failed; minting Nova without guides",
			slog.String("error", gerr.Error()))
	} else {
		guideSuffix = realtime.GuideInstructions(guides)
	}
	persona := realtime.ResolvePersona(req.Persona)
	instructions := realtime.InstructionsForServerExecution(persona) +
		realtime.SessionDirectives + baseKnowledge +
		realtime.AccentDirective(sv.AccentID) + guideSuffix
	novaConfig := realtime.BuildNovaSessionConfig(instructions)
	configJSON, err := json.Marshal(novaConfig)
	if err != nil {
		l.Error("realtime-broker: Nova session config marshal failed",
			slog.String("error", err.Error()), slog.String("sessionId", sessionID))
		return Response{Error: "nova_bridge_failed", Code: http.StatusBadGateway,
			Message: "Could not establish a Nova Sonic bridge session; use the fallback cascade."}
	}

	bs, err := realtime.BuildBridgeSession(ctx, b.novaMint, b.bridgeBaseURL,
		req.UserID, req.DeviceID, req.Surface, sessionID, realtime.NovaConfigDigest(novaConfig))
	if err != nil {
		l.Error("realtime-broker: nova bridge session build failed", slog.String("error", err.Error()),
			slog.String("sessionId", sessionID))
		observ.EmitMetric(metricsNamespace, "MintErrors", 1, "Count",
			map[string]string{"Surface": req.Surface, "Engine": string(voiceengine.EngineNovaSonic)})
		return Response{Error: "nova_bridge_failed", Code: http.StatusBadGateway,
			Message: "Could not establish a Nova Sonic bridge session; use the fallback cascade."}
	}

	// RecordMint creates the slot the bridge must atomically redeem. Unlike a
	// client-direct token, a Nova bootstrap without this record is unusable,
	// so fail before returning the URL rather than handing the client a
	// guaranteed-to-be-rejected session.
	if err := b.gate.RecordMint(
		ctx, req.UserID, sessionID, req.Surface, voiceengine.EngineNovaSonic,
	); err != nil {
		l.Error("realtime-broker: nova bridge session record failed", slog.String("error", err.Error()),
			slog.String("sessionId", sessionID))
		observ.EmitMetric(metricsNamespace, "MintErrors", 1, "Count",
			map[string]string{"Surface": req.Surface, "Engine": string(voiceengine.EngineNovaSonic)})
		return Response{Error: "nova_bridge_failed", Code: http.StatusBadGateway,
			Message: "Could not establish a Nova Sonic bridge session; use the fallback cascade."}
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
		SessionConfig:        configJSON,
		ToolManifest:         realtime.ToolManifestJSONForServerExecution(),
		SessionID:            sessionID,
		QuotaWarning:         strings.Join(warnings, ","),
	}
}

// handleGeminiDirect issues the gemini-direct session bootstrap (M13) for a
// device pinned to gemini-flash-live: persona/voice/accent/guides resolve
// exactly like the OpenAI path (voice through the Gemini chain — user
// geminiVoice setting ?? persona GeminiVoice ?? Kore), then a single-use
// config-constrained ephemeral token mints against the Gemini API and the
// client connects DIRECTLY to Google (the API key never leaves this
// function). The quota gate has already passed (caller).
func (b *broker) handleGeminiDirect(ctx context.Context, l *slog.Logger, req Request, sessionID string, warnings []string) Response {
	if b.geminiMint == nil {
		l.Error("realtime-broker: gemini-flash-live pinned but gemini minter unavailable",
			slog.String("sessionId", sessionID))
		observ.EmitMetric(metricsNamespace, "MintErrors", 1, "Count",
			map[string]string{"Surface": req.Surface, "Engine": string(voiceengine.EngineGeminiFlashLive)})
		return Response{Error: "gemini_unavailable", Code: http.StatusBadGateway,
			Message: "The Gemini Live engine is not configured; use the fallback cascade."}
	}

	// Same one-read voice-identity resolution posture as the OpenAI path,
	// through the Gemini chain (D4b); the accent directive is voice-agnostic
	// and composes into the instructions identically.
	gv := realtime.ResolveSessionGeminiVoiceForDevice(ctx, b.settings, b.table, req.UserID, req.DeviceID, req.Persona)
	accentDirective := realtime.AccentDirective(gv.AccentID)
	baseKnowledge := realtime.BuildBaseKnowledge(
		store.LoadProfileForDevice(ctx, b.settings, b.table, req.UserID, req.DeviceID), time.Now())

	guideSuffix := ""
	if guides, gerr := realtime.LoadEnabledGuides(ctx, b.ddb, b.table, req.UserID); gerr != nil {
		l.Warn("realtime-broker: guide load failed; minting without guides",
			slog.String("error", gerr.Error()))
	} else {
		guideSuffix = realtime.GuideInstructions(guides)
	}
	persona := realtime.ResolvePersona(req.Persona)
	instructions := realtime.InstructionsForSurface(persona, req.Surface) + realtime.SessionDirectives + baseKnowledge + accentDirective + guideSuffix

	start := time.Now()
	res, err := b.geminiMint.MintForSurface(ctx, gv.Voice, instructions, req.Surface)
	observ.EmitMetric(metricsNamespace, "EphemeralTokenMintLatency",
		float64(time.Since(start).Milliseconds()), "Milliseconds",
		map[string]string{"Surface": req.Surface})
	if err != nil {
		l.Error("realtime-broker: gemini token mint failed", slog.String("error", err.Error()),
			slog.String("sessionId", sessionID))
		observ.EmitMetric(metricsNamespace, "MintErrors", 1, "Count",
			map[string]string{"Surface": req.Surface, "Engine": string(voiceengine.EngineGeminiFlashLive)})
		return Response{Error: "mint_failed", Code: http.StatusBadGateway,
			Message: "Could not mint a Gemini Live session token; use the fallback cascade."}
	}

	// Post-spend bookkeeping (same ledger marker + dayMints bump as the other
	// engines). Best-effort: the token is already minted.
	if err := b.gate.RecordMint(
		ctx, req.UserID, sessionID, req.Surface, voiceengine.EngineGeminiFlashLive,
	); err != nil {
		l.Warn("realtime-broker: gemini mint bookkeeping failed", slog.String("error", err.Error()),
			slog.String("sessionId", sessionID))
	}

	observ.EmitMetric(metricsNamespace, "SessionsBrokered", 1, "Count",
		map[string]string{"Surface": req.Surface, "Engine": string(voiceengine.EngineGeminiFlashLive)})
	l.Info("realtime-broker: gemini session minted",
		slog.String("sessionId", sessionID),
		slog.String("engine", string(voiceengine.EngineGeminiFlashLive)),
		slog.String("model", res.Model),
		slog.String("voice", res.Voice))

	return Response{
		Mode:           "gemini-direct",
		Engine:         string(voiceengine.EngineGeminiFlashLive),
		Model:          res.Model,
		Voice:          res.Voice,
		GeminiEndpoint: realtime.GeminiLiveEndpoint,
		AccessToken:    &res.AccessToken,
		SessionConfig:  res.SessionConfig,
		ToolManifest:   res.ToolManifest,
		SessionID:      sessionID,
		QuotaWarning:   strings.Join(warnings, ","),
	}
}

func (b *broker) handleFallbackTurn(ctx context.Context, l *slog.Logger, req Request) Response {
	var p turnPayload
	if err := json.Unmarshal(orEmptyObject(req.Payload), &p); err != nil ||
		(len(p.Messages) == 0 && strings.TrimSpace(p.Text) == "") {
		return badRequest("payload.text or payload.messages is required")
	}
	if len(p.Messages) > 0 {
		if err := realtime.ValidateChatMessages(p.Messages); err != nil {
			return badRequest(err.Error())
		}
	}
	if resp, rejected := b.gateFallback(ctx, l, req); rejected {
		return resp
	}

	// A degraded turn gets the same server-composed knowledge a realtime
	// session would (M15): without this, "what's the weather" works by voice
	// and fails in the text fallback, which is a worse bug than the outage
	// that triggered the fallback.
	extraSystem := realtime.SessionDirectives + realtime.BuildBaseKnowledge(
		store.LoadProfileForDevice(ctx, b.settings, b.table, req.UserID, req.DeviceID), time.Now())

	// Tool-capable turn: the server-executable tool catalog only. The
	// model's tool_calls are returned verbatim for the WEB function to
	// execute (this function has no tool-side IAM, by design); device-local
	// tools cannot be delegated through this topology.
	if len(p.Messages) > 0 {
		res, err := b.fallback.TurnWithToolsForSurface(ctx, req.Persona, req.Surface, p.Messages, extraSystem)
		if err != nil {
			return b.fallbackError(l, req, "turn", err)
		}
		b.countFallback(req, "turn")
		return Response{Text: res.Text, ToolCalls: res.ToolCalls}
	}

	text, err := b.fallback.TurnForSurface(ctx, req.Persona, req.Surface, p.Text, extraSystem)
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
		log:             logger,
		gate:            gate,
		minter:          realtime.NewMinter(loader, model),
		miniMinter:      realtime.NewMinter(loader, realtime.MiniRealtimeModel),
		azureMinter:     newAzureMinterFromEnv(loader, os.Getenv("AZURE_OPENAI_DEPLOYMENT")),
		azureMiniMinter: newAzureMinterFromEnv(loader, os.Getenv("AZURE_OPENAI_MINI_DEPLOYMENT")),
		geminiMint:      realtime.NewGeminiMinter(loader, realtime.GeminiLiveModelFromEnv()),
		fallback:        realtime.NewFallbackClient(loader),
		ddb:             ddb,
		table:           appCfg.TableName,
		settings:        ddb, // *dynamodb.Client satisfies SettingsGetter (GetItem)
		bridgeBaseURL:   os.Getenv("NOVA_BRIDGE_URL"),
	}
	wireNovaBridge(b, logger, ctx, appCfg.JWTKmsKeyID)
	lambda.Start(b.Handle)
}

// wireNovaBridge installs the Nova Sonic bridge token minter (M12, FR-VE-03):
// an auth.Signer-backed closure that mints a short-lived first-party JWT scoped
// to the bridge (scope "nova", sid=sessionID, cfg=session-config digest) for
// each nova-pinned session bootstrap. Requires JWT_KMS_KEY_ID (the same KMS signing key the web function
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
	b.novaMint = func(ctx context.Context, userID, deviceID, surface, sessionID, configDigest string) (string, time.Time, error) {
		tok, err := signer.SignAccessToken(ctx, auth.Claims{
			Sub:       userID,
			Sid:       sessionID,
			Did:       deviceID,
			Surface:   surface,
			Scope:     realtime.NovaScope,
			ConfigSHA: configDigest,
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

// azureMinimums are the per-surface client versions that first shipped the
// Azure bootstrap modes. A client at or above its surface's minimum is
// trusted to handle an Azure credential even if it predates the explicit
// capability list.
//
// The web surface is deliberately absent: it does not send X-LN-Client at
// all today, so it can only ever qualify through Capabilities. Android is
// listed but its live builds send "android/0.2.2-hal+r5", which the
// contracts/headers.md grammar rejects outright over the pre-release suffix
// — so in practice Android also qualifies only through Capabilities until
// that header is fixed (gap register F3).
var azureMinimums = map[string][3]int{
	"android": {0, 3, 0},
	"m5stack": {9, 9, 9}, // no M5Stack build supports Azure; keep it unreachable
}

// modeForEngine is the session-bootstrap mode an engine produces. A client
// must declare this mode in Request.Capabilities to be handed that engine.
func modeForEngine(e voiceengine.Engine) string {
	if e.IsVoiceLive() {
		return "voice-live-direct"
	}
	return "azure-direct"
}

// clientSupportsAzure reports whether the calling client can handle the
// bootstrap shape the pinned Azure engine produces. Capability declaration
// wins; a parseable version at or above the surface minimum is the fallback
// for clients that shipped before the capability list existed.
func clientSupportsAzure(req Request, engine voiceengine.Engine) bool {
	want := modeForEngine(engine)
	for _, c := range req.Capabilities {
		if c == want {
			return true
		}
	}
	v, ok := clientver.Parse(req.ClientVersion)
	if !ok {
		return false
	}
	min, known := azureMinimums[v.Surface]
	if !known {
		return false
	}
	return v.AtLeast(min[0], min[1], min[2])
}

// newAzureMinterFromEnv builds an Azure minter when both the resource endpoint
// and the deployment name are configured, and returns nil otherwise. A nil
// minter is a supported state: the pin cascades to openai-realtime with a
// warning rather than failing the session, so shipping the engine constants
// ahead of the Azure configuration is safe.
//
// AZURE_OPENAI_ENDPOINT is the resource's OpenAI host — the value of
// properties.endpoints["OpenAI Realtime API"], not properties.endpoint, which
// on a kind=AIServices resource is the cognitiveservices.azure.com form.
func newAzureMinterFromEnv(loader *config.Loader, deployment string) realtimeMintAPI {
	endpoint := strings.TrimSpace(os.Getenv("AZURE_OPENAI_ENDPOINT"))
	if endpoint == "" || strings.TrimSpace(deployment) == "" {
		return nil
	}
	return realtime.NewAzureMinter(loader, endpoint, deployment)
}
