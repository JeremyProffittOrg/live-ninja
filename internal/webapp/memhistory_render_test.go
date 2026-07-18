package webapp

// Render smoke tests for the M10 /memory and M11 /history client-rendered
// shells (same pattern as downloads_render_tmp_test.go): both pages
// execute against a nil bind, carry their page metadata, and contain the
// ids their controllers (memory.mjs / history.mjs) wire up, including the
// explicit loading/empty/error state containers.

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderMemoryPage(t *testing.T) {
	_, rend := newTestShell(t)
	var buf bytes.Buffer
	if err := rend.Render(&buf, "pages/memory", nil); err != nil {
		t.Fatalf("render memory: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"<title>Memory — Live Ninja</title>",
		`id="memTypeFilter"`, // populated entity-type select, never free text
		`id="memBody"`,
		`id="memLoading"`,
		`id="memEmpty"`,
		`id="memError"`,
		`id="entityDialog"`,
		`id="guideList"`,
		`id="guideDialog"`,
		`href="/memory" aria-current="page"`,
		"memory.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("memory output missing %q", want)
		}
	}
}

func TestRenderHistoryPage(t *testing.T) {
	_, rend := newTestShell(t)
	var buf bytes.Buffer
	if err := rend.Render(&buf, "pages/history", nil); err != nil {
		t.Fatalf("render history: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"<title>History — Live Ninja</title>",
		`id="topicChips"`,   // topic multi-select populated from taxonomy
		`id="deviceFilter"`, // device picker populated from /api/v1/devices
		`id="fromDate"`,
		`id="toDate"`,
		`id="histBody"`,
		`id="histLoading"`,
		`id="histEmpty"`,
		`id="histError"`,
		`id="histDetailView"`,
		`id="topicManager"`,
		`href="/history" aria-current="page"`,
		"history.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("history output missing %q", want)
		}
	}
}
