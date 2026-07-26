package realtime

// Gemini Live ephemeral-token mint (M13, gemini-flash-live engine). The
// broker is the sole holder of the Gemini API key (SSM
// /live-ninja/prod/gemini/api_key); clients receive only a
// single-use, config-constrained ephemeral token and connect DIRECTLY to
// Google — no bridge, no AWS in the media path (the Nova exception stays
// Nova-only).
//
// Protocol facts:
//   - Gemini's current ephemeral-token contract is v1beta for both mint and
//     Live connection (the July 2026 API contract replaced the preview
//     v1alpha-only behavior used by the original Phase 0 spike).
//   - The provisioning request authenticates with exactly one credential:
//     x-goog-api-key. OAuth2 plus an API key is rejected as mixed auth.
//   - Token-authenticated WSS sessions use the BidiGenerateContentConstrained
//     method with the token URL-escaped in an access_token query param — NOT
//     the API-key BidiGenerateContent endpoint.
//   - The raw wire `setup` frame nests responseModalities/speechConfig under
//     generationConfig (the SDK's LiveConnectConfig flattens them; the wire
//     protocol does not).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// GeminiAPIVersion is shared by token minting and the direct Live endpoint.
// Ephemeral tokens are accepted only when both use the same API version.
const GeminiAPIVersion = "v1beta"

const geminiAuthTokensEndpoint = "https://generativelanguage.googleapis.com/" + GeminiAPIVersion + "/auth_tokens"

// GeminiLiveEndpoint is the WSS endpoint clients open with the minted
// ephemeral token. Ephemeral tokens are only honored by the v1beta
// *Constrained* method (Phase 0 spike; matches the JS SDK's live.connect
// routing for auth_tokens/… keys). Clients append ?access_token=<url-escaped
// token>. Deliberately NOT named anything in the wsUrl/bridgeUrl family —
// pre-M12 firmware detects Nova by field *presence* (gemini-plan.md §3.4).
const GeminiLiveEndpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContentConstrained"

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
	// locked into the token via the REST wire's bidiGenerateContentSetup field
	// (called LiveConnectConstraints by the Go SDK); sending it client-side too
	// is the documented workaround for the known Google bug where a
	// constraints-only systemInstruction is intermittently ignored.
	SessionConfig json.RawMessage
	ToolManifest  json.RawMessage
}

// geminiTokenCreator is the token call injectable for tests. Production uses
// a small REST adapter that performs the same input-to-wire conversion as the
// SDK while retaining stricter redirect and credential-redaction behavior.
type geminiTokenCreator func(ctx context.Context, cfg *genai.CreateAuthTokenConfig) (*genai.AuthToken, error)

// GeminiMinter mints config-constrained Gemini Live ephemeral tokens. Its
// credentials resolve per-call through the SSM-backed config.Loader (cached
// 5 min) — neither appears in a deployed env var.
type GeminiMinter struct {
	loader *config.Loader
	model  string
	// create overrides the token call in tests; nil = production REST path.
	create geminiTokenCreator
	// provisioningClient/Endpoint are overridable only for HTTP contract tests.
	provisioningClient   *http.Client
	provisioningEndpoint string
}

func geminiProvisioningHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// geminiAuthTokenRequest is the final REST wire shape emitted by the official
// Go SDK after it converts the ergonomic LiveConnectConstraints input and
// flattens the nested setup object. Every field present here must also be in
// FieldMask or the provisioning service rejects the request. The setup omits
// sessionResumption entirely: that is the one client-controlled field, so a
// reconnect can add the latest server-issued handle. With an empty fieldMask
// Google ignores the client's entire setup frame, including that handle, and
// a uses:1 token cannot resume.
type geminiAuthTokenRequest struct {
	Uses                     *int32          `json:"uses,omitempty"`
	ExpireTime               *time.Time      `json:"expireTime,omitempty"`
	NewSessionExpireTime     *time.Time      `json:"newSessionExpireTime,omitempty"`
	FieldMask                string          `json:"fieldMask"`
	BidiGenerateContentSetup json.RawMessage `json:"bidiGenerateContentSetup"`
}

// geminiSetupFieldMask mirrors the official SDK's one-level field-mask
// expansion. Every provisioned setup field is represented: Google returns
// INVALID_ARGUMENT if bidiGenerateContentSetup contains a field absent from
// this mask. Empty message fields (for example inputAudioTranscription:{})
// still need a top-level mask entry because their presence enables the
// feature.
func geminiSetupFieldMask(setupJSON json.RawMessage) (string, error) {
	var setup map[string]any
	if err := json.Unmarshal(setupJSON, &setup); err != nil {
		return "", fmt.Errorf("realtime: decode Gemini provisioning setup: %w", err)
	}
	fields := make([]string, 0, len(setup))
	for field, value := range setup {
		if nested, ok := value.(map[string]any); ok && len(nested) > 0 {
			for child := range nested {
				fields = append(fields, field+"."+child)
			}
			continue
		}
		fields = append(fields, field)
	}
	sort.Strings(fields)
	if len(fields) == 0 {
		return "", fmt.Errorf("realtime: Gemini provisioning setup has no lockable fields")
	}
	return strings.Join(fields, ","), nil
}

// geminiProvisioningSetup derives the server-locked portion of the setup
// frame. sessionResumption must not merely be absent from FieldMask: Google
// requires every provisioned field to be masked. Omitting it from the
// provisioned object lets the client's first setup enable resumption and lets
// later connections supply the newest handle while every other field remains
// server-controlled.
func geminiProvisioningSetup(setupJSON json.RawMessage) (json.RawMessage, error) {
	var setup map[string]any
	if err := json.Unmarshal(setupJSON, &setup); err != nil {
		return nil, fmt.Errorf("realtime: decode Gemini provisioning setup: %w", err)
	}
	delete(setup, "sessionResumption")
	if len(setup) == 0 {
		return nil, fmt.Errorf("realtime: Gemini provisioning setup has no lockable fields")
	}
	provisioningJSON, err := json.Marshal(setup)
	if err != nil {
		return nil, fmt.Errorf("realtime: marshal Gemini provisioning setup: %w", err)
	}
	return provisioningJSON, nil
}

func createGeminiAuthToken(
	ctx context.Context,
	client *http.Client,
	endpoint, apiKey string,
	cfg *genai.CreateAuthTokenConfig,
	setupJSON json.RawMessage,
) (*genai.AuthToken, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("realtime: Gemini provisioning credential is empty")
	}
	if cfg == nil {
		return nil, fmt.Errorf("realtime: Gemini provisioning config is empty")
	}
	if len(setupJSON) == 0 {
		return nil, fmt.Errorf("realtime: Gemini provisioning setup is empty")
	}
	provisioningSetup, err := geminiProvisioningSetup(setupJSON)
	if err != nil {
		return nil, err
	}
	fieldMask, err := geminiSetupFieldMask(provisioningSetup)
	if err != nil {
		return nil, err
	}
	wireCfg := geminiAuthTokenRequest{
		Uses:                     cfg.Uses,
		FieldMask:                fieldMask,
		BidiGenerateContentSetup: provisioningSetup,
	}
	if !cfg.ExpireTime.IsZero() {
		expireTime := cfg.ExpireTime
		wireCfg.ExpireTime = &expireTime
	}
	if !cfg.NewSessionExpireTime.IsZero() {
		newSessionExpireTime := cfg.NewSessionExpireTime
		wireCfg.NewSessionExpireTime = &newSessionExpireTime
	}
	body, err := json.Marshal(wireCfg)
	if err != nil {
		return nil, fmt.Errorf("realtime: marshal Gemini auth-token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("realtime: build Gemini auth-token request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)
	if client == nil {
		client = geminiProvisioningHTTPClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		// Keep transport errors URL/header-free so a future client or endpoint
		// change cannot accidentally carry the credential into broker logs.
		return nil, fmt.Errorf("realtime: Gemini auth-token transport failed")
	}
	defer resp.Body.Close()

	const maxResponseBytes = 64 << 10
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&apiErr)
		message := strings.TrimSpace(strings.ReplaceAll(apiErr.Error.Message, apiKey, "[REDACTED]"))
		switch {
		case apiErr.Error.Status != "" && message != "":
			return nil, fmt.Errorf("realtime: Gemini auth-token endpoint returned %s (%s): %s",
				resp.Status, apiErr.Error.Status, message)
		case message != "":
			return nil, fmt.Errorf("realtime: Gemini auth-token endpoint returned %s: %s", resp.Status, message)
		default:
			return nil, fmt.Errorf("realtime: Gemini auth-token endpoint returned %s", resp.Status)
		}
	}

	var token genai.AuthToken
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&token); err != nil {
		return nil, fmt.Errorf("realtime: decode Gemini auth-token response: %w", err)
	}
	return &token, nil
}

// NewGeminiMinter builds a GeminiMinter. model comes from GEMINI_LIVE_MODEL
// (default DefaultGeminiLiveModel).
func NewGeminiMinter(loader *config.Loader, model string) *GeminiMinter {
	if model == "" {
		model = DefaultGeminiLiveModel
	}
	return &GeminiMinter{
		loader:               loader,
		model:                model,
		provisioningClient:   geminiProvisioningHTTPClient(),
		provisioningEndpoint: geminiAuthTokensEndpoint,
	}
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
		// Not a schema object (shouldn't happen for any tool in the catalog —
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
	return geminiToolDeclarationsForSurface("")
}

func geminiToolDeclarationsForSurface(surface string) []map[string]any {
	manifest := toolManifestForSurface(surface)
	decls := make([]map[string]any, 0, len(manifest))
	for _, t := range manifest {
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
	return buildGeminiSetupForSurface(model, voice, instructions, "")
}

func buildGeminiSetupForSurface(model, voice, instructions, surface string) map[string]any {
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
		"tools":                    []map[string]any{{"functionDeclarations": geminiToolDeclarationsForSurface(surface)}},
		"sessionResumption":        map[string]any{},
		"contextWindowCompression": map[string]any{"slidingWindow": map[string]any{}},
		"inputAudioTranscription":  map[string]any{},
		"outputAudioTranscription": map[string]any{},
	}
}

// buildGeminiConstraints mirrors the server-locked subset of
// buildGeminiSetup as SDK-typed LiveConnectConstraints. SessionResumption is
// deliberately nil here and present only in the client setup frame, so a
// reconnect can supply the newest handle. The client cannot substitute its
// own model/voice/instructions even though it sends the setup frame itself.
func buildGeminiConstraints(model, voice, instructions string) *genai.LiveConnectConstraints {
	return buildGeminiConstraintsForSurface(model, voice, instructions, "")
}

func buildGeminiConstraintsForSurface(model, voice, instructions, surface string) *genai.LiveConnectConstraints {
	tools := []*genai.Tool{{FunctionDeclarations: sdkFunctionDeclarationsForSurface(surface)}}
	return &genai.LiveConnectConstraints{
		Model: "models/" + model,
		Config: &genai.LiveConnectConfig{
			ResponseModalities: []genai.Modality{genai.ModalityAudio},
			SpeechConfig: &genai.SpeechConfig{
				VoiceConfig: &genai.VoiceConfig{
					PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{VoiceName: voice},
				},
			},
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: instructions}}},
			Tools:             tools,
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
	return sdkFunctionDeclarationsForSurface("")
}

func sdkFunctionDeclarationsForSurface(surface string) []*genai.FunctionDeclaration {
	raw, err := json.Marshal(geminiToolDeclarationsForSurface(surface))
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
// single-use, config-constrained ephemeral token against v1beta. The
// caller runs the quota gate BEFORE calling this.
func (m *GeminiMinter) Mint(ctx context.Context, voice, instructions string) (*GeminiMintResult, error) {
	return m.MintForSurface(ctx, voice, instructions, "")
}

// MintForSurface scopes device-local tools to the client that receives the
// token while preserving the same server-executed catalog on every surface.
func (m *GeminiMinter) MintForSurface(ctx context.Context, voice, instructions, surface string) (*GeminiMintResult, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(geminiTokenTTL)
	newSessionExpiresAt := now.Add(geminiNewSessionWindow)
	uses := int32(1)
	setup := buildGeminiSetupForSurface(m.model, voice, instructions, surface)
	setupJSON, err := json.Marshal(setup)
	if err != nil {
		return nil, fmt.Errorf("realtime: marshal gemini session config: %w", err)
	}
	tokenCfg := &genai.CreateAuthTokenConfig{
		Uses:                   &uses,
		ExpireTime:             expiresAt,
		NewSessionExpireTime:   newSessionExpiresAt,
		LiveConnectConstraints: buildGeminiConstraintsForSurface(m.model, voice, instructions, surface),
		// A non-nil empty slice tells the SDK to lock exactly the fields in
		// LiveConnectConstraints. Nil means a global lock, which would ignore
		// the client-only sessionResumption field.
		LockAdditionalFields: []string{},
	}

	var tok *genai.AuthToken
	if m.create != nil {
		tok, err = m.create(ctx, tokenCfg)
	} else {
		// The provisioning service accepts exactly one authentication form.
		// Follow the current v1beta REST contract exactly: x-goog-api-key only
		// and bidiGenerateContentSetup in the body. LiveConnectConstraints is
		// the Go SDK's input-only convenience field; the small REST adapter
		// above performs the SDK's conversion without weakening redirect or
		// credential-redaction safeguards.
		apiKey, keyErr := m.loader.Get(ctx, config.ParamGeminiAPIKey, config.EnvOverrideGeminiAPIKey)
		if keyErr != nil {
			return nil, fmt.Errorf("realtime: resolve gemini credential: %w", keyErr)
		}
		client := m.provisioningClient
		if client == nil {
			client = geminiProvisioningHTTPClient()
		}
		endpoint := m.provisioningEndpoint
		if endpoint == "" {
			endpoint = geminiAuthTokensEndpoint
		}
		tok, err = createGeminiAuthToken(ctx, client, endpoint, apiKey, tokenCfg, setupJSON)
	}
	if err != nil {
		return nil, fmt.Errorf("realtime: gemini auth token mint: %w", err)
	}
	if tok == nil || tok.Name == "" {
		return nil, fmt.Errorf("realtime: gemini auth token mint returned no token name")
	}

	return &GeminiMintResult{
		AccessToken: GeminiAccessToken{
			Value:               tok.Name,
			ExpiresAt:           expiresAt.Format(time.RFC3339),
			NewSessionExpiresAt: newSessionExpiresAt.Format(time.RFC3339),
		},
		Model:         m.model,
		Voice:         voice,
		SessionConfig: setupJSON,
		ToolManifest:  ToolManifestJSONForSurface(surface),
	}, nil
}
