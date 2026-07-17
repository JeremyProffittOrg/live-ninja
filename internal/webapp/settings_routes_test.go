package webapp

import (
	"bytes"
	"encoding/json"
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
		Personas []map[string]any `json:"personas"`
	}
	if err := json.Unmarshal([]byte(catalogIsland), &cat); err != nil {
		t.Fatalf("catalogs-data island is not valid JSON: %v", err)
	}
	if len(cat.Voices) != 10 {
		t.Errorf("catalog voices = %d, want 10", len(cat.Voices))
	}
	if len(cat.Personas) == 0 {
		t.Errorf("catalog personas empty")
	}

	// SSR'd control states.
	for _, want := range []string{
		`name="voice" value="marin" checked`,
		`name="theme" value="light" checked`,
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

// TestSettingsPageUnknownVoiceFallsBack: an unrecognized stored voice
// renders with cedar selected in the UI but stays verbatim in the island
// (settings.schema.json forward-compat rule).
func TestSettingsPageUnknownVoiceFallsBack(t *testing.T) {
	doc := store.DefaultSettings()
	doc["voice"] = "future-voice-x"

	view, err := buildSettingsPageView(doc)
	if err != nil {
		t.Fatalf("buildSettingsPageView: %v", err)
	}
	var cedarSelected bool
	for _, v := range view.Voices {
		if v.ID == "cedar" && v.Selected {
			cedarSelected = true
		}
		if v.Selected && v.ID != "cedar" {
			t.Errorf("unexpected selected voice row %q", v.ID)
		}
	}
	if !cedarSelected {
		t.Errorf("unknown voice should select cedar in the UI")
	}
	if !strings.Contains(string(view.SettingsJSON), "future-voice-x") {
		t.Errorf("stored voice must be preserved verbatim in the data island")
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
		{"bad voiceEngine pin", func(d map[string]any) {
			d["voiceEngine"] = map[string]any{"default": "openai-realtime", "devices": map[string]any{"dev1": "cassette"}}
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
