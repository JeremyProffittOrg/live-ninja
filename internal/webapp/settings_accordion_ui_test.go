package webapp

import (
	"strings"
	"testing"
)

// The accordion is a template/JS contract with no compiler between the two.
// Pin every trigger/panel pair and its initial accessible state so a markup
// cleanup cannot silently leave a section permanently hidden.
func TestConversationSettingsAccordionContract(t *testing.T) {
	html := readAsset(t, "templates/pages/conversation.html")
	sections := []struct {
		name string
		open bool
	}{
		{"About", true},
		{"WakeWord", false},
		{"Persona", false},
		{"VoiceEngine", false},
		{"Turn", false},
		{"Appearance", false},
		{"Mic", false},
		{"Privacy", false},
		{"Account", false},
	}

	if got := strings.Count(html, "data-settings-accordion-trigger"); got != len(sections) {
		t.Fatalf("settings accordion has %d triggers, want %d", got, len(sections))
	}

	for _, section := range sections {
		triggerID := "section" + section.name + "Trigger"
		panelID := "section" + section.name + "Panel"
		triggerAt := strings.Index(html, `id="`+triggerID+`"`)
		if triggerAt < 0 {
			t.Errorf("missing accordion trigger %s", triggerID)
			continue
		}
		trigger := html[triggerAt:min(len(html), triggerAt+500)]
		for _, want := range []string{
			"data-settings-accordion-trigger",
			`aria-controls="` + panelID + `"`,
			`aria-expanded="` + map[bool]string{true: "true", false: "false"}[section.open] + `"`,
		} {
			if !strings.Contains(trigger, want) {
				t.Errorf("%s missing %q", triggerID, want)
			}
		}

		panelAt := strings.Index(html, `id="`+panelID+`"`)
		if panelAt < 0 {
			t.Errorf("missing accordion panel %s", panelID)
			continue
		}
		panel := html[panelAt:min(len(html), panelAt+220)]
		for _, want := range []string{`role="region"`, `aria-labelledby="` + triggerID + `"`} {
			if !strings.Contains(panel, want) {
				t.Errorf("%s missing %q", panelID, want)
			}
		}
		if section.open == strings.Contains(panel, " hidden") {
			t.Errorf("%s hidden state does not match open=%v", panelID, section.open)
		}
	}
}

func TestSettingsEdgeBarsAndAccordionBehaviorAreShipped(t *testing.T) {
	html := readAsset(t, "templates/pages/conversation.html")
	css := readAsset(t, "static/css/app.css")
	js := readAsset(t, "static/js/settings-accordion.mjs")
	settingsJS := readAsset(t, "static/js/settings.mjs")

	for _, want := range []string{
		`aria-label="Open settings"`,
		`aria-controls="settingsDrawer"`,
		`id="settingsDrawerClose" aria-label="Close settings" autofocus`,
		`conv-settings-tab--close`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("conversation settings edge controls missing %q", want)
		}
	}
	for _, want := range []string{
		// The bar became a ~40px upper-left corner tab on 2026-08-01; its
		// geometry is pinned in mobile_shell_ui_test.go
		// (TestEdgeTabsSitInTheUpperLeft). Only its existence matters here.
		"height: var(--ln-edge-tab);",
		".conv-settings-tab--close",
		".set-accordion__trigger",
		".set-accordion__panel",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("settings styles missing %q", want)
		}
	}
	for _, want := range []string{
		"panel.hidden = !expanded",
		"candidate === trigger",
		"case 'ArrowDown':",
		"case 'ArrowUp':",
		"case 'Home':",
		"case 'End':",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("settings accordion behavior missing %q", want)
		}
	}
	if !strings.Contains(settingsJS, "Open settings — ${n} suggestion") {
		t.Error("suggestion count must be reflected in the opener's accessible name")
	}
}
