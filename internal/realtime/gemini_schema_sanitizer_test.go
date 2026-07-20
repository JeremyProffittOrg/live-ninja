package realtime

// M20/D1-D2 sanitizer tests (tool-parity-plan.md). geminiToolDeclarations()
// deep-copies each tool's `parameters` map and strips any JSON-Schema
// keyword genai.Schema does not model, folding each stripped constraint
// into that parameter's own description as prose (Q4). At today's SDK pin
// (google.golang.org/genai v1.64.0) every keyword the 20 real tools use is
// modeled, so the strip path is exercised here against synthetic schemas
// built with keywords genai.Schema genuinely does not have a struct field
// for (additionalProperties, const, multipleOf, uniqueItems) — proving the
// mechanism works rather than asserting a currently-empty diff on real
// data.

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGeminiSchemaKeywordsMatchesVendoredStruct pins the reflected keyword
// set to the fields the actual vendored genai.Schema struct exposes, so a
// future SDK bump that adds/removes a field is caught here instead of
// silently changing sanitizer behavior. Field list transcribed by reading
// google.golang.org/genai@v1.64.0/types.go's `type Schema struct` — not
// from memory.
func TestGeminiSchemaKeywordsMatchesVendoredStruct(t *testing.T) {
	want := []string{
		"anyOf", "default", "description", "enum", "example", "format",
		"items", "maxItems", "maxLength", "maxProperties", "maximum",
		"minItems", "minLength", "minProperties", "minimum", "nullable",
		"pattern", "properties", "propertyOrdering", "required", "title",
		"type",
	}
	require.Len(t, geminiSchemaKeywords, len(want), "geminiSchemaKeywords size drifted from the vendored Schema struct")
	for _, kw := range want {
		assert.True(t, geminiSchemaKeywords[kw], "expected %q to be a modeled genai.Schema keyword", kw)
	}

	// Keywords the 20 real tools' schemas actually use today — all must be
	// modeled, or the strip path would fire on real production data and
	// this whole test suite's "nothing is stripped today" premise (see
	// gemini_mint_test.go TestGeminiToolDeclarationsMirrorManifest) breaks.
	usedByRealTools := []string{
		"type", "description", "properties", "required", "enum", "items",
		"minLength", "maxLength", "pattern", "minimum", "maximum",
	}
	for _, kw := range usedByRealTools {
		assert.True(t, geminiSchemaKeywords[kw], "tool manifest uses %q; genai.Schema must model it", kw)
	}

	// Keywords genai.Schema genuinely does NOT model at this SDK pin —
	// these are exactly what the strip path exists for.
	notModeled := []string{
		"additionalProperties", "const", "multipleOf", "uniqueItems",
		"contentEncoding", "contentMediaType", "$ref", "allOf", "oneOf", "not",
	}
	for _, kw := range notModeled {
		assert.False(t, geminiSchemaKeywords[kw], "expected %q NOT to be a modeled genai.Schema keyword", kw)
	}
}

// TestSanitizeSchemaNodeStripsUnmodeledKeywordsIntoProse drives the
// sanitizer directly against a synthetic schema carrying keywords
// genai.Schema does not model, at every nesting depth the real manifest
// uses (top-level parameters object, a property schema, and an array's
// items schema).
func TestSanitizeSchemaNodeStripsUnmodeledKeywordsIntoProse(t *testing.T) {
	original := map[string]any{
		"type":                 "object",
		"additionalProperties": false, // not modeled — must be stripped
		"properties": map[string]any{
			"code": map[string]any{
				"type":        "string",
				"description": "A status code.",
				"const":       "OK", // not modeled — must be stripped
			},
			"tags": map[string]any{
				"type": "array",
				// uniqueItems placed at both the array node and (unusually,
				// but the sanitizer must not care) its items node, to prove
				// recursion strips at every depth, not just the top level.
				"items": map[string]any{
					"type":        "string",
					"uniqueItems": true, // not modeled — must be stripped
				},
				"uniqueItems": true, // not modeled — must be stripped
			},
			"count": map[string]any{
				"type":      "integer",
				"multipleOf": 5, // not modeled — must be stripped
			},
		},
		"required": []string{"code"},
	}

	sanitized := sanitizeGeminiParameters(original)

	// Original must be untouched (deep copy, not aliasing — D1's aliasing
	// footgun fix).
	assert.Equal(t, false, original["additionalProperties"])
	origProps := original["properties"].(map[string]any)
	assert.Equal(t, "OK", origProps["code"].(map[string]any)["const"])

	// Sanitized copy: unmodeled keywords gone at every level.
	assert.NotContains(t, sanitized, "additionalProperties")
	sanProps := sanitized["properties"].(map[string]any)
	code := sanProps["code"].(map[string]any)
	assert.NotContains(t, code, "const")
	assert.Equal(t, "A status code. Must be exactly OK.", code["description"])

	tags := sanProps["tags"].(map[string]any)
	assert.NotContains(t, tags, "uniqueItems")
	assert.Equal(t, "Items must be unique.", tags["description"])
	tagItems := tags["items"].(map[string]any)
	assert.NotContains(t, tagItems, "uniqueItems")
	assert.Equal(t, "Items must be unique.", tagItems["description"])

	count := sanProps["count"].(map[string]any)
	assert.NotContains(t, count, "multipleOf")
	assert.Equal(t, "Must be a multiple of 5.", count["description"])

	// The top-level parameters object itself gained a description carrying
	// its own stripped constraint (it had none before).
	assert.Equal(t, "No additional properties are allowed.", sanitized["description"])

	// Modeled keywords (type, required, properties structure) survive
	// unchanged.
	assert.Equal(t, "object", sanitized["type"])
	assert.Equal(t, []string{"code"}, sanitized["required"])
}

// TestSanitizeGeminiParametersDoesNotAliasToolManifest is the direct
// regression test for the pre-D1 aliasing footgun: mutating the sanitized
// copy must never reach the shared toolManifest literal.
func TestSanitizeGeminiParametersDoesNotAliasToolManifest(t *testing.T) {
	for _, tool := range toolManifest {
		params, ok := tool["parameters"].(map[string]any)
		if !ok {
			continue
		}
		sanitized := sanitizeGeminiParameters(params)
		sanitized["__mutated_by_test__"] = true
		if props, ok := sanitized["properties"].(map[string]any); ok {
			for _, v := range props {
				if child, ok := v.(map[string]any); ok {
					child["__mutated_by_test__"] = true
				}
			}
		}
		assert.NotContains(t, params, "__mutated_by_test__",
			"tool %v: sanitized copy must not alias toolManifest's parameters map", tool["name"])
	}
}

// TestGeminiSetupAndConstraintsDeclareSameToolSchemas is D2: the raw wire
// `setup` frame (buildGeminiSetup) and the SDK-typed constraints
// (buildGeminiConstraints) must declare the SAME tool schemas
// post-sanitization. Before D1 this was only true by accident (both paths
// happened to carry the same unsanitized reference); this test would catch
// a future change that lets the two paths diverge (e.g. one path adding a
// post-processing step the other lacks).
func TestGeminiSetupAndConstraintsDeclareSameToolSchemas(t *testing.T) {
	const model, voice, instructions = "gemini-3.1-flash-live-preview", "Kore", "You are terse."

	setup := buildGeminiSetup(model, voice, instructions)
	setupTools, ok := setup["tools"].([]map[string]any)
	require.True(t, ok)
	require.NotEmpty(t, setupTools)
	setupDecls, ok := setupTools[0]["functionDeclarations"].([]map[string]any)
	require.True(t, ok)

	constraints := buildGeminiConstraints(model, voice, instructions)
	require.NotNil(t, constraints.Config)
	require.NotEmpty(t, constraints.Config.Tools)
	sdkDecls := constraints.Config.Tools[0].FunctionDeclarations

	require.Len(t, sdkDecls, len(setupDecls), "setup frame and minted constraints must declare the same number of tools")

	setupJSON, err := json.Marshal(setupDecls)
	require.NoError(t, err)
	sdkJSON, err := json.Marshal(sdkDecls)
	require.NoError(t, err)

	assert.JSONEq(t, string(setupJSON), string(sdkJSON),
		"the raw wire setup frame and the SDK-typed constraints locked into the minted token must agree on every tool schema")

	// Also confirm neither path silently reintroduces the OpenAI "type"
	// discriminator or drops a tool.
	for _, d := range setupDecls {
		assert.NotContains(t, d, "type")
	}
	names := make(map[string]bool, len(sdkDecls))
	for _, d := range sdkDecls {
		names[d.Name] = true
	}
	for _, d := range setupDecls {
		name, _ := d["name"].(string)
		assert.True(t, names[name], "tool %q present in setup frame but missing from SDK constraints", name)
	}
}
