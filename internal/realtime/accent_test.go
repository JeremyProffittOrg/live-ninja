package realtime

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TestVoiceCatalogGenderTags: every supported voice carries a perceived
// gender tag the settings filter chips can bucket on.
func TestVoiceCatalogGenderTags(t *testing.T) {
	if len(SupportedVoices) != 10 {
		t.Fatalf("SupportedVoices = %d entries, want 10 (schema enum)", len(SupportedVoices))
	}
	for _, v := range SupportedVoices {
		switch v.Gender {
		case "female", "male", "neutral":
		default:
			t.Errorf("voice %q gender = %q, want female|male|neutral", v.ID, v.Gender)
		}
	}
}

// TestAccentCatalogAndDirectivesInSync enforces the catalog.go/mint.go
// pairing: every non-none accent has a directive, every directive is
// reachable from the catalog, and "none" leads the list as the default.
func TestAccentCatalogAndDirectivesInSync(t *testing.T) {
	if len(SupportedAccents) == 0 || SupportedAccents[0].ID != "none" || SupportedAccents[0].Label != "Default" {
		t.Fatalf("SupportedAccents must start with {none, Default}, got %+v", SupportedAccents)
	}
	seen := map[string]bool{}
	for _, a := range SupportedAccents {
		if a.ID == "" || a.Label == "" {
			t.Errorf("accent %+v has empty id/label", a)
		}
		if seen[a.ID] {
			t.Errorf("duplicate accent id %q", a.ID)
		}
		seen[a.ID] = true
		if a.ID == "none" {
			continue
		}
		if d, ok := accentDirectives[a.ID]; !ok || strings.TrimSpace(d) == "" {
			t.Errorf("accent %q has no directive in accentDirectives", a.ID)
		}
	}
	for id := range accentDirectives {
		if !seen[id] {
			t.Errorf("directive %q is not in SupportedAccents", id)
		}
	}
}

func TestIsSupportedAccent(t *testing.T) {
	for _, ok := range []string{"", "none", "irish", "new-york"} {
		if !IsSupportedAccent(ok) {
			t.Errorf("IsSupportedAccent(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"martian", "IRISH", "irish "} {
		if IsSupportedAccent(bad) {
			t.Errorf("IsSupportedAccent(%q) = true, want false", bad)
		}
	}
}

func TestAccentDirective(t *testing.T) {
	got := AccentDirective("irish")
	if !strings.HasPrefix(got, "\n\n") || !strings.Contains(got, "Irish accent") {
		t.Errorf("AccentDirective(irish) = %q, want a \\n\\n-prefixed Irish directive", got)
	}
	for _, none := range []string{"", "none", "future-accent-x"} {
		if d := AccentDirective(none); d != "" {
			t.Errorf("AccentDirective(%q) = %q, want \"\"", none, d)
		}
	}
}

// fakeAccentSettingsGetter is a scripted SettingsGetter for settings-read
// resolution tests (accent + voiceprefs_test.go's ResolveSessionVoice).
type fakeAccentSettingsGetter struct {
	item map[string]ddbtypes.AttributeValue
	err  error
}

func (f *fakeAccentSettingsGetter) GetItem(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &dynamodb.GetItemOutput{Item: f.item}, nil
}
