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
	"reflect"
	"sort"
	"strings"
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

// geminiSchemaKeywords is the set of JSON-Schema keywords genai.Schema
// actually models, computed by reflecting on the vendored SDK struct's
// `json` tags rather than hand-maintaining a list from memory (M20/D1,
// tool-parity-plan.md P4/Q4). Reflection keeps this in lockstep with
// whatever genai.Schema version go.mod resolves — if a future SDK bump
// adds or drops a field, the sanitizer below adjusts automatically instead
// of silently going stale.
//
// Verified 2026-07-20 against google.golang.org/genai v1.64.0
// (types.go:1846 `type Schema struct`). At that pin the modeled keywords
// are: anyOf, default, description, enum, example, format, items,
// maxItems, maxLength, maxProperties, maximum, minItems, minLength,
// minProperties, minimum, nullable, pattern, properties, propertyOrdering,
// required, title, type — i.e. every keyword the current
// internal/tools/registry.go `jsonSchema()` renderer and the hand-written
// mint.go literal actually use (type, description, properties, required,
// enum, items, minLength, maxLength, pattern, minimum, maximum) round-trips
// through genai.Schema intact today. The strip-and-annotate path below
// exists to protect against a keyword that ISN'T on this list ever landing
// in the manifest (e.g. additionalProperties, const, multipleOf,
// uniqueItems) — proven by direct unit test against synthetic schemas in
// gemini_schema_sanitizer_test.go, since none of the 20 real tools
// currently exercise it.
var geminiSchemaKeywords = func() map[string]bool {
	t := reflect.TypeOf(genai.Schema{})
	set := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag, ok := t.Field(i).Tag.Lookup("json")
		if !ok || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			set[name] = true
		}
	}
	return set
}()

// describeStrippedConstraint renders one stripped JSON-Schema keyword as a
// plain-English sentence (Q4) so the model still learns the rule instead of
// being silently rejected by the router for violating a constraint Gemini
// was never told about. Generated purely from the keyword name and its
// value — never authored per tool — so it can never drift the way
// hand-written per-tool prose would.
func describeStrippedConstraint(keyword string, value any) string {
	switch keyword {
	case "minLength":
		return fmt.Sprintf("Minimum %v characters.", value)
	case "maxLength":
		return fmt.Sprintf("Max %v characters.", value)
	case "pattern":
		return fmt.Sprintf("Must match the pattern %v.", value)
	case "minimum":
		return fmt.Sprintf("Minimum value %v.", value)
	case "maximum":
		return fmt.Sprintf("Maximum value %v.", value)
	case "minItems":
		return fmt.Sprintf("At least %v item(s).", value)
	case "maxItems":
		return fmt.Sprintf("At most %v item(s).", value)
	case "multipleOf":
		return fmt.Sprintf("Must be a multiple of %v.", value)
	case "const":
		return fmt.Sprintf("Must be exactly %v.", value)
	case "uniqueItems":
		return "Items must be unique."
	case "additionalProperties":
		return "No additional properties are allowed."
	case "exclusiveMinimum":
		return fmt.Sprintf("Must be greater than %v.", value)
	case "exclusiveMaximum":
		return fmt.Sprintf("Must be less than %v.", value)
	default:
		b, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprintf("Constraint %q applies.", keyword)
		}
		return fmt.Sprintf("Constraint %q: %s.", keyword, string(b))
	}
}

// deepCopyValue recursively copies a JSON-shaped value (the map[string]any /
// []string / []any / scalar tree produced by encoding/json and by the
// toolManifest literal). Used so geminiToolDeclarations never hands out a
// parameters map that aliases toolManifest's — pre-D1 it did, which meant a
// caller mutating the "sanitized" copy would corrupt every engine's shared
// manifest.
func deepCopyValue(v any) any {
	switch vv := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(vv))
		for k, val := range vv {
			out[k] = deepCopyValue(val)
		}
		return out
	case []any:
		out := make([]any, len(vv))
		for i, val := range vv {
			out[i] = deepCopyValue(val)
		}
		return out
	case []string:
		out := make([]string, len(vv))
		copy(out, vv)
		return out
	default:
		// Scalars (string, float64/int, bool, nil) are copy-by-value already.
		return v
	}
}

// sanitizeSchemaNode mutates one JSON-Schema node (a `parameters` object, a
// property schema, or an array's `items` schema) in place: any keyword not
// in geminiSchemaKeywords is deleted and folded into that same node's own
// `description` as prose (Q4 — "that parameter's description", not the
// owning tool's). Recurses into "properties" (object member schemas) and
// "items" (array element schema) so a constraint nested arbitrarily deep
// still survives as prose. Deterministic: stripped keywords are sorted
// before rendering so repeated runs produce byte-identical output.
func sanitizeSchemaNode(node map[string]any) {
	if node == nil {
		return
	}
	var stripped []string
	for key := range node {
		switch key {
		case "properties", "items", "description":
			continue // structural/recursed, never a "constraint" to strip
		}
		if !geminiSchemaKeywords[key] {
			stripped = append(stripped, key)
		}
	}
	if len(stripped) > 0 {
		sort.Strings(stripped)
		sentences := make([]string, 0, len(stripped))
		for _, key := range stripped {
			sentences = append(sentences, describeStrippedConstraint(key, node[key]))
			delete(node, key)
		}
		desc, _ := node["description"].(string)
		if desc != "" {
			desc += " "
		}
		node["description"] = desc + strings.Join(sentences, " ")
	}

	if props, ok := node["properties"].(map[string]any); ok {
		for _, v := range props {
			if child, ok := v.(map[string]any); ok {
				sanitizeSchemaNode(child)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		sanitizeSchemaNode(items)
	}
}

// sanitizeGeminiParameters deep-copies a tool's `parameters` JSON-Schema map
// and sanitizes the copy in place (sanitizeSchemaNode), so the original
// toolManifest entry is never mutated or aliased.
func sanitizeGeminiParameters(params map[string]any) map[string]any {
	copied, ok := deepCopyValue(params).(map[string]any)
	if !ok {
		// Not a schema object (shouldn't happen for any of the 20 tools —
		// every "parameters" value is a JSON-Schema object) — hand back the
		// original rather than panic; there is nothing to sanitize.
		return params
	}
	sanitizeSchemaNode(copied)
	return copied
}

// geminiToolDeclarations translates the OpenAI-shaped tool manifest entries
// ({type:"function", name, description, parameters}) into Gemini
// functionDeclarations (same JSON-Schema parameters, no type field). The
// parameters map is deep-copied and sanitized (sanitizeGeminiParameters) so
// Gemini never receives a keyword genai.Schema can't model and never shares
// backing storage with toolManifest. Execution is identical across engines:
// the model's toolCall routes to POST /api/v1/tools/invoke and the result
// returns as toolResponse.
func geminiToolDeclarations() []map[string]any {
	decls := make([]map[string]any, 0, len(toolManifest))
	for _, t := range toolManifest {
		decl := map[string]any{
			"name":        t["name"],
			"description": t["description"],
		}
		if params, ok := t["parameters"].(map[string]any); ok {
			decl["parameters"] = sanitizeGeminiParameters(params)
		} else {
			decl["parameters"] = t["parameters"]
		}
		decls = append(decls, decl)
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
