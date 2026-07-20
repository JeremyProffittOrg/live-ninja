package realtime

import "testing"

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
