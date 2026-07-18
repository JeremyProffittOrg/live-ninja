package realtime

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestPersonaPrefsKey(t *testing.T) {
	cases := []struct{ ref, want string }{
		{"default", "default"},
		{"valley-girl", "valley-girl"},
		{"custom", "custom"},
		{"", "default"},
		{"user:u1:my-persona", "my-persona"},
		{"shared:cool-persona", "cool-persona"},
		// Malformed refs fall back to the ref itself (never panic).
		{"user:u1", "user:u1"},
		{"user::", "user::"},
		{"shared:", "shared:"},
	}
	for _, tc := range cases {
		if got := PersonaPrefsKey(tc.ref); got != tc.want {
			t.Errorf("PersonaPrefsKey(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

// TestResolveVoiceChain pins the locked precedence:
// personaPrefs ?? suggested ?? top-level (override, then stored) ?? cedar,
// with unknown candidates falling through at every step.
func TestResolveVoiceChain(t *testing.T) {
	cases := []struct {
		name                           string
		pref, suggested, override, top string
		want                           string
	}{
		{"pref wins over everything", "shimmer", "coral", "ash", "ballad", "shimmer"},
		{"suggested when no pref", "", "coral", "ash", "ballad", "coral"},
		{"override when no pref/suggested", "", "", "ash", "ballad", "ash"},
		{"stored top-level last", "", "", "", "ballad", "ballad"},
		{"cedar floor", "", "", "", "", "cedar"},
		{"unknown pref falls through", "future-voice-x", "coral", "", "", "coral"},
		{"unknown suggested falls through", "", "not-a-voice", "", "ballad", "ballad"},
		{"all unknown falls to cedar", "future-voice-x", "??", "junk", "stale", "cedar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveVoiceChain(tc.pref, tc.suggested, tc.override, tc.top); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

func TestResolveAccentChain(t *testing.T) {
	cases := []struct {
		name      string
		pref      *string
		suggested string
		top       string
		want      string
	}{
		{"pref accent wins over suggested and top", strPtr("irish"), "british", "german", "irish"},
		{"present-empty pref means explicitly none", strPtr(""), "british", "german", ""},
		{"pref none normalizes to empty", strPtr("none"), "british", "german", ""},
		{"suggested accent seeds when pref absent", nil, "british", "german", "british"},
		{"absent pref+suggested falls to top-level", nil, "", "german", "german"},
		{"absent everything", nil, "", "", ""},
		{"top-level none normalizes", nil, "", "none", ""},
		{"unknown pref accent passes through (mints without directive)", strPtr("martian"), "british", "irish", "martian"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveAccentChain(tc.pref, tc.suggested, tc.top); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// voicePrefsItem builds a marshaled settings item for the fake getter.
func voicePrefsItem(t *testing.T, doc map[string]any) map[string]ddbtypes.AttributeValue {
	t.Helper()
	av, err := attributevalue.MarshalMap(doc)
	if err != nil {
		t.Fatalf("marshal settings item: %v", err)
	}
	return av
}

func TestResolveSessionVoice(t *testing.T) {
	ctx := context.Background()
	// Disable stored-persona resolution explicitly so the "user:..." ref
	// below never lazily self-wires a real AWS client (personas_store.go);
	// the ref then resolves to the default persona, which is irrelevant to
	// these assertions because the personaPrefs entry outranks it anyway.
	SetPersonaStore(nil, "")
	t.Cleanup(func() { SetPersonaStore(nil, "") })

	full := map[string]any{
		"voice":       "ballad",
		"voiceAccent": "german",
		"personaPrefs": map[string]any{
			"valley-girl": map[string]any{"voice": "shimmer", "accent": "irish", "updatedAt": "2026-07-18T00:00:00Z"},
			"zen-monk":    map[string]any{"voice": "echo", "accent": ""},
			"my-persona":  map[string]any{"voice": "marin", "accent": "scottish"},
		},
	}

	t.Run("persona pref wins for voice and accent", func(t *testing.T) {
		g := &fakeAccentSettingsGetter{item: voicePrefsItem(t, full)}
		sv := ResolveSessionVoice(ctx, g, "tbl", "u1", "valley-girl", "")
		if sv.Voice != "shimmer" || sv.AccentID != "irish" {
			t.Errorf("got %+v, want shimmer/irish", sv)
		}
	})

	t.Run("present-empty accent pref suppresses the top-level accent", func(t *testing.T) {
		g := &fakeAccentSettingsGetter{item: voicePrefsItem(t, full)}
		sv := ResolveSessionVoice(ctx, g, "tbl", "u1", "zen-monk", "")
		if sv.Voice != "echo" || sv.AccentID != "" {
			t.Errorf("got %+v, want echo with no accent (explicit none)", sv)
		}
	})

	t.Run("stored-persona ref keys by its bare persona id", func(t *testing.T) {
		g := &fakeAccentSettingsGetter{item: voicePrefsItem(t, full)}
		sv := ResolveSessionVoice(ctx, g, "tbl", "u1", "user:u1:my-persona", "")
		if sv.Voice != "marin" || sv.AccentID != "scottish" {
			t.Errorf("got %+v, want marin/scottish", sv)
		}
	})

	t.Run("no pref: persona suggested voice+accent beat the top-level fallback", func(t *testing.T) {
		g := &fakeAccentSettingsGetter{item: voicePrefsItem(t, full)}
		// noir-detective suggests voice "ash" + accent "new-york" (builtin
		// registry); no personaPrefs entry exists for it, so its embedded
		// identity applies — the persona switch implies voice AND accent
		// with no separate pick, beating the stored top-level german.
		sv := ResolveSessionVoice(ctx, g, "tbl", "u1", "noir-detective", "")
		if sv.Voice != "ash" {
			t.Errorf("voice = %q, want the persona's suggested ash", sv.Voice)
		}
		if sv.AccentID != "new-york" {
			t.Errorf("accent = %q, want the persona's suggested new-york", sv.AccentID)
		}
	})

	t.Run("missing document falls to persona suggestion then default", func(t *testing.T) {
		g := &fakeAccentSettingsGetter{item: nil}
		sv := ResolveSessionVoice(ctx, g, "tbl", "u1", "logic-officer", "")
		if sv.Voice != "alloy" || sv.AccentID != "" {
			t.Errorf("got %+v, want alloy with no accent", sv)
		}
	})

	t.Run("read error degrades to override then default", func(t *testing.T) {
		g := &fakeAccentSettingsGetter{err: errors.New("dynamo down")}
		sv := ResolveSessionVoice(ctx, g, "tbl", "u1", "unknown-persona-id", "verse")
		// unknown persona resolves to the default persona (suggested cedar)
		// — pref/suggested chain: "", cedar → cedar wins over the override
		// per the locked order (suggested outranks top-level).
		if sv.Voice != "cedar" {
			t.Errorf("voice = %q, want cedar", sv.Voice)
		}
		if sv.AccentID != "" {
			t.Errorf("accent = %q, want \"\"", sv.AccentID)
		}
	})

	t.Run("nil getter still yields a mintable voice", func(t *testing.T) {
		sv := ResolveSessionVoice(ctx, nil, "tbl", "u1", "", "")
		if sv.Voice != "cedar" || sv.AccentID != "" {
			t.Errorf("got %+v, want cedar with no accent", sv)
		}
	})
}
