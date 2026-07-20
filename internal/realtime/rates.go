package realtime

// Rates holds per-1M-token USD list pricing for one realtime model, broken
// out by input/output and text/audio, plus cached-input rates. The
// session-cost badge (web/static/js/conversation.mjs) multiplies these
// against the token counts OpenAI reports on each `response.done` event —
// the numbers live here, server-side, so the client never hardcodes
// pricing that could silently drift from the model in use.
type Rates struct {
	TextInPer1M        float64 `json:"textInPer1M"`
	TextOutPer1M       float64 `json:"textOutPer1M"`
	AudioInPer1M       float64 `json:"audioInPer1M"`
	AudioOutPer1M      float64 `json:"audioOutPer1M"`
	CachedTextInPer1M  float64 `json:"cachedTextInPer1M"`
	CachedAudioInPer1M float64 `json:"cachedAudioInPer1M"`
}

// modelRates carries the gpt-realtime GA list pricing (OpenAI Realtime API,
// USD per 1,000,000 tokens), keyed by model id.
//
// NOTE: these are the best-known published OpenAI list prices at the time
// this badge was written (gpt-realtime GA, mirrors openai.com/api/pricing
// as of 2025-08); this repo has no pricing doc to source them from
// (docs/voice-engines.md deliberately avoids hard numbers — "provider
// pricing moves"). Treat the badge as an *estimate*, not a bill, and
// reconcile these against OpenAI's live pricing page if it starts looking
// off or when a new realtime model ships.
var modelRates = map[string]Rates{
	"gpt-realtime": {
		TextInPer1M:        4.00,
		TextOutPer1M:       16.00,
		AudioInPer1M:       32.00,
		AudioOutPer1M:      64.00,
		CachedTextInPer1M:  0.40,
		CachedAudioInPer1M: 0.40,
	},
	// Gemini Live list pricing (ai.google.dev/gemini-api/docs/pricing,
	// verified 2026-07-19; M13). Gemini Live has no input caching, so the
	// cached rates equal the uncached ones — a session with cache-shaped
	// usage numbers prices identically instead of silently discounting.
	"gemini-3.1-flash-live-preview": {
		TextInPer1M:        0.75,
		TextOutPer1M:       4.50,
		AudioInPer1M:       3.00,
		AudioOutPer1M:      12.00,
		CachedTextInPer1M:  0.75,
		CachedAudioInPer1M: 3.00,
	},
}

// defaultRates backstops any model id not (yet) listed in modelRates —
// e.g. a future/mini variant — so the badge still renders an estimate
// instead of silently going blank.
var defaultRates = modelRates["gpt-realtime"]

// RatesFor returns the per-1M-token rate table for model, falling back to
// the gpt-realtime rates for unknown or future model names.
func RatesFor(model string) Rates {
	if r, ok := modelRates[model]; ok {
		return r
	}
	return defaultRates
}
