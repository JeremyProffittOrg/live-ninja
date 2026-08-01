package realtime

// Persona registry + stored-persona ref resolution tests (personas
// platform feature): the built-in seed set, the mint resolution order
// (built-in -> user's own -> shared catalog -> default), the shared-
// visibility re-check, and the structural no-Scan guarantee (the store
// seam only exposes GetItem — a fake records every call).

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fakePersonaGetter is a GetItem-only DynamoDB fake keyed on "pk|sk".
type fakePersonaGetter struct {
	items map[string]map[string]ddbtypes.AttributeValue
	calls []string // "pk|sk" per GetItem — proves key lookups only
}

func (f *fakePersonaGetter) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	pk := params.Key["pk"].(*ddbtypes.AttributeValueMemberS).Value
	sk := params.Key["sk"].(*ddbtypes.AttributeValueMemberS).Value
	f.calls = append(f.calls, pk+"|"+sk)
	if it, ok := f.items[pk+"|"+sk]; ok {
		return &dynamodb.GetItemOutput{Item: it}, nil
	}
	return &dynamodb.GetItemOutput{}, nil
}

func seedPersonaItem(f *fakePersonaGetter, pk, sk, name, instructions string, shared bool) {
	if f.items == nil {
		f.items = map[string]map[string]ddbtypes.AttributeValue{}
	}
	f.items[pk+"|"+sk] = map[string]ddbtypes.AttributeValue{
		"pk":           &ddbtypes.AttributeValueMemberS{Value: pk},
		"sk":           &ddbtypes.AttributeValueMemberS{Value: sk},
		"personaId":    &ddbtypes.AttributeValueMemberS{Value: strings.TrimPrefix(sk, "PERSONA#")},
		"name":         &ddbtypes.AttributeValueMemberS{Value: name},
		"instructions": &ddbtypes.AttributeValueMemberS{Value: instructions},
		"shared":       &ddbtypes.AttributeValueMemberBOOL{Value: shared},
	}
}

func withPersonaStore(t *testing.T, f *fakePersonaGetter) {
	t.Helper()
	SetPersonaStore(f, "live-ninja-test")
	t.Cleanup(func() { SetPersonaStore(nil, "") })
}

func TestBuiltinPersonaSeedSet(t *testing.T) {
	all := BuiltinPersonas()
	if len(all) < 11 {
		t.Fatalf("built-in registry has %d personas, want at least 11 (default + 10 new)", len(all))
	}
	if all[0].ID != "default" {
		t.Errorf("first builtin = %q, want default", all[0].ID)
	}

	// EVERY built-in, not a sample. docs/qa-report.md flagged the old
	// eleven-id sample as a coverage gap: a wrong voice on any unsampled
	// persona went unnoticed, and the registry has kept growing since.
	// Iterating the registry closes it for good — a persona added without a
	// blurb, without a valid voice, or with a style that fails to compose can
	// no longer slip in by simply not being listed here.
	ids := make([]string, 0, len(all))
	for _, p := range all {
		if p.ID != "default" { // default's style IS the core; asserted below
			ids = append(ids, p.ID)
		}
	}
	// The originals are named explicitly as well, so deleting one shows up as
	// a failure here rather than silently shrinking the loop above.
	for _, id := range []string{"valley-girl", "logic-officer", "deputy-chief",
		"noir-detective", "bard", "zen-monk", "drill-sergeant", "play-by-play",
		"butler", "surfer", "worried-grandma", "sommelier", "heh-heh-duo",
		"pirate-captain", "cool-intensity",
		"product-owner", "staff-developer", "staff-sre",
		"esp32-engineer", "esp32-s2-engineer", "esp32-s3-engineer",
		"esp32-c2-engineer", "esp32-c3-engineer", "esp32-c5-engineer",
		"esp32-c6-engineer", "esp32-h2-engineer", "esp32-p4-engineer"} {
		if !IsBuiltinPersona(id) {
			t.Errorf("built-in %q went missing from the registry", id)
		}
	}
	// Yoda was removed on owner request 2026-08-01; it must not come back
	// under its old id.
	if IsBuiltinPersona("swamp-master") {
		t.Error("the swamp-master (Yoda) persona was removed and must stay removed")
	}

	for _, id := range ids {
		if !IsBuiltinPersona(id) {
			t.Errorf("IsBuiltinPersona(%q) = false, want true", id)
		}
		p := ResolvePersona(id)
		if p.ID != id {
			t.Errorf("ResolvePersona(%q).ID = %q", id, p.ID)
		}
		// Every styled persona keeps the operational core (tool + safety
		// rules) underneath its style block.
		if !strings.Contains(p.Instructions, "Never claim a tool action happened") {
			t.Errorf("persona %q lost the operational core", id)
		}
		if p.Style == "" || !strings.Contains(p.Instructions, p.Style) {
			t.Errorf("persona %q instructions do not embed its style block", id)
		}
		if p.Voice == "" || !allowedRealtimeVoices[p.Voice] {
			t.Errorf("persona %q suggested voice %q is not a realtime voice", id, p.Voice)
		}
		if p.Description == "" {
			t.Errorf("persona %q has no description", id)
		}
		// The Gemini suggestion is hand-curated per persona (M13 D4b) and is
		// just as easy to typo as the OpenAI one.
		if p.GeminiVoice == "" || !allowedGeminiVoices[p.GeminiVoice] {
			t.Errorf("persona %q gemini voice %q is not a Gemini Live voice", id, p.GeminiVoice)
		}
	}

	// The catalog surface (settings/conversation pickers) lists every
	// built-in with its blurb (init() feeds personaDescriptions).
	infos := ListPersonas()
	if len(infos) != len(all) {
		t.Fatalf("ListPersonas() = %d entries, want %d", len(infos), len(all))
	}
	for _, info := range infos {
		if info.Description == "" {
			t.Errorf("catalog entry %q has no description", info.ID)
		}
	}
}

// TestWorkingPersonasPushBackWithoutTakingPower guards the three professional
// personas added on 2026-08-01 (owner request). They are a different class from
// the entertainment built-ins: their entire value is that they disagree with
// the owner out loud, and their entire risk is that a "senior engineer" voice
// is exactly the register in which a style block would try to legislate. This
// pins both halves.
func TestWorkingPersonasPushBackWithoutTakingPower(t *testing.T) {
	for _, id := range []string{"product-owner", "staff-developer", "staff-sre",
		"esp32-engineer", "esp32-s2-engineer", "esp32-s3-engineer",
		"esp32-c2-engineer", "esp32-c3-engineer", "esp32-c5-engineer",
		"esp32-c6-engineer", "esp32-h2-engineer", "esp32-p4-engineer"} {
		p := ResolvePersona(id)
		if p.ID != id {
			t.Fatalf("ResolvePersona(%q).ID = %q", id, p.ID)
		}
		lower := strings.ToLower(p.Style)

		// 1. It actually pushes back. A persona that only ever agrees is
		//    worth nothing on a design call, which is why it exists.
		if !strings.Contains(lower, "disagree") && !strings.Contains(lower, "contradict") &&
			!strings.Contains(lower, "wrong") {
			t.Errorf("persona %q has no explicit pushback behaviour", id)
		}

		// 2. It does not legislate. Style shapes delivery only: naming a tool
		//    or claiming a capability in this register would read to the model
		//    as policy rather than personality. composeStyle's framing sentence
		//    is the real guard; this catches the drafting mistake earlier.
		for _, forbidden := range []string{
			"send_email", "web_lookup", "memory_search", "code_update",
			"deliverable_", "file_read", "you may not", "you must not",
			"never call", "refuse to",
		} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("persona %q style contains policy/tool language %q", id, forbidden)
			}
		}

		// 3. It survives a spoken reply. The core caps answers at one to three
		//    sentences and forbids reading out markdown, so a style that asks
		//    for structured output fights the core it is layered on.
		// Word-boundary, not substring: "comfortable" contains "table", and a
		// plain Contains flagged the C2 persona for it.
		for _, forbidden := range []string{"bullet", "markdown", "table", "checklist", "numbered list"} {
			if regexp.MustCompile(`` + regexp.QuoteMeta(forbidden) + ``).MatchString(lower) {
				t.Errorf("persona %q style asks for %q, which cannot be spoken", id, forbidden)
			}
		}

		// 4. The operational core is still underneath it.
		if !strings.Contains(p.Instructions, "Never claim a tool action happened") {
			t.Errorf("persona %q lost the operational core", id)
		}
	}
}

func TestDefaultPersonaUnchanged(t *testing.T) {
	p := ResolvePersona("")
	if p.ID != "default" {
		t.Fatalf("empty id resolved to %q", p.ID)
	}
	if p.Instructions != coreInstructions {
		t.Errorf("default persona instructions changed from the operational core")
	}
	if ResolvePersona("no-such-persona").ID != "default" {
		t.Errorf("unknown id did not fall back to default")
	}
}

func TestResolutionOrderBuiltinWinsOverStore(t *testing.T) {
	// Even with a store installed, a built-in ID never touches DynamoDB.
	f := &fakePersonaGetter{}
	withPersonaStore(t, f)

	if got := ResolvePersona("valley-girl"); got.ID != "valley-girl" {
		t.Fatalf("builtin resolved to %q", got.ID)
	}
	if len(f.calls) != 0 {
		t.Errorf("builtin resolution hit the store: %v", f.calls)
	}
}

func TestResolveUserPersonaRef(t *testing.T) {
	f := &fakePersonaGetter{}
	seedPersonaItem(f, "USER#u1", "PERSONA#abc123", "Radio DJ", "Groovy 70s radio patter.", false)
	withPersonaStore(t, f)

	ref := UserPersonaRef("u1", "abc123")
	p := ResolvePersona(ref)
	if p.Name != "Radio DJ" {
		t.Fatalf("user ref resolved to %q (id=%s)", p.Name, p.ID)
	}
	// User-authored text is composed onto the operational core, framed as
	// style-only.
	if !strings.Contains(p.Instructions, "Groovy 70s radio patter.") ||
		!strings.HasPrefix(p.Instructions, coreInstructions) {
		t.Errorf("user persona instructions not composed on the core")
	}
	if len(f.calls) != 1 || f.calls[0] != "USER#u1|PERSONA#abc123" {
		t.Errorf("unexpected store access pattern: %v", f.calls)
	}

	// Ownership re-check at mint: a deleted persona (absent item) falls
	// back to the default.
	if got := ResolvePersona(UserPersonaRef("u1", "gone")); got.ID != "default" {
		t.Errorf("deleted persona resolved to %q, want default", got.ID)
	}
	// Another user's partition simply has no item — same fallback.
	if got := ResolvePersona(UserPersonaRef("u2", "abc123")); got.ID != "default" {
		t.Errorf("other user's ref resolved to %q, want default", got.ID)
	}
}

func TestResolveSharedPersonaRefVisibility(t *testing.T) {
	f := &fakePersonaGetter{}
	seedPersonaItem(f, "CATALOG", "PERSONA#sh1", "Shared DJ", "Shared patter.", true)
	seedPersonaItem(f, "CATALOG", "PERSONA#sh2", "Unshared", "Should not resolve.", false)
	withPersonaStore(t, f)

	if p := ResolvePersona(SharedPersonaRef("sh1")); p.Name != "Shared DJ" {
		t.Fatalf("shared ref resolved to %q", p.Name)
	}
	// shared=false mirror (mid-flight unshare) fails the visibility
	// re-check and falls back to default.
	if p := ResolvePersona(SharedPersonaRef("sh2")); p.ID != "default" {
		t.Errorf("unshared mirror resolved to %q, want default", p.ID)
	}
	// Absent mirror (unshare/delete write-through) — default.
	if p := ResolvePersona(SharedPersonaRef("nope")); p.ID != "default" {
		t.Errorf("absent mirror resolved to %q, want default", p.ID)
	}
}

func TestMalformedRefsFallBackToDefault(t *testing.T) {
	f := &fakePersonaGetter{}
	withPersonaStore(t, f)

	for _, id := range []string{"user:", "user:u1", "user::p", "user:u1:", "shared:", "weird:ref"} {
		if p := ResolvePersona(id); p.ID != "default" {
			t.Errorf("ResolvePersona(%q) = %q, want default", id, p.ID)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("malformed refs touched the store: %v", f.calls)
	}
}

func TestResolveWithoutStoreConfigured(t *testing.T) {
	// No store installed and no lazy env wiring possible in tests — the
	// resolver must degrade to default, never panic or error.
	SetPersonaStore(nil, "")
	t.Cleanup(func() { SetPersonaStore(nil, "") })
	if p := ResolvePersona("shared:whatever"); p.ID != "default" {
		t.Errorf("resolved to %q with no store, want default", p.ID)
	}
}

// TestBuiltinPersonaGroups: every built-in lands in a known picker section
// (owner 2026-08-01), the three families hold exactly who they should, and
// GroupOrder covers every group actually in use. The last check is the one
// that matters: the web picker renders sections from a fixed order, so a
// persona tagged with a group nobody renders would exist on the server and be
// unreachable in the UI.
func TestBuiltinPersonaGroups(t *testing.T) {
	inOrder := map[string]bool{}
	for _, g := range GroupOrder {
		inOrder[g] = true
	}

	counts := map[string]int{}
	for _, p := range BuiltinPersonas() {
		if !inOrder[p.Group] {
			t.Errorf("persona %q has group %q, which GroupOrder does not render", p.ID, p.Group)
		}
		counts[p.Group]++
	}

	// The default persona is the whole General section; the two working
	// families are pinned by size so adding one without tagging it shows up
	// here rather than as a persona quietly filed under Fun.
	if got := counts[GroupPDLC]; got != 3 {
		t.Errorf("PDLC group has %d personas, want 3", got)
	}
	if got := counts[GroupESP32]; got != 9 {
		t.Errorf("ESP32 group has %d personas, want 9 (one per chip)", got)
	}
	if ResolvePersona("default").Group != GroupGeneral {
		t.Errorf("the default persona must sit in %s", GroupGeneral)
	}
	for _, id := range []string{"product-owner", "staff-developer", "staff-sre"} {
		if g := ResolvePersona(id).Group; g != GroupPDLC {
			t.Errorf("persona %q group = %q, want %s", id, g, GroupPDLC)
		}
	}
	for _, id := range []string{"esp32-engineer", "esp32-p4-engineer", "esp32-h2-engineer"} {
		if g := ResolvePersona(id).Group; g != GroupESP32 {
			t.Errorf("persona %q group = %q, want %s", id, g, GroupESP32)
		}
	}
	// An untagged persona must not vanish from a grouped picker.
	if groupOrDefault("") != GroupGeneral || groupOrDefault("Nonsense") != GroupGeneral {
		t.Error("an unknown group must fall back to General, not disappear")
	}

	// The catalog surface carries the group too — that is what the picker
	// actually reads.
	for _, info := range ListPersonas() {
		if !inOrder[info.Group] {
			t.Errorf("catalog entry %q has unrenderable group %q", info.ID, info.Group)
		}
	}
}
