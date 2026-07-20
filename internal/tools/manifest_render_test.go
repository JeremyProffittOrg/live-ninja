package tools

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenderManifestSurfacesEveryConstraintKind is the M19/C2
// drift-resistance test (tool-parity-plan.md): a synthetic Definition
// populating every ParamSpec field proves renderManifest surfaces ALL SIX
// renderable constraint kinds — enum, minLength, maxLength, pattern,
// minimum, maximum — and excludes Unadvertised params. If a future
// jsonSchema() edit silently drops a constraint kind, the advertised
// schema stops matching what coerce() enforces (the model gets rejected
// by rules it was never shown — the exact P2 failure mode); this test
// makes that edit loud instead of silent.
func TestRenderManifestSurfacesEveryConstraintKind(t *testing.T) {
	def := &Definition{
		Name:        "constraint_probe",
		Description: "Synthetic tool exercising every renderable ParamSpec constraint kind.",
		Params: []ParamSpec{
			{
				// Kinds: minLength, maxLength, pattern (SafeName).
				Name:        "slug",
				Type:        "string",
				Description: "safe filename slug",
				Required:    true,
				MinLen:      2,
				MaxLen:      40,
				SafeName:    true,
			},
			{
				// Kind: enum.
				Name:        "mode",
				Type:        "string",
				Description: "enumerated mode",
				Required:    true,
				Enum:        []string{"fast", "slow"},
			},
			{
				// Kinds: minimum, maximum (integer).
				Name:        "count",
				Type:        "integer",
				Description: "bounded integer",
				Min:         floatPtr(1),
				Max:         floatPtr(10),
			},
			{
				// Kinds: minimum, maximum (number — the other numeric type).
				Name:        "ratio",
				Type:        "number",
				Description: "bounded number",
				Min:         floatPtr(0.5),
				Max:         floatPtr(2.5),
			},
			{
				Name:        "tags",
				Type:        "string_array",
				Description: "array param",
			},
			{
				Name:        "flag",
				Type:        "boolean",
				Description: "boolean param",
			},
			{
				// Unadvertised compat alias: still validated by coerce(),
				// must never appear in the rendered schema. Carries every
				// remaining ParamSpec field (OutOfRangeHint) so the whole
				// struct is populated somewhere in this Definition — and is
				// deliberately Required so the "required" assertion below
				// proves Unadvertised wins over Required.
				Name:           "legacy",
				Type:           "integer",
				Description:    "hidden compat alias",
				Required:       true,
				Min:            floatPtr(1),
				Max:            floatPtr(10),
				Unadvertised:   true,
				OutOfRangeHint: "Use constraint_probe.count instead.",
			},
		},
	}

	out := renderManifest([]*Definition{def})
	require.Len(t, out, 1)
	entry := out[0]
	assert.Equal(t, "function", entry["type"])
	assert.Equal(t, "constraint_probe", entry["name"])
	assert.Equal(t, def.Description, entry["description"])

	params, ok := entry["parameters"].(map[string]any)
	require.True(t, ok, "parameters must be an object schema")
	assert.Equal(t, "object", params["type"])

	props, ok := params["properties"].(map[string]any)
	require.True(t, ok, "parameters.properties must be an object")

	// Required: exactly the advertised required params, sorted — and never
	// an Unadvertised one, even though "legacy" IS marked Required above.
	assert.Equal(t, []string{"mode", "slug"}, params["required"])

	slug, ok := props["slug"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "string", slug["type"])
	assert.Equal(t, "safe filename slug", slug["description"])
	assert.Equal(t, 2, slug["minLength"], "constraint kind minLength must be rendered")
	assert.Equal(t, 40, slug["maxLength"], "constraint kind maxLength must be rendered")
	assert.Equal(t, safeFileNamePattern, slug["pattern"], "constraint kind pattern must be rendered")

	mode, ok := props["mode"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "string", mode["type"])
	assert.Equal(t, []string{"fast", "slow"}, mode["enum"], "constraint kind enum must be rendered")

	count, ok := props["count"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "integer", count["type"])
	assert.Equal(t, float64(1), count["minimum"], "constraint kind minimum must be rendered")
	assert.Equal(t, float64(10), count["maximum"], "constraint kind maximum must be rendered")

	ratio, ok := props["ratio"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "number", ratio["type"])
	assert.Equal(t, 0.5, ratio["minimum"], "minimum must render on number params too")
	assert.Equal(t, 2.5, ratio["maximum"], "maximum must render on number params too")

	tags, ok := props["tags"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "array", tags["type"], "string_array must render as JSON-schema array")
	assert.Equal(t, map[string]any{"type": "string"}, tags["items"])

	flag, ok := props["flag"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "boolean", flag["type"])

	// The Unadvertised param is excluded from the rendered schema entirely
	// (it remains accepted and range-enforced by validateArgs/coerce).
	assert.NotContains(t, props, "legacy",
		"Unadvertised params must be excluded from the rendered manifest")
}

// TestRenderManifestConstraintTestCoversEveryParamSpecField pins the
// ParamSpec field set. When a field is added, this fails to force a
// decision: does jsonSchema()/renderManifest need to surface it, and does
// TestRenderManifestSurfacesEveryConstraintKind need a new assertion?
// Update knownFields (and that test) together — never just the count.
func TestRenderManifestConstraintTestCoversEveryParamSpecField(t *testing.T) {
	knownFields := map[string]bool{
		"Name":           true,
		"Type":           true,
		"Description":    true,
		"Required":       true,
		"Enum":           true, // rendered: enum
		"MinLen":         true, // rendered: minLength
		"MaxLen":         true, // rendered: maxLength
		"Min":            true, // rendered: minimum
		"Max":            true, // rendered: maximum
		"SafeName":       true, // rendered: pattern
		"Unadvertised":   true, // rendered: excluded entirely
		"OutOfRangeHint": true, // error-message-only, never rendered
	}
	typ := reflect.TypeOf(ParamSpec{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		assert.Containsf(t, knownFields, name,
			"new ParamSpec field %q: decide whether renderManifest must surface it and update TestRenderManifestSurfacesEveryConstraintKind + this list", name)
	}
	assert.Equal(t, len(knownFields), typ.NumField(),
		"ParamSpec field removed — prune knownFields and the constraint test with it")
}

// TestCatalogManifestEqualsRegistryManifest closes the render loop: the
// dependency-free package-level CatalogManifest (what the realtime broker
// binds) and a live Registry's Manifest (the registry Invoke enforces
// against) must be deeply identical — same tools, same order, same
// schemas. Both go through renderManifest by construction; this keeps a
// future refactor from quietly forking them again.
func TestCatalogManifestEqualsRegistryManifest(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	catalog := CatalogManifest()
	registry := r.Manifest()
	if !reflect.DeepEqual(catalog, registry) {
		t.Errorf("CatalogManifest and (*Registry).Manifest diverged\ncatalog:  %#v\nregistry: %#v",
			catalog, registry)
	}
}
