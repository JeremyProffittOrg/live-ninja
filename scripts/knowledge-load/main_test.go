package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitDocumentPrefixesEverySectionWithTitleAndContext(t *testing.T) {
	doc := "2027 Tahoe manual: fuel\nVehicle: Jeremy's Tahoe.\n\n## Fuel — octane\nUse 91.\n\n## Fuel — tank\n24.0 gal.\n"
	got := splitDocument(doc)
	if len(got) != 2 {
		t.Fatalf("want 2 records, got %d: %q", len(got), got)
	}
	for _, r := range got {
		if !strings.HasPrefix(r, "2027 Tahoe manual: fuel — Fuel — ") || !strings.Contains(r, "Vehicle: Jeremy's Tahoe.") {
			t.Errorf("record lacks title/context prefix: %q", r)
		}
	}
	if !strings.Contains(got[0], "Use 91.") || !strings.Contains(got[1], "24.0 gal.") {
		t.Errorf("section bodies misplaced: %q", got)
	}
}

func TestSplitDocumentBoundsLongSections(t *testing.T) {
	para := strings.Repeat("word ", 150) // ~750 runes
	doc := "Title\n\n## Big\n" + strings.Repeat(para+"\n\n", 6) + strings.Repeat("x", 5000)
	for i, r := range splitDocument(doc) {
		if n := utf8.RuneCountInString(r); n > maxRecordRunes {
			t.Errorf("record %d is %d runes, over %d", i, n, maxRecordRunes)
		}
		if !strings.HasPrefix(r, "Title — Big\n") {
			t.Errorf("record %d lost its prefix", i)
		}
	}
}

func TestValidCollection(t *testing.T) {
	for c, want := range map[string]bool{"tahoe-2027": true, "": false, "Tahoe": false, "a/b": false} {
		if validCollection(c) != want {
			t.Errorf("validCollection(%q) != %v", c, want)
		}
	}
}
