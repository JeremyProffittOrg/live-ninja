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
	// gpt-realtime-mini was a MODEL ID with no rate row, so every
	// openai-realtime-mini session was priced at full gpt-realtime rates by
	// the defaultRates fallback below — a live billing defect in the cost
	// badge, not new work (azure-voice-plan.md R10). Adding the row makes the
	// existing mini engine read correctly cheaper.
	"gpt-realtime-mini": {
		TextInPer1M:        0.60,
		TextOutPer1M:       2.40,
		AudioInPer1M:       10.00,
		AudioOutPer1M:      20.00,
		CachedTextInPer1M:  0.06,
		CachedAudioInPer1M: 0.30,
	},
	// Azure OpenAI Realtime, keyed on the DEPLOYMENT names WS-A M3 actually
	// created, because the deployment name is what the broker sends as
	// `model` and therefore what RatesFor is called with. Keying these on the
	// dotted model ids would miss every lookup and silently re-create the
	// defect above (gap register W6).
	// Azure list rates per 1M tokens, carried from azure-migration-plan.md
	// WS-B M4 (read 2026-08-18); gpt-realtime-2.1 matches gpt-realtime-2.
	"gpt-realtime-2-1": {
		TextInPer1M:        4.00,
		TextOutPer1M:       24.00,
		AudioInPer1M:       32.00,
		AudioOutPer1M:      64.00,
		CachedTextInPer1M:  0.40,
		CachedAudioInPer1M: 0.40,
	},
	"gpt-realtime-2-1-mini": {
		TextInPer1M:        0.60,
		TextOutPer1M:       2.40,
		AudioInPer1M:       10.00,
		AudioOutPer1M:      20.00,
		CachedTextInPer1M:  0.06,
		CachedAudioInPer1M: 0.30,
	},
}

// ratesMissing names models that are deliberately shipped WITHOUT a rate row,
// because no rate has been published for them. RatesForModel reports these as
// "unknown" so the cost badge can be suppressed, rather than letting
// defaultRates quietly present gpt-realtime prices as if they were measured.
//
// azure-realtime and phi4-mm-realtime are Azure AI Voice Live models. Voice
// Live bills by tier (Pro/Basic/Lite) and azure-realtime appears in the
// supported-models table but in none of the published tier rows, so there is
// no number to put here that would not be invented.
var ratesMissing = map[string]bool{
	"azure-realtime":   true,
	"phi4-mm-realtime": true,
}

// defaultRates backstops any model id not (yet) listed in modelRates —
// e.g. a future/mini variant — so the badge still renders an estimate
// instead of silently going blank.
var defaultRates = modelRates["gpt-realtime"]

// RatesFor returns the per-1M-token rate table for model, falling back to
// the gpt-realtime rates for unknown or future model names.
//
// Prefer RatesForModel on any new call site. This fallback is what made R10
// invisible: a model with no row was billed at full gpt-realtime rates and
// nothing anywhere said so.
func RatesFor(model string) Rates {
	if r, ok := modelRates[model]; ok {
		return r
	}
	return defaultRates
}

// RatesForModel returns the rate table for model and whether it is a real,
// published rate. ok=false means the caller must suppress the cost badge
// rather than render an estimate — either the model is on the ratesMissing
// list, or it has no row at all and would otherwise be silently priced as
// gpt-realtime (azure-voice-plan.md WS-B M5).
func RatesForModel(model string) (Rates, bool) {
	if ratesMissing[model] {
		return Rates{}, false
	}
	r, ok := modelRates[model]
	if !ok {
		return defaultRates, false
	}
	return r, true
}
