package realtime

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

// TestToolManifestJSONInitOrder guards the B3 init-order chain
// (tool-parity-plan.md M19): toolManifest is now derived at package init
// from tools.CatalogManifest(), and toolManifestJSON is a package-level
// func() var initialised from toolManifest. Go resolves package-level
// dependency order automatically, but this pins it: ToolManifestJSON()
// must be non-empty and parse to the full 20-tool catalog, each entry a
// complete OpenAI function-tool declaration.
func TestToolManifestJSONInitOrder(t *testing.T) {
	raw := ToolManifestJSON()
	require.NotEmpty(t, raw, "ToolManifestJSON must be non-empty — init-order regression")

	var entries []map[string]any
	require.NoError(t, json.Unmarshal(raw, &entries))
	require.Len(t, entries, 20, "the full tool catalog must be bound")

	for i, e := range entries {
		assert.Equal(t, "function", e["type"], "entry %d type", i)
		name, _ := e["name"].(string)
		assert.NotEmpty(t, name, "entry %d name", i)
		desc, _ := e["description"].(string)
		assert.NotEmpty(t, desc, "entry %d (%s) description", i, name)
		params, ok := e["parameters"].(map[string]any)
		require.True(t, ok, "entry %d (%s) parameters must be an object", i, name)
		assert.Equal(t, "object", params["type"], "entry %d (%s) parameters.type", i, name)
	}
}

// TestBrokerBoundManifestMatchesRouterCatalog is THE missing parity test
// (tool-parity-plan.md M19/C1): whatever the broker binds into every
// session (ToolManifestJSON, the exact bytes handed to OpenAI/Gemini/
// fallback and echoed to clients) must equal what the tool router
// enforces (a fresh render of tools.CatalogManifest — the same
// renderManifest the live Registry's Manifest() goes through, over the
// same definitions() Invoke validates against).
//
// It lives on the CONSUMER side deliberately: if anyone reintroduces a
// hand-written manifest literal in mint.go — the P2 drift that broke
// set_timer in prod — this fails, no matter how plausible the literal
// looks. And it is deliberately DEEP, not a name/count check: the
// count-only assertion form (old gemini_mint_test.go:53) is exactly the
// weak form that let 6 tools' parameter schemas drift undetected. Per
// tool it demands a full reflect.DeepEqual of the bound parameters
// against the freshly rendered catalog entry, plus exact description and
// type equality. Both sides are compared in JSON-normalized (wire) form,
// because the bound side IS wire bytes.
func TestBrokerBoundManifestMatchesRouterCatalog(t *testing.T) {
	var bound []map[string]any
	require.NoError(t, json.Unmarshal(ToolManifestJSON(), &bound),
		"broker-bound manifest must parse")
	require.NotEmpty(t, bound)

	// Freshly render the router's catalog and JSON-normalize it so both
	// sides compare in wire form (numbers as float64, arrays as []any).
	renderedJSON, err := json.Marshal(tools.CatalogManifest())
	require.NoError(t, err)
	var enforced []map[string]any
	require.NoError(t, json.Unmarshal(renderedJSON, &enforced))
	require.NotEmpty(t, enforced)

	index := func(entries []map[string]any, side string) map[string]map[string]any {
		byName := make(map[string]map[string]any, len(entries))
		for i, e := range entries {
			name, _ := e["name"].(string)
			require.NotEmptyf(t, name, "%s entry %d has no name", side, i)
			require.NotContainsf(t, byName, name, "%s manifest declares %q twice", side, name)
			byName[name] = e
		}
		return byName
	}
	boundByName := index(bound, "bound")
	enforcedByName := index(enforced, "enforced")

	// Name-set equality, BOTH directions.
	for name := range boundByName {
		assert.Containsf(t, enforcedByName, name,
			"broker binds tool %q but the router does not enforce it", name)
	}
	for name := range enforcedByName {
		assert.Containsf(t, boundByName, name,
			"router enforces tool %q but the broker never binds it", name)
	}

	// Same catalog order too — both sides derive from definitions() order,
	// and a reintroduced literal that merely reorders is still a fork.
	require.Len(t, bound, len(enforced))
	for i := range bound {
		assert.Equalf(t, enforced[i]["name"], bound[i]["name"],
			"manifest order diverged at index %d", i)
	}

	// Per-tool deep equality: description, wire type, and a full
	// reflect.DeepEqual of the parameters schema.
	for name, be := range boundByName {
		ee, ok := enforcedByName[name]
		if !ok {
			continue // already failed the name-set assertion above
		}
		assert.Equalf(t, "function", be["type"], "tool %q wire type", name)
		assert.Equalf(t, ee["description"], be["description"],
			"tool %q: bound description drifted from the router catalog", name)
		if !reflect.DeepEqual(be["parameters"], ee["parameters"]) {
			t.Errorf("tool %q: bound parameters have drifted from the router-enforced schema\nbound:    %#v\nenforced: %#v",
				name, be["parameters"], ee["parameters"])
		}
	}

	// Consumer-visible spot check of the Unadvertised contract (M18 A2):
	// the compat aliases the router still accepts must never be TAUGHT to
	// the model — the bound schema advertises inSeconds only.
	for _, toolName := range []string{"set_timer", "set_reminder"} {
		entry, ok := boundByName[toolName]
		require.Truef(t, ok, "%s must be in the bound manifest", toolName)
		params, ok := entry["parameters"].(map[string]any)
		require.Truef(t, ok, "%s parameters must be an object", toolName)
		props, ok := params["properties"].(map[string]any)
		require.Truef(t, ok, "%s properties must be an object", toolName)
		assert.Containsf(t, props, "inSeconds", "%s must advertise inSeconds", toolName)
		assert.NotContainsf(t, props, "seconds",
			"%s must NOT advertise the unadvertised compat alias \"seconds\"", toolName)
	}
}
