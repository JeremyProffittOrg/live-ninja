package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
)

// DefaultRealtimeModel is the model used when OPENAI_REALTIME_MODEL is
// unset (workflow passes the OpenAIRealtimeModel template parameter,
// default gpt-realtime).
const DefaultRealtimeModel = "gpt-realtime"

// clientSecretsURL is OpenAI's ephemeral-token mint endpoint. The token
// returned is config-bound: the session object sent here (model, voice,
// instructions, tools, turn detection) is fixed server-side and cannot be
// overridden by the client that later connects with the ephemeral secret.
const clientSecretsURL = "https://api.openai.com/v1/realtime/client_secrets"

// ephemeralTTLSeconds is how long a minted client secret stays valid.
// Clients use it immediately to open their WebRTC/WSS session, so 60s is
// deliberately tight (shared spec: expires_after 60).
const ephemeralTTLSeconds = 60

// toolManifest is the OpenAI Realtime function-tool declaration set bound
// into every session at mint. Execution never happens client-side or in
// OpenAI: every function_call is routed to POST /api/v1/tools/invoke where
// the server-side tool router (internal/tools) re-validates arguments
// against its own schemas and re-authorizes the user per call. This
// manifest is what the model sees; the router remains the enforcement
// point.
var toolManifest = []map[string]any{
	{
		"type":        "function",
		"name":        "send_email",
		"description": "Send an email. By default the message goes to the account owner's own address. Sending to any other recipient requires the user's explicit confirmation and confirmExternal=true.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to": map[string]any{
					"type":        "string",
					"description": "Recipient email address. Omit to send to the account owner.",
				},
				"subject": map[string]any{"type": "string", "description": "Email subject line."},
				"body":    map[string]any{"type": "string", "description": "Plain-text email body."},
				"confirmExternal": map[string]any{
					"type":        "boolean",
					"description": "Must be true when 'to' is not the account owner, and only after the user explicitly confirmed the external recipient out loud.",
				},
			},
			"required": []string{"subject", "body"},
		},
	},
	{
		"type":        "function",
		"name":        "set_timer",
		"description": "Set a one-shot timer that notifies the user when it fires.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"seconds": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     86400,
					"description": "Timer duration in seconds from now (max 24 hours).",
				},
				"label": map[string]any{"type": "string", "description": "Short label for what the timer is for."},
			},
			"required": []string{"seconds"},
		},
	},
	{
		"type":        "function",
		"name":        "set_reminder",
		"description": "Schedule a reminder notification at a specific future time.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"at": map[string]any{
					"type":        "string",
					"description": "When to fire, as an ISO-8601 date-time with timezone offset, e.g. 2026-07-18T09:00:00-04:00.",
				},
				"message": map[string]any{"type": "string", "description": "The reminder text to deliver."},
			},
			"required": []string{"at", "message"},
		},
	},
	{
		"type":        "function",
		"name":        "device_control",
		"description": "Send a control action to one of the user's own registered devices (e.g. the M5Stack terminal). Only devices belonging to the user can be controlled.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"deviceId": map[string]any{"type": "string", "description": "The target device's ID from the user's registered devices."},
				"action":   map[string]any{"type": "string", "description": "Control action to perform, e.g. screen_on, screen_off, volume_up, volume_down, mute, unmute."},
			},
			"required": []string{"deviceId", "action"},
		},
	},
	{
		"type":        "function",
		"name":        "get_weather",
		"description": "Get current weather and a short forecast for a location.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"location": map[string]any{"type": "string", "description": "City or place name, e.g. 'Raleigh, NC'."},
				"days": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     7,
					"description": "Forecast days to include (default 1).",
				},
			},
			"required": []string{"location"},
		},
	},
	{
		"type":        "function",
		"name":        "web_lookup",
		"description": "Look up a factual topic (encyclopedia-style summary). Use for people, places, things, and definitions.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "The topic to look up."},
			},
			"required": []string{"query"},
		},
	},
	{
		"type":        "function",
		"name":        "remember_note",
		"description": "Save a note for the user to recall later.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string", "description": "The note content to remember."},
				"tags": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional short tags to organize the note.",
				},
			},
			"required": []string{"text"},
		},
	},
	{
		"type":        "function",
		"name":        "recall_note",
		"description": "Search the user's previously saved notes.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Words or a tag to search the saved notes for."},
			},
			"required": []string{"query"},
		},
	},
	{
		"type": "function",
		"name": "memory_search",
		"description": "Search the user's long-term memory (people, places, information, projects, tasks, plans) " +
			"by meaning. Use before asking the user to repeat something they may have told you before.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "What to look for, phrased naturally."},
				"type": map[string]any{
					"type":        "string",
					"enum":        []string{"person", "place", "info", "project", "task", "plan"},
					"description": "Optionally restrict results to one entity type.",
				},
				"limit": map[string]any{
					"type": "integer", "minimum": 1, "maximum": 20,
					"description": "Maximum results to return (default 5).",
				},
			},
			"required": []string{"query"},
		},
	},
	{
		"type": "function",
		"name": "memory_write",
		"description": "Save or update a long-term memory entity about the user's life (person, place, information, " +
			"project, task, or plan). Use for lasting facts worth remembering across conversations.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"type": map[string]any{
					"type":        "string",
					"enum":        []string{"person", "place", "info", "project", "task", "plan"},
					"description": "The kind of entity being remembered.",
				},
				"name": map[string]any{"type": "string", "description": "Short display name for the entity."},
				"attrs": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Facts as \"key=value\" entries, e.g. [\"birthday=March 3\"].",
				},
				"relations": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Edges to other entities as \"relationType:targetEntityId\" entries.",
				},
				"entityId": map[string]any{"type": "string", "description": "Existing entity ID to update; omit to create."},
			},
			"required": []string{"type", "name"},
		},
	},
	{
		"type":        "function",
		"name":        "entity_get",
		"description": "Fetch one memory entity by ID, with all stored facts and relationships.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"entityId": map[string]any{"type": "string", "description": "The entity's ID from a memory_search result."},
			},
			"required": []string{"entityId"},
		},
	},
	{
		"type": "function",
		"name": "plan_upsert",
		"description": "Create or update a multi-step plan in the user's long-term memory. The steps list replaces " +
			"any previous steps.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"planId": map[string]any{"type": "string", "description": "Existing plan's ID to update; omit to create."},
				"title":  map[string]any{"type": "string", "description": "The plan's title."},
				"steps": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "The full ordered list of steps.",
				},
			},
			"required": []string{"title", "steps"},
		},
	},
	{
		"type": "function",
		"name": "forget",
		"description": "Permanently delete one memory entity at the user's explicit request. Only call when the " +
			"user asks you to forget something.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"entityId": map[string]any{"type": "string", "description": "The ID of the entity to forget."},
			},
			"required": []string{"entityId"},
		},
	},
	{
		"type": "function",
		"name": "web_research",
		"description": "Research a topic with a recency filter: recent items with publication dates plus " +
			"encyclopedic background. Use for time-sensitive questions and always cite source dates.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "The topic to research."},
				"days": map[string]any{
					"type": "integer", "minimum": 1, "maximum": 365,
					"description": "Only include items newer than this many days (default 30).",
				},
				"url": map[string]any{
					"type":        "string",
					"description": "Optional exact https URL to fetch directly; only allow-listed authoritative domains (anthropic.com, openai.com).",
				},
			},
			"required": []string{"query"},
		},
	},
}

// toolManifestJSON is the manifest marshaled once at init (it is static).
var toolManifestJSON = func() json.RawMessage {
	b, err := json.Marshal(toolManifest)
	if err != nil {
		// Static data — a marshal failure is a programming error.
		panic(fmt.Sprintf("realtime: marshal tool manifest: %v", err))
	}
	return b
}()

// ToolManifestJSON returns the static OpenAI function-tool manifest bound
// into every minted session (also returned verbatim to clients).
func ToolManifestJSON() json.RawMessage { return toolManifestJSON }

// ClientSecret is the minted ephemeral credential returned to clients.
type ClientSecret struct {
	Value     string `json:"value"`
	ExpiresAt string `json:"expiresAt"` // RFC3339 UTC
}

// MintResult is the broker's session-mint payload: everything a client
// needs to open its direct WebRTC/WSS session with OpenAI.
type MintResult struct {
	ClientSecret  ClientSecret
	Model         string
	Voice         string
	SessionConfig json.RawMessage
	ToolManifest  json.RawMessage
}

// Minter mints config-bound OpenAI Realtime ephemeral tokens. The OpenAI
// API key is resolved per-call through the SSM-backed config.Loader
// (cached 5 min) — it never appears in an env var of a deployed function.
type Minter struct {
	httpc  *http.Client
	loader *config.Loader
	model  string
}

// NewMinter builds a Minter. model comes from OPENAI_REALTIME_MODEL
// (default DefaultRealtimeModel).
func NewMinter(loader *config.Loader, model string) *Minter {
	if model == "" {
		model = DefaultRealtimeModel
	}
	return &Minter{
		httpc:  &http.Client{Timeout: 10 * time.Second},
		loader: loader,
		model:  model,
	}
}

// Model returns the realtime model this Minter binds into sessions.
func (m *Minter) Model() string { return m.model }

// Mint resolves the persona server-side and POSTs to OpenAI's
// client_secrets endpoint for a ~60s ephemeral token whose session config
// (model, voice, instructions, tools, semantic-VAD barge-in) is fixed at
// mint time. instructionsSuffix is appended verbatim to the persona's
// instructions — the broker passes the enabled-guide injection block
// (guides.go, FR-MEM-07); it is always server-derived, never client input.
// The caller (broker handler) runs the quota gate BEFORE calling this —
// Mint itself performs no quota checks.
func (m *Minter) Mint(ctx context.Context, personaID, voice, instructionsSuffix string) (*MintResult, error) {
	persona := ResolvePersona(personaID)

	sessionConfig := map[string]any{
		"type":  "realtime",
		"model": m.model,
		"audio": map[string]any{
			"output": map[string]any{"voice": voice},
			// GA realtime API nests turn detection under audio.input —
			// a top-level session.turn_detection is rejected with 400
			// "Unknown parameter" (broke every mint in prod 2026-07-18).
			"input": map[string]any{
				"turn_detection": map[string]any{
					"type":               "semantic_vad",
					"interrupt_response": true,
				},
			},
		},
		"instructions": persona.Instructions + instructionsSuffix,
		"tools":        toolManifest,
	}
	body, err := json.Marshal(map[string]any{
		"expires_after": map[string]any{
			"anchor":  "created_at",
			"seconds": ephemeralTTLSeconds,
		},
		"session": sessionConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("realtime: marshal mint request: %w", err)
	}

	apiKey, err := m.loader.Get(ctx, config.ParamOpenAIAPIKey, config.EnvOverrideOpenAIAPIKey)
	if err != nil {
		return nil, fmt.Errorf("realtime: resolve openai key: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, clientSecretsURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("realtime: build mint request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("realtime: mint request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("realtime: read mint response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("realtime: openai client_secrets returned %d: %s",
			resp.StatusCode, truncate(string(respBody), 500))
	}

	var out struct {
		Value     string `json:"value"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("realtime: decode mint response: %w", err)
	}
	if out.Value == "" {
		return nil, fmt.Errorf("realtime: openai client_secrets response missing value")
	}

	expiresAt := time.Unix(out.ExpiresAt, 0).UTC()
	if out.ExpiresAt == 0 {
		expiresAt = time.Now().UTC().Add(ephemeralTTLSeconds * time.Second)
	}

	cfgJSON, err := json.Marshal(sessionConfig)
	if err != nil {
		return nil, fmt.Errorf("realtime: marshal session config: %w", err)
	}

	return &MintResult{
		ClientSecret: ClientSecret{
			Value:     out.Value,
			ExpiresAt: expiresAt.Format(time.RFC3339),
		},
		Model:         m.model,
		Voice:         voice,
		SessionConfig: cfgJSON,
		ToolManifest:  toolManifestJSON,
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
