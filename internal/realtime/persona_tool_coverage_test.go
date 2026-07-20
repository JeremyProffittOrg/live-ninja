package realtime

// M20/D5 (tool-parity-plan.md): guards P3 — "six tools are never described
// to the model" — from silently recurring when tool 21 lands. Every tool
// name in toolManifest must appear verbatim in coreInstructions, or sit on
// deliberatelyUnmentionedTools, an explicit, named, reviewed allow-list.
// D4 folded the six previously-missing deliverable/file tools into one
// "documents and downloads" clause, so this allow-list is empty today.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// deliberatelyUnmentionedTools names tool-manifest entries that are
// intentionally absent from coreInstructions, with the reason recorded
// inline. Keep this empty unless there is a real, reviewed reason a tool
// should stay unadvertised in the prompt (e.g. an internal/diagnostic-only
// tool never meant to be model-invoked) — every entry here is a
// conversation to have before adding it, not a default escape hatch.
var deliberatelyUnmentionedTools = map[string]string{
	// (none — D4 named all 20 tools in coreInstructions)
}

// TestEveryManifestToolIsDiscoverableFromPersonaPrompt is D5.
func TestEveryManifestToolIsDiscoverableFromPersonaPrompt(t *testing.T) {
	requireNonEmptyManifest(t)

	for _, tool := range toolManifest {
		name, ok := tool["name"].(string)
		if !ok || name == "" {
			t.Fatalf("toolManifest entry missing a string name: %#v", tool)
		}
		if reason, allowed := deliberatelyUnmentionedTools[name]; allowed {
			t.Logf("tool %q deliberately unmentioned in coreInstructions: %s", name, reason)
			continue
		}
		assert.True(t, strings.Contains(coreInstructions, name),
			"tool %q must be named in coreInstructions (personas.go) so the model can discover it, "+
				"or be added to deliberatelyUnmentionedTools with a reason", name)
	}
}

// TestDeliberatelyUnmentionedToolsStaysNearEmpty keeps the allow-list from
// quietly becoming the new dumping ground P3 was: it must reference only
// real tool names (no stale entries surviving a rename) and must stay
// small enough that every entry was a deliberate, individually-reviewed
// decision.
func TestDeliberatelyUnmentionedToolsStaysNearEmpty(t *testing.T) {
	const maxAllowed = 2 // generous ceiling; today's expected count is 0
	assert.LessOrEqualf(t, len(deliberatelyUnmentionedTools), maxAllowed,
		"deliberatelyUnmentionedTools has grown to %d entries — P3 (tool-parity-plan.md) is the exact "+
			"failure this allow-list must stay near-empty to prevent", len(deliberatelyUnmentionedTools))

	names := make(map[string]bool, len(toolManifest))
	for _, tool := range toolManifest {
		if name, ok := tool["name"].(string); ok {
			names[name] = true
		}
	}
	for name := range deliberatelyUnmentionedTools {
		assert.True(t, names[name], "deliberatelyUnmentionedTools references %q, which is not in toolManifest — stale entry", name)
	}
}

// requireNonEmptyManifest guards against this test silently passing on an
// empty manifest (e.g. an init-order regression zeroing toolManifest before
// this test runs), which would make the coverage loop above vacuously true.
func requireNonEmptyManifest(t *testing.T) {
	t.Helper()
	if len(toolManifest) == 0 {
		t.Fatal("toolManifest is empty — cannot assert persona coverage over zero tools")
	}
}
