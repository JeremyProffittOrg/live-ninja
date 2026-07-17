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
	"encoding/hex"
	"fmt"
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
	return a, nil
}

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
