package webapp

// Guards for the fingerprinted module graph (plan.md §4.3).
//
// The failure these exist to prevent is not a cosmetic one: templates load an
// ENTRY module by its fingerprinted URL, the modules import their siblings by
// logical path, and web/sw.js serves /static/* stale-while-revalidate. Before
// the import map, a deploy that changed one module could pair it with a
// pre-deploy sibling out of the service-worker cache; module linking then
// failed and the ENTIRE page went inert with nothing on screen to say why.
// That happened in production on 2026-08-01.
//
// So these tests pin the three things that have to stay simultaneously true:
//   1. every module a page can reach is in the map,
//   2. the map is parsed before the first module is fetched, and
//   3. the CSP authorizes the map's exact bytes without ever opening the door
//      to inline scripts generally.
// Any one of them silently regressing brings the whole failure mode back.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/web"
)

// parseImportMap pulls the JSON body back out of the rendered element.
func parseImportMap(t *testing.T, a *Assets) map[string]string {
	t.Helper()
	el := string(a.ImportMapScript())
	const open = `<script type="importmap">`
	const closeTag = `</script>`
	require.True(t, strings.HasPrefix(el, open), "import map element must open with %s", open)
	require.True(t, strings.HasSuffix(el, closeTag), "import map element must close with %s", closeTag)
	body := strings.TrimSuffix(strings.TrimPrefix(el, open), closeTag)

	var parsed struct {
		Imports map[string]string `json:"imports"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed), "import map body must be valid JSON")
	return parsed.Imports
}

// TestImportMapCoversEveryModule: every .mjs that ships is mapped to its
// fingerprinted twin. A module missing from the map keeps resolving to its
// logical URL, which is exactly the stale-sibling hazard.
func TestImportMapCoversEveryModule(t *testing.T) {
	assets, err := NewAssets(web.Files)
	require.NoError(t, err)
	imports := parseImportMap(t, assets)

	var modules []string
	require.NoError(t, fs.WalkDir(web.Files, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".mjs") {
			return err
		}
		modules = append(modules, "/"+p)
		return nil
	}))
	require.NotEmpty(t, modules, "no .mjs assets found — the walk is wrong, not the map")

	for _, logical := range modules {
		hashed, ok := imports[logical]
		if !assert.Truef(t, ok, "%s is not in the import map", logical) {
			continue
		}
		assert.Equalf(t, assets.AssetPath(logical), hashed,
			"%s must map to its fingerprinted path", logical)
		assert.NotEqualf(t, logical, hashed,
			"%s mapped to itself — the fingerprint index did not resolve it", logical)
	}

	// Nothing that is not a module belongs here: import maps govern ES-module
	// resolution only, and a stray .css/.js key would be silently inert.
	for key := range imports {
		assert.Truef(t, strings.HasSuffix(key, ".mjs"),
			"import map contains a non-module key %q", key)
	}
}

// TestImportMapCoversEverySourceSpecifier: the relative specifiers actually
// written in the shipped JS all resolve to a mapped entry. This is the test
// that fails when someone adds a new module and the map stops being complete.
func TestImportMapCoversEverySourceSpecifier(t *testing.T) {
	assets, err := NewAssets(web.Files)
	require.NoError(t, err)
	imports := parseImportMap(t, assets)

	// Static `from './x.mjs'` and dynamic `import('./x.mjs')` alike.
	specifier := regexp.MustCompile(`(?:from|import)\s*\(?\s*'(\.{1,2}/[^']+\.mjs)'`)

	require.NoError(t, fs.WalkDir(web.Files, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".mjs") {
			return err
		}
		body, err := fs.ReadFile(web.Files, p)
		if err != nil {
			return err
		}
		for _, m := range specifier.FindAllSubmatch(body, -1) {
			// Resolve the specifier the way the browser does: relative to the
			// importing module's own directory.
			resolved := path.Join("/"+path.Dir(p), string(m[1]))
			assert.Containsf(t, imports, resolved,
				"%s imports %q (resolves to %s), which is not in the import map",
				p, m[1], resolved)
		}
		return nil
	}))
}

// TestImportMapPrecedesEveryModuleLoad: an import map is only consulted for
// modules fetched after it is parsed. If a page's module <script> ever moved
// above it, that page would resolve its imports against logical URLs again —
// and the regression would be invisible in every other test here.
func TestImportMapPrecedesEveryModuleLoad(t *testing.T) {
	assets, rend := newTestShell(t)
	importMap := string(assets.ImportMapScript())
	require.NotEmpty(t, importMap)

	for _, page := range []string{
		"pages/landing", "pages/conversation", "pages/history",
		"pages/memory", "pages/personas", "pages/downloads",
	} {
		var buf bytes.Buffer
		require.NoErrorf(t, rend.Render(&buf, page, nil), "render %s", page)
		html := buf.String()

		mapAt := strings.Index(html, importMap)
		if !assert.GreaterOrEqualf(t, mapAt, 0,
			"%s does not carry the import map — html/template escaped it, or base.html dropped it", page) {
			continue
		}
		firstModule := strings.Index(html, `<script type="module"`)
		if firstModule >= 0 {
			assert.Lessf(t, mapAt, firstModule,
				"%s loads a module before the import map is parsed", page)
		}
	}
}

// TestImportMapCSPHashMatchesTheRenderedBytes: the hash is computed over the
// map's body at startup and must still describe what the template emits. A
// mismatch does not degrade — the browser refuses the element outright and the
// page silently loses every fingerprinted resolution.
func TestImportMapCSPHashMatchesTheRenderedBytes(t *testing.T) {
	assets, err := NewAssets(web.Files)
	require.NoError(t, err)

	el := string(assets.ImportMapScript())
	body := strings.TrimSuffix(
		strings.TrimPrefix(el, `<script type="importmap">`), `</script>`)
	sum := sha256.Sum256([]byte(body))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"

	assert.Equal(t, want, assets.ImportMapCSPHash(),
		"the CSP hash must be over the exact bytes ImportMapScript emits")
}

// TestPageCSPCarriesTheImportMapHashWithoutUnsafeInline: the hash goes INSIDE
// script-src (appending to the policy string would land it in frame-ancestors),
// and it is a hash precisely so 'unsafe-inline' never has to be granted.
func TestPageCSPCarriesTheImportMapHashWithoutUnsafeInline(t *testing.T) {
	assets, err := NewAssets(web.Files)
	require.NoError(t, err)
	hash := assets.ImportMapCSPHash()
	require.NotEmpty(t, hash)

	csp := pageCSPWith(hash)

	assert.Contains(t, csp, pageCSPScriptSrc+" "+hash,
		"the hash must extend script-src, not some later directive")
	assert.Contains(t, csp, "frame-ancestors 'none'",
		"the rest of the policy must survive the splice")

	// Scoped to script-src on purpose: style-src legitimately carries
	// 'unsafe-inline' (static markup uses style attributes), so a
	// whole-policy check here would be asserting the wrong thing.
	var scriptSrc string
	for _, directive := range strings.Split(csp, "; ") {
		if strings.HasPrefix(directive, "script-src ") {
			scriptSrc = directive
		}
	}
	require.NotEmpty(t, scriptSrc, "policy lost its script-src directive")
	assert.NotContains(t, scriptSrc, "'unsafe-inline'",
		"script-src must never gain 'unsafe-inline' — the hash is what makes that unnecessary")
	assert.Contains(t, scriptSrc, hash, "script-src must carry the import map hash")

	// No assets, no inline element to authorize, no hash.
	assert.Equal(t, pageCSP, pageCSPWith(""))
}
