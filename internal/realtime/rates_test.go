package realtime

import (
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

func TestRatesForKnownModel(t *testing.T) {
	r := RatesFor("gpt-realtime")
	if r.TextInPer1M <= 0 || r.TextOutPer1M <= 0 || r.AudioInPer1M <= 0 || r.AudioOutPer1M <= 0 {
		t.Fatalf("expected positive per-1M rates, got %+v", r)
	}
	if r.AudioInPer1M <= r.TextInPer1M {
		t.Errorf("expected audio input to be priced above text input: %+v", r)
	}
	if r.CachedTextInPer1M >= r.TextInPer1M {
		t.Errorf("expected cached text input to be cheaper than uncached: %+v", r)
	}
	if r.CachedAudioInPer1M >= r.AudioInPer1M {
		t.Errorf("expected cached audio input to be cheaper than uncached: %+v", r)
	}
}

func TestRatesForGeminiLiveModel(t *testing.T) {
	r := RatesFor("gemini-3.1-flash-live-preview")
	if r.AudioInPer1M != 3.00 || r.AudioOutPer1M != 12.00 || r.TextInPer1M != 0.75 || r.TextOutPer1M != 4.50 {
		t.Fatalf("gemini rates wrong: %+v", r)
	}
	// Gemini Live has no input caching — cached rates must equal uncached so
	// a cache-shaped usage report can't silently under-price the badge.
	if r.CachedTextInPer1M != r.TextInPer1M || r.CachedAudioInPer1M != r.AudioInPer1M {
		t.Fatalf("gemini cached rates must equal uncached: %+v", r)
	}
}

func TestRatesForUnknownModelFallsBack(t *testing.T) {
	got := RatesFor("some-future-realtime-model")
	want := RatesFor("gpt-realtime")
	if got != want {
		t.Errorf("unknown model = %+v, want fallback %+v", got, want)
	}
}

func TestRatesForEmptyModelFallsBack(t *testing.T) {
	got := RatesFor("")
	want := RatesFor("gpt-realtime")
	if got != want {
		t.Errorf("empty model = %+v, want fallback %+v", got, want)
	}
}

// TestRatesCoverEveryShippedEngine is the guard WS-B M5 exists to install.
// Every model id a shipped engine can send to RatesFor must either have an
// explicit rate row or be declared on ratesMissing. Neither is allowed to be
// the silent defaultRates fallback, which is what let openai-realtime-mini
// bill at full gpt-realtime rates unnoticed (R10).
func TestRatesCoverEveryShippedEngine(t *testing.T) {
	// The model id each engine actually puts on the wire as Response.Model.
	// Azure entries are DEPLOYMENT names (WS-A M3), not dotted model ids,
	// because the deployment name is what the broker sends.
	shipped := map[voiceengine.Engine]string{
		voiceengine.EngineOpenAIRealtime:     DefaultRealtimeModel,
		voiceengine.EngineOpenAIRealtimeMini: MiniRealtimeModel,
		voiceengine.EngineGeminiFlashLive:    "gemini-3.1-flash-live-preview",
		voiceengine.EngineGPTLiveAzure:       "gpt-realtime-2-1",
		voiceengine.EngineGPTLiveAzureMini:   "gpt-realtime-2-1-mini",
		voiceengine.EngineAzureVoiceLive:     "azure-realtime",
		voiceengine.EngineAzureVoiceLiveLite: "phi4-mm-realtime",
	}

	for engine, model := range shipped {
		_, explicit := modelRates[model]
		declaredMissing := ratesMissing[model]
		if !explicit && !declaredMissing {
			t.Errorf("engine %s sends model %q, which has no rate row and is not on ratesMissing — "+
				"it is being billed at gpt-realtime rates by the silent fallback", engine, model)
		}
		if explicit && declaredMissing {
			t.Errorf("model %q is both priced and declared missing; pick one", model)
		}

		_, ok := RatesForModel(model)
		if ok != explicit {
			t.Errorf("RatesForModel(%q) ok = %v, want %v", model, ok, explicit)
		}
	}

	// nova-sonic is deliberately absent: its usage events never reach the
	// client, so no badge is rendered for it.
	if _, present := shipped[voiceengine.EngineNovaSonic]; present {
		t.Error("nova-sonic should not be in the shipped-rate map")
	}
	// Every engine except nova-sonic must be covered, so a new engine
	// constant cannot ship without a pricing decision.
	for _, e := range voiceengine.All {
		if e == voiceengine.EngineNovaSonic {
			continue
		}
		if _, ok := shipped[e]; !ok {
			t.Errorf("engine %s ships but this test names no model for it — add its rate row", e)
		}
	}
}

// TestMiniModelIsPricedSeparately pins the R10 fix: the mini engine must no
// longer resolve to the full gpt-realtime rate table.
func TestMiniModelIsPricedSeparately(t *testing.T) {
	full := RatesFor(DefaultRealtimeModel)
	mini := RatesFor(MiniRealtimeModel)
	if mini == full {
		t.Fatal("gpt-realtime-mini is still priced at full gpt-realtime rates (R10)")
	}
	if mini.AudioInPer1M >= full.AudioInPer1M || mini.AudioOutPer1M >= full.AudioOutPer1M {
		t.Errorf("the mini model should be cheaper on audio: mini=%v full=%v", mini, full)
	}
}
