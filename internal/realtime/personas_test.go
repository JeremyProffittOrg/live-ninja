package realtime

// Persona registry + stored-persona ref resolution tests (personas
// platform feature): the built-in seed set, the mint resolution order
// (built-in -> user's own -> shared catalog -> default), the shared-
// visibility re-check, and the structural no-Scan guarantee (the store
// seam only exposes GetItem — a fake records every call).

import (
	"context"
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

	// The task-mandated trio plus a sample of the range set.
	for _, id := range []string{"valley-girl", "logic-officer", "deputy-chief",
		"noir-detective", "bard", "zen-monk", "drill-sergeant", "play-by-play",
		"butler", "surfer", "worried-grandma"} {
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
