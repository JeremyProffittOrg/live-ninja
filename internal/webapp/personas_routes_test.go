package webapp

// Route-level tests for the persona platform surface (personas_routes.go)
// over a FakeDynamo-backed store: the grouped library list, create /
// duplicate (server-side copy), the built-in edit/delete/share guards,
// share visibility (attributed, owner excluded from their own "shared"
// group), and the mint-side qualifyPersonaRef ordering + ':'-injection
// rejection.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// newPersonasApp builds a Fiber app with only the persona routes mounted,
// authenticated as the given user (mirrors newHistoryAPIApp).
func newPersonasApp(t *testing.T, userID string) (*fiber.App, *Deps, *testutil.FakeDynamo) {
	t.Helper()
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja")
	deps := &Deps{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, userID)
		return c.Next()
	})
	registerPersonaRoutes(app.Group("/api/v1"), deps)
	return app, deps, fake
}

func seedProfile(fake *testutil.FakeDynamo, userID, name string) {
	fake.SeedItem(map[string]types.AttributeValue{
		"pk":     &types.AttributeValueMemberS{Value: "USER#" + userID},
		"sk":     &types.AttributeValueMemberS{Value: "PROFILE"},
		"userId": &types.AttributeValueMemberS{Value: userID},
		"name":   &types.AttributeValueMemberS{Value: name},
		"email":  &types.AttributeValueMemberS{Value: userID + "@example.com"},
		"role":   &types.AttributeValueMemberS{Value: "member"},
		"status": &types.AttributeValueMemberS{Value: "active"},
	})
}

func groupIDs(body map[string]any, group string) []string {
	rows, _ := body[group].([]any)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		m, _ := r.(map[string]any)
		id, _ := m["id"].(string)
		out = append(out, id)
	}
	return out
}

// TestPersonasPageRenders executes the /personas SSR template exactly as
// handleClientDataPage does (nil bind, pages_routes.go). This is the
// regression guard for the prod 500: the route + nav link shipped while
// the template was absent, so every /personas navigation failed at
// render time.
func TestPersonasPageRenders(t *testing.T) {
	_, rend := newTestShell(t)
	var buf bytes.Buffer
	if err := rend.Render(&buf, "pages/personas", nil); err != nil {
		t.Fatalf("render pages/personas: %v", err)
	}
	html := buf.String()
	for _, want := range []string{"Personas", "js/personas", "perDeleteConfirm", "perVoice"} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered /personas page missing %q", want)
		}
	}
}

func TestPersonaLibraryListGrouped(t *testing.T) {
	app, deps, _ := newPersonasApp(t, "u1")
	ctx := context.Background()

	// One own persona + one persona shared by another user.
	mustCreatePersona(t, deps, "u1", "mine1", "My DJ")
	mustCreatePersona(t, deps, "u2", "theirs1", "Their Chef")
	if _, err := deps.Store.SetUserPersonaShared(ctx, "u2", "Casey", "theirs1", true); err != nil {
		t.Fatalf("share theirs1: %v", err)
	}

	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/personas", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d (%v)", resp.StatusCode, body)
	}

	builtin := groupIDs(body, "builtin")
	if len(builtin) < 11 || builtin[0] != "default" {
		t.Errorf("builtin group = %v (want >= 11 entries, default first)", builtin)
	}
	// Built-in entries never expose instructions.
	firstBuiltin := body["builtin"].([]any)[0].(map[string]any)
	if _, leaked := firstBuiltin["instructions"]; leaked {
		t.Errorf("builtin entry leaked instructions")
	}

	if mine := groupIDs(body, "mine"); len(mine) != 1 || mine[0] != "mine1" {
		t.Errorf("mine group = %v, want [mine1]", mine)
	}
	sharedRows, _ := body["shared"].([]any)
	if len(sharedRows) != 1 {
		t.Fatalf("shared group = %v, want 1 entry", sharedRows)
	}
	sh := sharedRows[0].(map[string]any)
	if sh["id"] != "theirs1" || sh["owner"] != "Casey" {
		t.Errorf("shared entry = %v, want theirs1 attributed to Casey", sh)
	}
	if _, leaked := sh["instructions"]; leaked {
		t.Errorf("shared entry leaked another user's instructions")
	}
}

func TestPersonaSharedGroupExcludesOwn(t *testing.T) {
	app, deps, _ := newPersonasApp(t, "u1")
	mustCreatePersona(t, deps, "u1", "mine1", "My DJ")
	if _, err := deps.Store.SetUserPersonaShared(context.Background(), "u1", "Me", "mine1", true); err != nil {
		t.Fatalf("share: %v", err)
	}

	_, body := doJSON(t, app, http.MethodGet, "/api/v1/personas", nil)
	if shared := groupIDs(body, "shared"); len(shared) != 0 {
		t.Errorf("own shared persona duplicated into shared group: %v", shared)
	}
	mineRows, _ := body["mine"].([]any)
	if len(mineRows) != 1 || mineRows[0].(map[string]any)["shared"] != true {
		t.Errorf("mine entry missing shared badge: %v", mineRows)
	}
}

// mustCreatePersona seeds a persona through the store (fixed ID for
// test addressing — the HTTP create path generates random IDs).
func mustCreatePersona(t *testing.T, deps *Deps, userID, id, name string) {
	t.Helper()
	err := deps.Store.CreateUserPersona(context.Background(), userID, &store.UserPersona{
		PersonaID:    id,
		Name:         name,
		Description:  "about " + name,
		Instructions: "Talk like " + name + ".",
		Voice:        "cedar",
	})
	if err != nil {
		t.Fatalf("create persona %s: %v", id, err)
	}
}

func TestPersonaCreateValidateAndDuplicate(t *testing.T) {
	app, _, _ := newPersonasApp(t, "u1")

	// Plain create.
	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/personas", map[string]any{
		"name": "Radio DJ", "description": "Groovy", "instructions": "Smooth 70s patter.", "voice": "ash",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d (%v)", resp.StatusCode, body)
	}
	if body["name"] != "Radio DJ" || body["instructions"] != "Smooth 70s patter." || body["shared"] != false {
		t.Errorf("created persona = %v", body)
	}

	// Validation: missing instructions, bad voice.
	resp, _ = doJSON(t, app, http.MethodPost, "/api/v1/personas", map[string]any{"name": "X"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing instructions status = %d, want 400", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodPost, "/api/v1/personas", map[string]any{
		"name": "X", "instructions": "y", "voice": "not-a-voice",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad voice status = %d, want 400", resp.StatusCode)
	}

	// Duplicate a built-in: server-side copy seeds name/desc/voice/style.
	resp, body = doJSON(t, app, http.MethodPost, "/api/v1/personas", map[string]any{"copyOf": "valley-girl"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("duplicate builtin status = %d (%v)", resp.StatusCode, body)
	}
	if body["name"] != "Valley Girl (copy)" || body["voice"] != "coral" {
		t.Errorf("builtin copy = %v", body)
	}
	if instr, _ := body["instructions"].(string); instr == "" {
		t.Errorf("builtin copy has empty instructions (nothing to edit)")
	}

	// Duplicate an unknown source: 400.
	resp, _ = doJSON(t, app, http.MethodPost, "/api/v1/personas", map[string]any{"copyOf": "nope"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("copy of unknown status = %d, want 400", resp.StatusCode)
	}
}

func TestPersonaCopySharedToMine(t *testing.T) {
	app, deps, _ := newPersonasApp(t, "u1")
	mustCreatePersona(t, deps, "u2", "theirs1", "Their Chef")
	if _, err := deps.Store.SetUserPersonaShared(context.Background(), "u2", "Casey", "theirs1", true); err != nil {
		t.Fatalf("share: %v", err)
	}

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/personas", map[string]any{"copyOf": "theirs1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("copy shared status = %d (%v)", resp.StatusCode, body)
	}
	if body["name"] != "Their Chef (copy)" || body["instructions"] != "Talk like Their Chef." {
		t.Errorf("shared copy = %v", body)
	}
}

func TestPersonaBuiltinGuards(t *testing.T) {
	app, _, _ := newPersonasApp(t, "u1")

	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/personas/default"},
		{http.MethodDelete, "/api/v1/personas/valley-girl"},
		{http.MethodPost, "/api/v1/personas/logic-officer/share"},
	} {
		resp, body := doJSON(t, app, tc.method, tc.path, map[string]any{
			"name": "x", "instructions": "y", "shared": true,
		})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s status = %d (%v), want 403", tc.method, tc.path, resp.StatusCode, body)
		}
	}

	// Built-in GET works but never exposes instructions.
	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/personas/valley-girl", nil)
	if resp.StatusCode != http.StatusOK || body["builtin"] != true {
		t.Fatalf("get builtin = %d (%v)", resp.StatusCode, body)
	}
	if _, leaked := body["instructions"]; leaked {
		t.Errorf("builtin GET leaked instructions")
	}
}

func TestPersonaUpdateDeleteShareFlow(t *testing.T) {
	app, deps, fake := newPersonasApp(t, "u1")
	seedProfile(fake, "u1", "Jeremy")
	mustCreatePersona(t, deps, "u1", "p1", "DJ")

	// Update.
	resp, body := doJSON(t, app, http.MethodPut, "/api/v1/personas/p1", map[string]any{
		"name": "DJ Prime", "instructions": "New patter.",
	})
	if resp.StatusCode != http.StatusOK || body["name"] != "DJ Prime" || body["instructions"] != "New patter." {
		t.Fatalf("update = %d (%v)", resp.StatusCode, body)
	}

	// Share: mirror appears, attributed via the PROFILE name.
	resp, body = doJSON(t, app, http.MethodPost, "/api/v1/personas/p1/share", map[string]any{"shared": true})
	if resp.StatusCode != http.StatusOK || body["shared"] != true {
		t.Fatalf("share = %d (%v)", resp.StatusCode, body)
	}
	cp, err := deps.Store.GetCatalogPersona(context.Background(), "p1")
	if err != nil || cp == nil || cp.OwnerName != "Jeremy" || cp.Instructions != "New patter." {
		t.Fatalf("mirror after share = %+v, err %v", cp, err)
	}

	// Another user's PUT/DELETE on it: 404 (never a cross-user mutation).
	otherApp, otherDeps, _ := newPersonasApp(t, "u2")
	otherDeps.Store = deps.Store // same table
	resp, _ = doJSON(t, otherApp, http.MethodPut, "/api/v1/personas/p1", map[string]any{"name": "hijack", "instructions": "x"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-user PUT status = %d, want 404", resp.StatusCode)
	}

	// Delete: gone + mirror removed.
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/personas/p1", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	if cp, _ := deps.Store.GetCatalogPersona(context.Background(), "p1"); cp != nil {
		t.Errorf("mirror survived delete")
	}
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/personas/p1", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("re-delete status = %d, want 404", resp.StatusCode)
	}
}

func TestQualifyPersonaRefOrderingAndInjection(t *testing.T) {
	_, deps, fake := newPersonasApp(t, "u1")
	ctx := context.Background()
	mustCreatePersona(t, deps, "u1", "own1", "Mine")
	mustCreatePersona(t, deps, "u2", "sh1", "Theirs")
	if _, err := deps.Store.SetUserPersonaShared(ctx, "u2", "Casey", "sh1", true); err != nil {
		t.Fatalf("share: %v", err)
	}

	cases := []struct{ in, want string }{
		{"", ""},                            // empty passes through (broker default)
		{"default", "default"},              // built-in untouched
		{"valley-girl", "valley-girl"},      // built-in untouched
		{"custom", "custom"},                // settings-page custom preset untouched
		{"own1", "user:u1:own1"},            // own persona -> user ref
		{"sh1", "shared:sh1"},               // shared catalog -> shared ref
		{"ghost", "ghost"},                  // unknown passes through (broker defaults)
		{"user:u2:own1", "default"},         // client-supplied ref REJECTED
		{"shared:sh1", "default"},           // client-supplied ref REJECTED
		{fmt.Sprintf("a%cb", '#'), "default"}, // key-syntax injection rejected
	}
	for _, tc := range cases {
		if got := qualifyPersonaRef(ctx, deps, "u1", tc.in); got != tc.want {
			t.Errorf("qualifyPersonaRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Own persona wins over a shared one with the same ID shape: u1 also
	// has "sh1"? Create it and confirm own-first ordering.
	mustCreatePersona(t, deps, "u1", "sh1", "MyShadow")
	if got := qualifyPersonaRef(ctx, deps, "u1", "sh1"); got != "user:u1:sh1" {
		t.Errorf("own persona did not win over shared: %q", got)
	}

	// End-to-end with the broker resolver: the qualified refs resolve
	// against the same table state via GetItem-only key lookups
	// (FakeDynamo satisfies realtime.PersonaItemGetter directly).
	realtime.SetPersonaStore(fake, "live-ninja")
	t.Cleanup(func() { realtime.SetPersonaStore(nil, "") })
	if p := realtime.ResolvePersona("user:u1:own1"); p.Name != "Mine" {
		t.Errorf("broker resolve own = %q", p.Name)
	}
	if p := realtime.ResolvePersona("shared:sh1"); p.Name != "Theirs" {
		t.Errorf("broker resolve shared = %q", p.Name)
	}
	// Delete + unshare, then re-check at mint falls back to default.
	if err := deps.Store.DeleteUserPersona(ctx, "u1", "own1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := deps.Store.SetUserPersonaShared(ctx, "u2", "Casey", "sh1", false); err != nil {
		t.Fatalf("unshare: %v", err)
	}
	if p := realtime.ResolvePersona("user:u1:own1"); p.ID != "default" {
		t.Errorf("deleted persona still resolved: %q", p.ID)
	}
	if p := realtime.ResolvePersona("shared:sh1"); p.ID != "default" {
		t.Errorf("unshared persona still resolved: %q", p.ID)
	}
}
