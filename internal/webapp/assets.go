// Asset fingerprinting + serving for the SSR shell (WS-D, plan.md M3
// "fingerprinted static asset generator; no-cache HTML / immutable
// assets").
//
// At startup every file under the embedded static/ tree is hashed
// (sha256, first 12 hex chars) and indexed twice:
//
//   - its logical path   /static/css/app.css          → Cache-Control: no-cache
//     (served with a strong ETag so revalidations are 304s — this is the
//     path JS modules and the service worker reference, since only
//     templates can call asset())
//   - its hashed path    /static/css/app.<hash>.css   → Cache-Control:
//     public, max-age=31536000, immutable
//
// Templates resolve logical → hashed via the asset() template func
// (pages_routes.go wires it in). asset() falls back to the logical path
// for files that don't exist yet — sibling workstreams add JS modules to
// web/static/js/ concurrently, and a missing module must degrade to a
// 404 on the network tab, not a template execution failure that takes
// the whole page down.
package webapp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"path"
	"strings"

	"github.com/gofiber/fiber/v2"
)

type assetEntry struct {
	body        []byte
	contentType string
	immutable   bool
	etag        string
}

// Assets is the startup-built index of every embedded static file.
type Assets struct {
	fsys fs.FS
	// routes maps a request path (logical AND hashed) to its entry.
	routes map[string]*assetEntry
	// hashed maps a logical request path to its fingerprinted variant.
	hashed map[string]string
	// importMapJSON is the exact <script type="importmap"> body rendered
	// into every page; importMapCSPHash is the CSP source expression that
	// authorizes those exact bytes. See buildImportMap.
	importMapJSON    string
	importMapCSPHash string
}

// NewAssets walks the embedded web FS's static/ tree and builds the
// fingerprint index. The FS root must contain "static" (web.Files does).
func NewAssets(fsys fs.FS) (*Assets, error) {
	a := &Assets{
		fsys:   fsys,
		routes: make(map[string]*assetEntry),
		hashed: make(map[string]string),
	}
	err := fs.WalkDir(fsys, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		hash := hex.EncodeToString(sum[:])[:12]

		logical := "/" + p
		entry := &assetEntry{
			body:        body,
			contentType: contentTypeFor(p),
			etag:        `"` + hash + `"`,
		}
		a.routes[logical] = entry

		ext := path.Ext(p)
		hashedPath := "/" + strings.TrimSuffix(p, ext) + "." + hash + ext
		imm := *entry
		imm.immutable = true
		a.routes[hashedPath] = &imm
		a.hashed[logical] = hashedPath
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("webapp: index static assets: %w", err)
	}
	a.buildImportMap()
	return a, nil
}

// buildImportMap makes every module-to-module edge resolve to a fingerprinted,
// content-addressed URL.
//
// The hazard it closes (found in production on 2026-08-01, plan.md §4.3):
// templates load an ENTRY module by its fingerprinted URL via asset(), but the
// modules import their siblings by LOGICAL path (`from './wakeword.mjs'`).
// web/sw.js serves /static/* stale-while-revalidate on the stated premise that
// everything there is fingerprinted — which was true of the entry module and
// false of its siblings. So a deploy that changed conversation.mjs handed the
// browser a brand-new entry module alongside a pre-deploy sibling out of the
// service-worker cache, module linking failed, and the ENTIRE page went silently
// inert (every button dead; plain <a> links and CSS scrolling still working).
//
// An import map fixes it at the resolution step: the specifier stays `./x.mjs`
// in the source, and the browser rewrites it to `/static/js/x.<hash>.mjs` before
// fetching. A cache hit on a content-addressed URL is by construction the exact
// bytes the importer was built against, so stale-while-revalidate becomes as
// safe as sw.js already claims it is.
//
// Only .mjs is mapped. Import maps govern ES-module resolution and nothing
// else: theme.js is a classic script, and wakeword-worklet.js is loaded through
// audioWorklet.addModule(), which import maps deliberately do not cover (a
// worklet has its own module map). The worklet is import-free, so it cannot
// fail to LINK — it is out of scope here, not overlooked.
//
// Browsers without import-map support ignore the element and resolve the
// logical specifier, i.e. they degrade to the pre-2026-08-01 behaviour rather
// than breaking.
func (a *Assets) buildImportMap() {
	imports := make(map[string]string, len(a.hashed))
	for logical, hashed := range a.hashed {
		if strings.HasSuffix(logical, ".mjs") {
			imports[logical] = hashed
		}
	}
	// json.Marshal sorts map keys, so the bytes are stable for a given set of
	// assets — which is what lets the CSP hash below be computed once here and
	// still match what the template renders on every request.
	body, err := json.Marshal(struct {
		Imports map[string]string `json:"imports"`
	}{Imports: imports})
	if err != nil {
		// Unreachable: the value is map[string]string. Degrade to no import
		// map rather than failing startup — that is the old behaviour, which
		// works, instead of a site that will not boot.
		return
	}
	a.importMapJSON = string(body)
	sum := sha256.Sum256(body)
	a.importMapCSPHash = "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// ImportMapScript is the whole <script type="importmap"> element, ready to be
// emitted verbatim into <head> ABOVE the first module load. It is returned as a
// complete element rather than as its body so the exact bytes the CSP hash was
// computed over are the exact bytes that reach the browser.
func (a *Assets) ImportMapScript() template.HTML {
	if a.importMapJSON == "" {
		return ""
	}
	// The content is server-generated JSON over our own asset paths — no user
	// input reaches it — and "</script>" cannot appear in a JSON string of
	// slash-delimited paths, since json.Marshal escapes nothing here that would
	// reintroduce it.
	return template.HTML(`<script type="importmap">` + a.importMapJSON + `</script>`)
}

// ImportMapCSPHash is the script-src source expression authorizing the import
// map element, e.g. "'sha256-…'". Empty when there is no map to authorize.
//
// A hash, deliberately, not 'unsafe-inline': the page CSP forbids inline
// scripts and TestPageCSPMatchesSpec pins that it always will. External import
// maps were removed from the HTML spec and are implemented nowhere, so a hash
// source is the only way to ship one under a strict policy.
func (a *Assets) ImportMapCSPHash() string { return a.importMapCSPHash }

// AssetPath resolves a logical asset path ("/static/css/app.css") to its
// fingerprinted URL. Unknown paths are returned unchanged (see package
// comment — concurrent workstreams may reference modules that land in a
// later commit; those degrade to a plain 404 rather than a render error).
func (a *Assets) AssetPath(logical string) string {
	if h, ok := a.hashed[logical]; ok {
		return h
	}
	return logical
}

// Handler serves /static/* from the in-memory index: hashed paths as
// immutable, logical paths no-cache with ETag revalidation.
func (a *Assets) Handler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		entry, ok := a.routes[c.Path()]
		if !ok {
			return fiber.ErrNotFound
		}
		c.Set(fiber.HeaderETag, entry.etag)
		if match := c.Get(fiber.HeaderIfNoneMatch); match != "" && strings.Contains(match, entry.etag) {
			return c.SendStatus(fiber.StatusNotModified)
		}
		c.Set(fiber.HeaderContentType, entry.contentType)
		c.Set("X-Content-Type-Options", "nosniff")
		if entry.immutable {
			c.Set(fiber.HeaderCacheControl, "public, max-age=31536000, immutable")
		} else {
			c.Set(fiber.HeaderCacheControl, "no-cache")
		}
		return c.Send(entry.body)
	}
}

// FileHandler serves one file from the embedded FS root at a fixed route
// — used for /sw.js, which must live at the site root so its service-
// worker scope is "/" (a /static/-scoped worker could not control page
// navigations). Always no-cache + ETag: a stale service worker is a
// deploy hazard.
func (a *Assets) FileHandler(fsPath, contentType string) (fiber.Handler, error) {
	body, err := fs.ReadFile(a.fsys, fsPath)
	if err != nil {
		return nil, fmt.Errorf("webapp: read %s: %w", fsPath, err)
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:])[:12] + `"`
	return func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderETag, etag)
		if match := c.Get(fiber.HeaderIfNoneMatch); match != "" && strings.Contains(match, etag) {
			return c.SendStatus(fiber.StatusNotModified)
		}
		c.Set(fiber.HeaderContentType, contentType)
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set(fiber.HeaderCacheControl, "no-cache")
		return c.Send(body)
	}, nil
}

// contentTypes pins the types we serve regardless of OS mime databases
// (mime.TypeByExtension consults the registry on Windows and /etc/mime.types
// on Linux — not reproducible across dev and Lambda).
var contentTypes = map[string]string{
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".map":         "application/json",
	".json":        "application/json; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".ico":         "image/x-icon",
	".wasm":        "application/wasm",
	".onnx":        "application/octet-stream",
	".tflite":      "application/octet-stream",
	".txt":         "text/plain; charset=utf-8",
	".html":        "text/html; charset=utf-8",
	".woff2":       "font/woff2",
}

func contentTypeFor(p string) string {
	ext := strings.ToLower(path.Ext(p))
	if ct, ok := contentTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
