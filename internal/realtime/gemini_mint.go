package realtime

// Gemini Live ephemeral-token mint (M13, gemini-flash-live engine). The
// broker is the sole holder of the Gemini API key (SSM
// /live-ninja/prod/gemini/api_key); clients receive only a single-use,
// config-constrained ephemeral token and connect DIRECTLY to Google — no
// bridge, no AWS in the media path (the Nova exception stays Nova-only).
//
// Protocol facts proven live in the Phase 0 spike (gemini-plan.md §10):
//   - Tokens must be minted against the v1alpha API surface
//     (genai.HTTPOptions{APIVersion: "v1alpha"}).
//   - Token-authenticated WSS sessions use the BidiGenerateContentConstrained
//     method with the token URL-escaped in an access_token query param — NOT
//     the API-key BidiGenerateContent endpoint.
//   - The raw wire `setup` frame nests responseModalities/speechConfig under
//     generationConfig (the SDK's LiveConnectConfig flattens them; the wire
//     protocol does not).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"google.golang.org/genai"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
)

// DefaultGeminiLiveModel is the model used when GEMINI_LIVE_MODEL is unset
// (template.yaml passes the GeminiLiveModel parameter, same default).
const DefaultGeminiLiveModel = "gemini-3.1-flash-live-preview"

// GeminiLiveEndpoint is the WSS endpoint clients open with the minted
// ephemeral token. Ephemeral tokens are only honored by the v1alpha
// *Constrained* method (Phase 0 spike; matches the JS SDK's live.connect
// routing for auth_tokens/… keys). Clients append ?access_token=<url-escaped
// token>. Deliberately NOT named anything in the wsUrl/bridgeUrl family —
// pre-M12 firmware detects Nova by field *presence* (gemini-plan.md §3.4).
const GeminiLiveEndpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContentConstrained"

// geminiTokenTTL is the minted token's message-window lifetime. 30 minutes
// (the API default) bounds a stolen token; past it the client re-fetches the
// session bootstrap (fresh token) and resumes via its resumption handle —
// the same per-reconnect re-mint pattern as the Nova bridge.
const geminiTokenTTL = 30 * time.Minute

// geminiNewSessionWindow is how long the token can open its FIRST session.
// Clients connect immediately after bootstrap; 2 minutes absorbs slow
// networks/retries (the spike minted with the same window). Resumption
// reconnects within geminiTokenTTL are NOT bounded by this.
const geminiNewSessionWindow = 2 * time.Minute

// GeminiAccessToken is the minted ephemeral credential returned to clients
// (the §3.4 bootstrap shape's accessToken object).
type GeminiAccessToken struct {
	Value string `json:"value"`
	// ExpiresAt is the token's message-window end (~30 min): past it the
	// client must re-fetch GET /api/v1/realtime/session for a fresh token.
	ExpiresAt string `json:"expiresAt"` // RFC3339 UTC
	// NewSessionExpiresAt is the first-connect window (~2 min).
	NewSessionExpiresAt string `json:"newSessionExpiresAt"` // RFC3339 UTC
}

// GeminiMintResult is everything a gemini-flash-live client needs to open
// its direct WSS session.
type GeminiMintResult struct {
	AccessToken GeminiAccessToken
	Model       string
	Voice       string
	// SessionConfig is the exact raw `setup` frame BODY the client must send
	// on open (wire shape, generationConfig nesting). The same config is also
	// locked into the token via liveConnectConstraints; sending it client-side
	// too is the documented workaround for the known Google bug where a
	// constraints-only systemInstruction is intermittently ignored.
	SessionConfig json.RawMessage
	ToolManifest  json.RawMessage
}

// geminiTokenCreator is the one SDK call the minter makes, injectable for
// tests (production: a genai.Client's AuthTokens service).
type geminiTokenCreator func(ctx context.Context, cfg *genai.CreateAuthTokenConfig) (*genai.AuthToken, error)

// GeminiMinter mints config-constrained Gemini Live ephemeral tokens. The
// Gemini API key resolves per-call through the SSM-backed config.Loader
// (cached 5 min) — it never appears in a deployed env var.
type GeminiMinter struct {
	loader *config.Loader
	model  string
	// create overrides the SDK token call in tests; nil = production path.
	create geminiTokenCreator
}

// NewGeminiMinter builds a GeminiMinter. model comes from GEMINI_LIVE_MODEL
// (default DefaultGeminiLiveModel).
func NewGeminiMinter(loader *config.Loader, model string) *GeminiMinter {
	if model == "" {
		model = DefaultGeminiLiveModel
	}
	return &GeminiMinter{loader: loader, model: model}
}

// GeminiLiveModelFromEnv resolves the broker's Gemini Live model id.
func GeminiLiveModelFromEnv() string {
	if m := os.Getenv("GEMINI_LIVE_MODEL"); m != "" {
		return m
	}
	return DefaultGeminiLiveModel
}

// Model returns the Gemini Live model this minter binds into sessions.
func (m *GeminiMinter) Model() string { return m.model }

// geminiToolDeclarations translates the OpenAI-shaped tool manifest entries
// ({type:"function", name, description, parameters}) into Gemini
// functionDeclarations (same JSON-Schema parameters, no type field).
// Execution is identical across engines: the model's toolCall routes to
// POST /api/v1/tools/invoke and the result returns as toolResponse.
func geminiToolDeclarations() []map[string]any {
	decls := make([]map[string]any, 0, len(toolManifest))
	for _, t := range toolManifest {
		decls = append(decls, map[string]any{
			"name":        t["name"],
			"description": t["description"],
			"parameters":  t["parameters"],
		})
	}
	return decls
}

// buildGeminiSetup assembles the raw wire `setup` frame body (the
// SessionConfig echo) for one session: model, AUDIO-only output with the
// resolved voice, full persona+directive instructions, the translated tool
// declarations, resumption + sliding-window compression (lifts the 15-min
// audio cap; goAway/resume handles the ~10-min connection recycle), and
// both transcription streams (they feed the same transcript sink the other
// engines use).
func buildGeminiSetup(model, voice, instructions string) map[string]any {
	return map[string]any{
		"model": "models/" + model,
		"generationConfig": map[string]any{
			"responseModalities": []string{"AUDIO"},
			"speechConfig": map[string]any{
				"voiceConfig": map[string]any{
					"prebuiltVoiceConfig": map[string]any{"voiceName": voice},
				},
			},
		},
		"systemInstruction": map[string]any{
			"parts": []map[string]any{{"text": instructions}},
		},
		"tools":                    []map[string]any{{"functionDeclarations": geminiToolDeclarations()}},
		"sessionResumption":        map[string]any{},
		"contextWindowCompression": map[string]any{"slidingWindow": map[string]any{}},
		"inputAudioTranscription":  map[string]any{},
		"outputAudioTranscription": map[string]any{},
	}
}

// buildGeminiConstraints mirrors buildGeminiSetup as the SDK-typed
// LiveConnectConstraints locked into the token at mint, so a client cannot
// substitute its own model/voice/instructions even though it sends the
// setup frame itself.
func buildGeminiConstraints(model, voice, instructions string) *genai.LiveConnectConstraints {
	tools := []*genai.Tool{{FunctionDeclarations: sdkFunctionDeclarations()}}
	return &genai.LiveConnectConstraints{
		Model: model,
		Config: &genai.LiveConnectConfig{
			ResponseModalities: []genai.Modality{genai.ModalityAudio},
			SpeechConfig: &genai.SpeechConfig{
				VoiceConfig: &genai.VoiceConfig{
					PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{VoiceName: voice},
				},
			},
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: instructions}}},
			Tools:             tools,
			SessionResumption: &genai.SessionResumptionConfig{},
			ContextWindowCompression: &genai.ContextWindowCompressionConfig{
				SlidingWindow: &genai.SlidingWindow{},
			},
			InputAudioTranscription:  &genai.AudioTranscriptionConfig{},
			OutputAudioTranscription: &genai.AudioTranscriptionConfig{},
		},
	}
}

// sdkFunctionDeclarations converts the tool manifest into the SDK's typed
// FunctionDeclaration list via a JSON round-trip (the manifest's parameters
// are plain map JSON-Schema; genai.Schema unmarshals the same wire shape).
func sdkFunctionDeclarations() []*genai.FunctionDeclaration {
	raw, err := json.Marshal(geminiToolDeclarations())
	if err != nil {
		panic(fmt.Sprintf("realtime: marshal gemini tool declarations: %v", err))
	}
	var decls []*genai.FunctionDeclaration
	if err := json.Unmarshal(raw, &decls); err != nil {
		panic(fmt.Sprintf("realtime: unmarshal gemini tool declarations: %v", err))
	}
	return decls
}

// Mint resolves nothing itself — the broker passes the already-resolved
// voice and full instruction text (persona + memory directive + accent +
// guides, the same composition the OpenAI path mints with) — and creates a
// single-use, config-constrained ephemeral token against v1alpha. The
// caller runs the quota gate BEFORE calling this.
func (m *GeminiMinter) Mint(ctx context.Context, voice, instructions string) (*GeminiMintResult, error) {
	create := m.create
	if create == nil {
		apiKey, err := m.loader.Get(ctx, config.ParamGeminiAPIKey, config.EnvOverrideGeminiAPIKey)
		if err != nil {
			return nil, fmt.Errorf("realtime: resolve gemini key: %w", err)
		}
		client, err := genai.NewClient(ctx, &genai.ClientConfig{
			APIKey:  apiKey,
			Backend: genai.BackendGeminiAPI,
			// Ephemeral tokens exist only on v1alpha (Phase 0 spike: minting
			// through the default surface yields a token every WSS method
			// rejects as an unregistered caller).
			HTTPOptions: genai.HTTPOptions{APIVersion: "v1alpha"},
		})
		if err != nil {
			return nil, fmt.Errorf("realtime: gemini client init: %w", err)
		}
		create = client.AuthTokens.Create
	}

	now := time.Now().UTC()
	expiresAt := now.Add(geminiTokenTTL)
	newSessionExpiresAt := now.Add(geminiNewSessionWindow)
	uses := int32(1)

	tok, err := create(ctx, &genai.CreateAuthTokenConfig{
		Uses:                   &uses,
		ExpireTime:             expiresAt,
		NewSessionExpireTime:   newSessionExpiresAt,
		LiveConnectConstraints: buildGeminiConstraints(m.model, voice, instructions),
	})
	if err != nil {
		return nil, fmt.Errorf("realtime: gemini auth token mint: %w", err)
	}
	if tok == nil || tok.Name == "" {
		return nil, fmt.Errorf("realtime: gemini auth token mint returned no token name")
	}

	cfgJSON, err := json.Marshal(buildGeminiSetup(m.model, voice, instructions))
	if err != nil {
		return nil, fmt.Errorf("realtime: marshal gemini session config: %w", err)
	}

	return &GeminiMintResult{
		AccessToken: GeminiAccessToken{
			Value:               tok.Name,
			ExpiresAt:           expiresAt.Format(time.RFC3339),
			NewSessionExpiresAt: newSessionExpiresAt.Format(time.RFC3339),
		},
		Model:         m.model,
		Voice:         voice,
		SessionConfig: cfgJSON,
		ToolManifest:  toolManifestJSON,
	}, nil
}
