package webapp

// Route-level tests for the assistant rules API (rules_routes.go) over a
// FakeDynamo-backed store: the first-list seed, create/update by name,
// toggle, delete, and validation rejections.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func ruleNames(body map[string]any) []string {
	raw, _ := body["rules"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			name, _ := m["name"].(string)
			out = append(out, name)
		}
	}
	return out
}

func TestRulesListSeedsDefaultRule(t *testing.T) {
	app, _, fake := newMemoryAPIApp(t)

	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/rules", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d (%v)", resp.StatusCode, body)
	}
	names := ruleNames(body)
	if len(names) != 1 || names[0] != "location-and-time" {
		t.Fatalf("first list must return only the seeded rule, got %v", names)
	}
	seed := body["rules"].([]any)[0].(map[string]any)
	if seed["source"] != "seed" || seed["enabled"] != true {
		t.Errorf("seed rule must be source=seed, enabled=true; got %v", seed)
	}
	if body, _ := seed["body"].(string); !strings.Contains(body, "set_current_location") {
		t.Errorf("seed rule body must carry the location instructions, got %q", body)
	}
	if fake.RawItem("USER#u1", "RULE#location-and-time") == nil {
		t.Errorf("seed rule must be persisted under RULE#location-and-time")
	}
}

func TestRulesCreateUpdateToggleDelete(t *testing.T) {
	app, _, fake := newMemoryAPIApp(t)

	// The seed is written only by a list of an empty account, so list first
	// the way the Memory page does on load.
	doJSON(t, app, http.MethodGet, "/api/v1/rules", nil)

	create := map[string]any{
		"name":        "trip-packing",
		"description": "Load when the user asks what to pack\nfor a trip.",
		"body":        "List clothes first, then chargers, then documents.",
	}
	resp, created := doJSON(t, app, http.MethodPost, "/api/v1/rules", create)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%v)", resp.StatusCode, created)
	}
	if created["name"] != "trip-packing" || created["source"] != "user" || created["enabled"] != true {
		t.Fatalf("create returned %v", created)
	}
	if created["description"] != "Load when the user asks what to pack for a trip." {
		t.Errorf("description must be collapsed to one line, got %q", created["description"])
	}
	if fake.RawItem("USER#u1", "RULE#trip-packing") == nil {
		t.Fatalf("RULE item missing after create")
	}

	// Disable, then edit without "enabled": the edit must keep it disabled.
	resp, toggled := doJSON(t, app, http.MethodPut, "/api/v1/rules/trip-packing", map[string]any{"enabled": false})
	if resp.StatusCode != http.StatusOK || toggled["enabled"] != false {
		t.Fatalf("toggle = %d %v", resp.StatusCode, toggled)
	}
	create["body"] = "List documents first."
	resp, updated := doJSON(t, app, http.MethodPost, "/api/v1/rules", create)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (%v)", resp.StatusCode, updated)
	}
	if updated["body"] != "List documents first." || updated["enabled"] != false {
		t.Errorf("update must replace the body and keep enabled=false, got %v", updated)
	}
	if updated["createdAt"] != created["createdAt"] {
		t.Errorf("update must preserve createdAt: %v vs %v", updated["createdAt"], created["createdAt"])
	}

	// List: seed + the new rule, sorted by name.
	_, body := doJSON(t, app, http.MethodGet, "/api/v1/rules", nil)
	if got := ruleNames(body); len(got) != 2 || got[0] != "location-and-time" || got[1] != "trip-packing" {
		t.Errorf("list = %v, want [location-and-time trip-packing]", got)
	}

	// Delete, then 404 on a second delete and on a toggle.
	resp, body = doJSON(t, app, http.MethodDelete, "/api/v1/rules/trip-packing", nil)
	if resp.StatusCode != http.StatusOK || body["ok"] != true {
		t.Fatalf("delete = %d %v", resp.StatusCode, body)
	}
	if fake.RawItem("USER#u1", "RULE#trip-packing") != nil {
		t.Errorf("delete must remove the RULE item")
	}
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/rules/trip-packing", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodPut, "/api/v1/rules/trip-packing", map[string]any{"enabled": true})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("toggle of a missing rule status = %d, want 404", resp.StatusCode)
	}
}

func TestRulesValidation(t *testing.T) {
	app, _, _ := newMemoryAPIApp(t)

	cases := []struct {
		label string
		body  map[string]any
	}{
		{"bad name", map[string]any{"name": "Trip Packing", "description": "Load when packing for a trip.", "body": "x"}},
		{"short name", map[string]any{"name": "ab", "description": "Load when packing for a trip.", "body": "x"}},
		{"short description", map[string]any{"name": "trip-packing", "description": "packing", "body": "x"}},
		{"long description", map[string]any{"name": "trip-packing", "description": strings.Repeat("a", 201), "body": "x"}},
		{"empty body", map[string]any{"name": "trip-packing", "description": "Load when packing for a trip.", "body": "  "}},
		{"long body", map[string]any{"name": "trip-packing", "description": "Load when packing for a trip.", "body": strings.Repeat("b", 4001)}},
	}
	for _, tc := range cases {
		resp, body := doJSON(t, app, http.MethodPost, "/api/v1/rules", tc.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%v)", tc.label, resp.StatusCode, body)
		}
	}

	resp, _ := doJSON(t, app, http.MethodPut, "/api/v1/rules/trip-packing", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("toggle without enabled status = %d, want 400", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/rules/Not_A_Slug", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("delete with invalid name status = %d, want 400", resp.StatusCode)
	}
}

func TestRulesLimitIsAConflict(t *testing.T) {
	app, _, _ := newMemoryAPIApp(t)

	// The seed counts toward the 50-rule cap once it exists.
	doJSON(t, app, http.MethodGet, "/api/v1/rules", nil)
	for i := 0; i < 49; i++ {
		resp, body := doJSON(t, app, http.MethodPost, "/api/v1/rules", map[string]any{
			"name":        fmt.Sprintf("rule-%02d", i),
			"description": "Load for the numbered test case.",
			"body":        "Do the thing.",
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("rule %d: status = %d (%v)", i, resp.StatusCode, body)
		}
	}
	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/rules", map[string]any{
		"name":        "one-too-many",
		"description": "Load for the numbered test case.",
		"body":        "Do the thing.",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("51st rule status = %d, want 409 (%v)", resp.StatusCode, body)
	}
}
