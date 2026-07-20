package webapp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

func TestValidateAndNormalizeSettings(t *testing.T) {
	valid := func() map[string]any {
		d := store.DefaultSettings()
		return d
	}

	if msg := validateAndNormalizeSettings(valid()); msg != "" {
		t.Fatalf("default document should validate, got %q", msg)
	}

	cases := []struct {
		name   string
		mutate func(map[string]any)
		wantOK bool
	}{
		{"sensitivity out of range", func(d map[string]any) { d["sensitivity"] = 1.5 }, false},
		{"bad theme", func(d map[string]any) { d["theme"] = "hotdog" }, false},
		{"bad turn detection", func(d map[string]any) { d["turnDetection"] = "psychic" }, false},
		{"bad wake engine", func(d map[string]any) { d["wakeEngine"] = "ears" }, false},
		{"empty wake word", func(d map[string]any) { d["wakeWord"] = " " }, false},
		{"bad retention", func(d map[string]any) {
			d["privacy"] = map[string]any{"retentionDays": float64(45)}
		}, false},
		{"retention as json float", func(d map[string]any) {
			d["privacy"] = map[string]any{"retentionDays": float64(90)}
		}, true},
		{"forward-compat voice allowed", func(d map[string]any) { d["voice"] = "future-voice-x" }, true},
		{"unknown top-level field preserved", func(d map[string]any) { d["someFutureField"] = "kept" }, true},
		{"instructions too long", func(d map[string]any) {
			d["persona"] = map[string]any{"presetId": "custom", "systemInstructions": strings.Repeat("a", 4001)}
		}, false},
		{"micDeviceId null ok", func(d map[string]any) { d["micDeviceId"] = nil }, true},
		{"micDeviceId number bad", func(d map[string]any) { d["micDeviceId"] = 7.0 }, false},
		{"bad liveStyle", func(d map[string]any) {
			d["appearance"] = map[string]any{"liveStyle": "vaporwave"}
		}, false},
		{"bad appStyle", func(d map[string]any) {
			d["appearance"] = map[string]any{"appStyle": "vaporwave"}
		}, false},
		{"bad accent", func(d map[string]any) {
			d["appearance"] = map[string]any{"accentColor": "red"}
		}, false},
		{"bad voiceEngine pin", func(d map[string]any) {
			d["voiceEngine"] = map[string]any{"default": "openai-realtime", "devices": map[string]any{"dev1": "cassette"}}
		}, false},
		{"gemini engine default pin ok", func(d map[string]any) {
			d["voiceEngine"] = map[string]any{"default": "gemini-flash-live", "devices": map[string]any{}}
		}, true},
		{"gemini engine device pin ok", func(d map[string]any) {
			d["voiceEngine"] = map[string]any{"default": "openai-realtime", "devices": map[string]any{"dev1": "gemini-flash-live"}}
		}, true},
		{"geminiVoice catalog value ok", func(d map[string]any) { d["geminiVoice"] = "Kore" }, true},
		{"geminiVoice forward-compat ok", func(d map[string]any) { d["geminiVoice"] = "future-gemini-voice" }, true},
		{"geminiVoice number bad", func(d map[string]any) { d["geminiVoice"] = 3.0 }, false},
		{"geminiVoice too long", func(d map[string]any) { d["geminiVoice"] = strings.Repeat("a", 65) }, false},
		{"voiceAccent irish ok", func(d map[string]any) { d["voiceAccent"] = "irish" }, true},
		{"voiceAccent forward-compat ok", func(d map[string]any) { d["voiceAccent"] = "future-accent-x" }, true},
		{"voiceAccent number bad", func(d map[string]any) { d["voiceAccent"] = 3.0 }, false},
		{"voiceAccent too long", func(d map[string]any) { d["voiceAccent"] = strings.Repeat("a", 65) }, false},
		{"personaPrefs valid entry", func(d map[string]any) {
			d["personaPrefs"] = map[string]any{
				"valley-girl": map[string]any{"voice": "coral", "accent": "irish", "updatedAt": "2026-07-18T00:00:00Z"},
			}
		}, true},
		{"personaPrefs forward-compat voice/extra field ok", func(d map[string]any) {
			d["personaPrefs"] = map[string]any{
				"x": map[string]any{"voice": "future-voice-x", "futureField": true},
			}
		}, true},
		{"personaPrefs not an object", func(d map[string]any) { d["personaPrefs"] = "cedar" }, false},
		{"personaPrefs entry not an object", func(d map[string]any) {
			d["personaPrefs"] = map[string]any{"x": "cedar"}
		}, false},
		{"personaPrefs voice not a string", func(d map[string]any) {
			d["personaPrefs"] = map[string]any{"x": map[string]any{"voice": 7.0}}
		}, false},
		{"personaPrefs accent too long", func(d map[string]any) {
			d["personaPrefs"] = map[string]any{"x": map[string]any{"accent": strings.Repeat("a", 65)}}
		}, false},
		{"personaPrefs empty key bad", func(d map[string]any) {
			d["personaPrefs"] = map[string]any{" ": map[string]any{"voice": "cedar"}}
		}, false},
	}
	for _, tc := range cases {
		d := valid()
		tc.mutate(d)
		msg := validateAndNormalizeSettings(d)
		if tc.wantOK && msg != "" {
			t.Errorf("%s: want valid, got %q", tc.name, msg)
		}
		if !tc.wantOK && msg == "" {
			t.Errorf("%s: want rejection, got valid", tc.name)
		}
	}

	// Non-custom preset clears instructions (normalization, not rejection).
	d := valid()
	d["persona"] = map[string]any{"presetId": "default", "systemInstructions": "stale text"}
	if msg := validateAndNormalizeSettings(d); msg != "" {
		t.Fatalf("normalization case should validate, got %q", msg)
	}
	if got := d["persona"].(map[string]any)["systemInstructions"]; got != nil {
		t.Errorf("instructions should be nulled for non-custom preset, got %v", got)
	}

	// A legacy client's single themeStyle migrates to liveStyle (it styled
	// the conversation live panel) and the deprecated key is dropped;
	// appStyle normalizes to its default.
	d = valid()
	d["appearance"] = map[string]any{"themeStyle": "minimal", "accentColor": ""}
	if msg := validateAndNormalizeSettings(d); msg != "" {
		t.Fatalf("legacy appearance should validate, got %q", msg)
	}
	ap := d["appearance"].(map[string]any)
	if ap["liveStyle"] != "minimal" || ap["appStyle"] != "ninja" {
		t.Errorf("legacy themeStyle migration wrong: %v", ap)
	}
	if _, has := ap["themeStyle"]; has {
		t.Errorf("deprecated themeStyle key must be dropped on write")
	}

	// voiceAccent normalization: absent -> "", the catalog's "none" id ->
	// its stored form "".
	d = valid()
	delete(d, "voiceAccent")
	if msg := validateAndNormalizeSettings(d); msg != "" {
		t.Fatalf("absent voiceAccent should validate, got %q", msg)
	}
	if got := d["voiceAccent"]; got != "" {
		t.Errorf("absent voiceAccent should normalize to \"\", got %v", got)
	}
	d = valid()
	d["voiceAccent"] = "none"
	if msg := validateAndNormalizeSettings(d); msg != "" {
		t.Fatalf("voiceAccent none should validate, got %q", msg)
	}
	if got := d["voiceAccent"]; got != "" {
		t.Errorf("voiceAccent \"none\" should normalize to \"\", got %v", got)
	}

	// geminiVoice normalization: absent -> "" (unset; the broker's Gemini
	// chain falls through to the persona's voice, then Kore).
	d = valid()
	delete(d, "geminiVoice")
	if msg := validateAndNormalizeSettings(d); msg != "" {
		t.Fatalf("absent geminiVoice should validate, got %q", msg)
	}
	if got := d["geminiVoice"]; got != "" {
		t.Errorf("absent geminiVoice should normalize to \"\", got %v", got)
	}

	// personaPrefs normalization: absent -> {}, entry accent "none" -> "".
	d = valid()
	delete(d, "personaPrefs")
	if msg := validateAndNormalizeSettings(d); msg != "" {
		t.Fatalf("absent personaPrefs should validate, got %q", msg)
	}
	if pp, ok := d["personaPrefs"].(map[string]any); !ok || len(pp) != 0 {
		t.Errorf("absent personaPrefs should normalize to {}, got %v", d["personaPrefs"])
	}
	d = valid()
	d["personaPrefs"] = map[string]any{"zen-monk": map[string]any{"voice": "sage", "accent": "none"}}
	if msg := validateAndNormalizeSettings(d); msg != "" {
		t.Fatalf("personaPrefs accent none should validate, got %q", msg)
	}
	if got := d["personaPrefs"].(map[string]any)["zen-monk"].(map[string]any)["accent"]; got != "" {
		t.Errorf("entry accent \"none\" should normalize to \"\", got %v", got)
	}

	// personaPrefs cap: beyond maxPersonaPrefs entries the oldest-updated
	// are pruned first (missing updatedAt counts oldest), newest survive.
	d = valid()
	big := map[string]any{}
	for i := 0; i < maxPersonaPrefs+10; i++ {
		big[fmt.Sprintf("persona-%03d", i)] = map[string]any{
			"voice":     "cedar",
			"updatedAt": fmt.Sprintf("2026-07-%02dT00:00:00Z", 1+i%28),
		}
	}
	big["no-timestamp"] = map[string]any{"voice": "cedar"} // counts oldest
	d["personaPrefs"] = big
	if msg := validateAndNormalizeSettings(d); msg != "" {
		t.Fatalf("oversized personaPrefs should validate (prune, not reject), got %q", msg)
	}
	pp := d["personaPrefs"].(map[string]any)
	if len(pp) != maxPersonaPrefs {
		t.Errorf("personaPrefs pruned to %d entries, want %d", len(pp), maxPersonaPrefs)
	}
	if _, kept := pp["no-timestamp"]; kept {
		t.Errorf("entry without updatedAt must be pruned first")
	}

	// version is server-owned and must be stripped from the client doc.
	d = valid()
	d["version"] = 99
	if msg := validateAndNormalizeSettings(d); msg != "" {
		t.Fatalf("unexpected: %q", msg)
	}
	if _, present := d["version"]; present {
		t.Errorf("client-sent version must be stripped")
	}
}
