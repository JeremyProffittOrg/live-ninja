package webapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// TestSettingsPageRenders proves the full SSR path: the settings page
// template set (layouts/base + partials/nav + pages/settings) parses
// from the embedded FS and executes with a real default document, and —
// critically — the <script type="application/json"> data island survives
// html/template's contextual escaping as byte-for-byte parseable JSON
// (json.Marshal HTML-escapes <,>,& so raw emission is safe; this test
// is the guard that stays true if the template or marshaling changes).
func TestSettingsPageRenders(t *testing.T) {
	doc := store.DefaultSettings()
	doc["voice"] = "marin"
	doc["theme"] = "light"
	doc["persona"] = map[string]any{"presetId": "custom", "systemInstructions": "Be <brief> & \"kind\"."}
	doc["privacy"] = map[string]any{"storeAudio": true, "storeTranscripts": true, "retentionDays": 7}

	view, err := buildSettingsPageView(doc)
	if err != nil {
		t.Fatalf("buildSettingsPageView: %v", err)
	}

	_, rend := newTestShell(t)
	var buf bytes.Buffer
	if err := rend.Render(&buf, "pages/settings", view); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	// An explicit stored theme SSRs onto <html> (themeAttr reflection).
	if !strings.Contains(html, `<html lang="en" data-theme="light">`) {
		t.Errorf("expected data-theme=light on <html> for theme=light")
	}

	// The JSON island must round-trip.
	island := extractBetween(t, html, `<script type="application/json" id="settings-data">`, `</script>`)
	var back map[string]any
	if err := json.Unmarshal([]byte(island), &back); err != nil {
		t.Fatalf("settings-data island is not valid JSON after templating: %v\n%s", err, island)
	}
	if back["voice"] != "marin" {
		t.Errorf("island voice = %v, want marin", back["voice"])
	}
	persona, _ := back["persona"].(map[string]any)
	if persona == nil || persona["systemInstructions"] != `Be <brief> & "kind".` {
		t.Errorf("island persona did not round-trip: %v", back["persona"])
	}

	catalogIsland := extractBetween(t, html, `<script type="application/json" id="catalogs-data">`, `</script>`)
	var cat struct {
		Voices   []map[string]any `json:"voices"`
		Accents  []map[string]any `json:"accents"`
		Personas []map[string]any `json:"personas"`
	}
	if err := json.Unmarshal([]byte(catalogIsland), &cat); err != nil {
		t.Fatalf("catalogs-data island is not valid JSON: %v", err)
	}
	if len(cat.Voices) != 10 {
		t.Errorf("catalog voices = %d, want 10", len(cat.Voices))
	}
	for _, vc := range cat.Voices {
		if g, _ := vc["gender"].(string); g != "female" && g != "male" && g != "neutral" {
			t.Errorf("catalog voice %v gender = %q, want female|male|neutral", vc["id"], g)
		}
	}
	if len(cat.Accents) != 10 {
		t.Errorf("catalog accents = %d, want 10", len(cat.Accents))
	}
	if len(cat.Personas) == 0 {
		t.Errorf("catalog personas empty")
	}

	// SSR'd control states. The standalone Voice/Accent section is gone —
	// personas are the unit of voice identity (personaPrefs; edited in the
	// conversation page's persona editor) — so the page must NOT render
	// voice controls but MUST point at the editor from the Persona section.
	for _, notWant := range []string{`name="voice"`, `id="voiceGenderChips"`, `id="voiceAccent"`} {
		if strings.Contains(html, notWant) {
			t.Errorf("rendered page still contains removed voice control %q", notWant)
		}
	}
	for _, want := range []string{
		`Each persona carries its own voice and accent`,
		`name="theme" value="light" checked`,
		`name="liveStyle" value="hal9000" checked`,
		`name="appStyle" value="ninja" checked`,
		`name="retentionDays" value="7" checked`,
		`id="storeAudio" aria-describedby="storeAudioNote" checked`,
		`value="custom" selected`,
		`aria-current="page"`, // nav marks /settings
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
	if strings.Contains(html, `id="customInstructionsField" hidden`) {
		t.Errorf("custom instructions field should be visible for presetId=custom")
	}
}

// TestSettingsPageVoicePreservedInIsland: with no voice controls on the
// page anymore, the stored top-level voice (the account fallback default)
// and the personaPrefs map must still ride the SettingsJSON island so
// write-backs preserve them verbatim (schema forward-compat rule).
func TestSettingsPageVoicePreservedInIsland(t *testing.T) {
	doc := store.DefaultSettings()
	doc["voice"] = "future-voice-x"
	doc["personaPrefs"] = map[string]any{
		"noir-detective": map[string]any{"voice": "ash", "accent": "irish"},
	}

	view, err := buildSettingsPageView(doc)
	if err != nil {
		t.Fatalf("buildSettingsPageView: %v", err)
	}
	island := string(view.SettingsJSON)
	for _, want := range []string{"future-voice-x", "personaPrefs", "noir-detective"} {
		if !strings.Contains(island, want) {
			t.Errorf("data island missing %q", want)
		}
	}
}

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

func extractBetween(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("marker %q not found in rendered page", start)
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("closing marker %q not found", end)
	}
	return rest[:j]
}
