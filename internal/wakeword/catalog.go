package wakeword

import (
	"strings"
	"sync"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// Catalog assembly (plan.md M6 distribution task): the response for
// GET /api/v1/wakeword is the static builtin list plus the caller's own
// custom models, wrapped with honest engine/platform capability flags.
//
// NOTE on the public snapshot: contracts/api.md also names a public
// /static/wakewords/catalog.json S3/CloudFront snapshot of the BUILTIN
// half (no live table read on a public path). The builtin entries below
// are the single source of truth that snapshot is generated from — the
// authed endpoint here additionally merges the caller's customs, which
// by definition can never be in a shared public snapshot.

// CatalogEntry is one catalog row (builtin or custom).
type CatalogEntry struct {
	ID            string   `json:"id"`
	Phrase        string   `json:"phrase"`
	Engine        string   `json:"engine"` // openwakeword | wakenet
	Source        string   `json:"source"` // builtin | custom
	Status        string   `json:"status"` // ready | pending | training | failed
	Platforms     []string `json:"platforms"`
	CreatedAt     string   `json:"createdAt,omitempty"`
	ReadyAt       string   `json:"readyAt,omitempty"`
	FailureReason string   `json:"failureReason,omitempty"`
}

// EngineInfo advertises whether an engine can train custom phrases
// server-side, with the reason when it can't (honest capability flag —
// locked decision: never fake availability).
type EngineInfo struct {
	ID        string `json:"id"`
	Trainable bool   `json:"trainable"`
	Reason    string `json:"reason,omitempty"`
}

// Catalog is the GET /api/v1/wakeword response body.
type Catalog struct {
	Engines []EngineInfo   `json:"engines"`
	Entries []CatalogEntry `json:"entries"`
	// Esp32CustomSupported is false until an oWW-ESP conversion path
	// exists (plan.md M6 locked decision): the M5 device selects among
	// the builtin WakeNet entries below via the config shadow; custom
	// phrases are web/android only for now.
	Esp32CustomSupported bool `json:"esp32CustomSupported"`
}

// engines is the fixed engine-availability list. Porcupine training
// needs a Picovoice account (external dependency — documented deferral);
// WakeNet models are Espressif-curated, never trained here.
var engines = []EngineInfo{
	{ID: "openwakeword", Trainable: true},
	{ID: "porcupine", Trainable: false, Reason: "requires a Picovoice account (not configured)"},
	{ID: "wakenet", Trainable: false, Reason: "curated built-in ESP-SR models only"},
}

// builtinEntries: the shipped default model plus the curated flashable
// WakeNet set (FR-K05/FR-M02). "hey-live-ninja" is the default the
// web/android clients ship with (settings.schema.json default). The
// wn9_* ids are Espressif ESP-SR WakeNet9 models baked into the M5
// firmware model partition — wn9_hiesp is the flashed default per
// plan.md §8 M5 notes; the others are curated candidates the shadow's
// wakeModel field may select once flashed. Builtin model bytes are
// distributed with the client/firmware, NOT via the model endpoint.
var builtinEntries = []CatalogEntry{
	// "hey jarvis" is the openWakeWord model the web/android clients bundle
	// as their guaranteed fallback — reserved so training can't shadow it.
	// "hey live ninja" is deliberately NOT here: no client ships a model
	// for it, so it goes through the normal custom-training pipeline (the
	// manifest route resolves the bare "hey-live-ninja" settings id to the
	// trained item via slug match — see Model()).
	{ID: "hey-jarvis", Phrase: "hey jarvis", Engine: "openwakeword", Source: "builtin", Status: store.WakewordStatusReady, Platforms: []string{"web", "android"}},
	{ID: "wn9_hiesp", Phrase: "hi esp", Engine: "wakenet", Source: "builtin", Status: store.WakewordStatusReady, Platforms: []string{"esp32"}},
	{ID: "wn9_hilexin", Phrase: "hi lexin", Engine: "wakenet", Source: "builtin", Status: store.WakewordStatusReady, Platforms: []string{"esp32"}},
	{ID: "wn9_alexa", Phrase: "alexa", Engine: "wakenet", Source: "builtin", Status: store.WakewordStatusReady, Platforms: []string{"esp32"}},
}

// BuiltinEntry returns the builtin catalog entry with the given id, or
// nil. Copies so callers can't mutate the package list.
func BuiltinEntry(id string) *CatalogEntry {
	for _, e := range builtinEntries {
		if e.ID == id {
			c := e
			return &c
		}
	}
	return nil
}

// BuiltinEntries returns a copy of the builtin list.
func BuiltinEntries() []CatalogEntry {
	out := make([]CatalogEntry, len(builtinEntries))
	copy(out, builtinEntries)
	return out
}

// collidesWithBuiltin reports whether a normalized phrase (or its slug)
// collides with a builtin entry by id or normalized phrase.
func collidesWithBuiltin(normalized string) bool {
	slug := Slug(normalized)
	for _, e := range builtinEntries {
		if e.ID == slug || strings.EqualFold(e.Phrase, normalized) {
			return true
		}
	}
	return false
}

// entryFromItem converts a stored custom wake-word item to its catalog
// representation.
func entryFromItem(w *store.Wakeword) CatalogEntry {
	platforms := w.Platforms
	if platforms == nil {
		platforms = []string{}
	}
	return CatalogEntry{
		ID:            w.ID,
		Phrase:        w.Phrase,
		Engine:        w.Engine,
		Source:        "custom",
		Status:        w.Status,
		Platforms:     platforms,
		CreatedAt:     w.CreatedAt,
		ReadyAt:       w.ReadyAt,
		FailureReason: w.FailureReason,
	}
}

// catalogCache is the per-user 5-minute in-memory catalog cache
// (plan.md M6 role spec). Warm-container only — each Lambda container
// has its own copy; invalidated locally on any mutation by that
// container. TTL bounds staleness across containers.
type catalogCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]catalogCacheEntry
}

type catalogCacheEntry struct {
	catalog   *Catalog
	expiresAt time.Time
}

func newCatalogCache(ttl time.Duration) *catalogCache {
	return &catalogCache{ttl: ttl, entries: make(map[string]catalogCacheEntry)}
}

func (c *catalogCache) get(userID string, now time.Time) *Catalog {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[userID]
	if !ok || now.After(e.expiresAt) {
		delete(c.entries, userID)
		return nil
	}
	return e.catalog
}

func (c *catalogCache) put(userID string, cat *Catalog, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[userID] = catalogCacheEntry{catalog: cat, expiresAt: now.Add(c.ttl)}
}

func (c *catalogCache) invalidate(userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, userID)
}
