// SSR page shell (WS-D): the html/template Views engine, the page routes
// (/ landing, /conversation — /settings is mounted by settings_routes.go),
// the HTML-aware 404/error handlers, and the security-header middleware.
//
// Template engine decision (task brief: "Fiber html template engine or std
// html/template — pick, note"): std html/template, implemented as a
// fiber.Views engine so handlers use the ordinary c.Render(...) seam
// (settings_routes.go already renders through it). Fiber's html/v2 engine
// is a separate go.mod dependency that only wraps html/template; the std
// library gives the identical contextual auto-escaping (load-bearing for
// the settings JSON data island) with no new dependency.
//
// Every page template renders inside layouts/base.html. The bind passed to
// c.Render flows to the template unchanged (settings_routes.go binds its
// settingsPageView directly); per-page constants (title, nav path, body
// class) are injected as template funcs at parse time, so binds never need
// shell-specific fields.
package webapp

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"path"
	"reflect"
	"strings"

	"github.com/gofiber/fiber/v2"
)

// pageCSP is the strict page policy from docs/web-ui-spec.md §0:
// self + api.openai.com (SDP exchange) only; blob: for wake-word worklet
// modules and remote-audio object URLs; data: images for the CSS select
// chevron; inline styles allowed (style attributes in static markup),
// inline scripts never. 'wasm-unsafe-eval' permits ONLY WebAssembly
// compilation (never JS eval) — required for the vendored onnxruntime
// wake-word engine (wakeword.mjs), whose .wasm is served same-origin and
// SHA-256-pinned client-side before instantiation.
const pageCSP = "default-src 'self'; " +
	"script-src 'self' 'wasm-unsafe-eval'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	// The wakewords bucket hosts trained wake-word detectors fetched via
	// presigned S3 URLs (wakeword.mjs fetchVerified) — SHA-256-pinned
	// client-side, so the wildcard-free bucket host is the only extra origin.
	"connect-src 'self' https://api.openai.com https://live-ninja-wakewords-759775734231.s3.amazonaws.com https://live-ninja-wakewords-759775734231.s3.us-east-1.amazonaws.com; " +
	"media-src 'self' blob:; " +
	"worker-src 'self' blob:; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// pageMeta carries the per-page constants injected as template funcs.
type pageMeta struct {
	Title     string
	Path      string // nav highlight; "" = no nav entry
	BodyClass string
}

var pageMetas = map[string]pageMeta{
	"pages/landing":      {Title: "Live Ninja — your private realtime voice assistant", Path: "/"},
	"pages/conversation": {Title: "Conversation — Live Ninja", Path: "/conversation"},
	"pages/settings":     {Title: "Settings — Live Ninja", Path: "/settings"},
	"pages/downloads":    {Title: "Downloads — Live Ninja", Path: "/downloads"},
	"pages/memory":       {Title: "Memory — Live Ninja", Path: "/memory"},
	"pages/personas":     {Title: "Personas — Live Ninja", Path: "/personas"},
	"pages/history":      {Title: "History — Live Ninja", Path: "/history"},
	"pages/error":        {Title: "Live Ninja"},
}

// Renderer implements fiber.Views over the embedded templates: one
// template set per page (base layout + partials + the page file).
type Renderer struct {
	pages map[string]*template.Template
}

// NewRenderer parses layouts/ + partials/ once and clones the set per
// page file. It is constructed in cmd/web/main.go and passed to
// fiber.Config.Views.
func NewRenderer(fsys fs.FS, assets *Assets) (*Renderer, error) {
	root := template.New("root").Funcs(template.FuncMap{
		"asset":     assets.AssetPath,
		"themeAttr": themeAttrOf,
		// Per-page funcs get real values in the per-page clones below;
		// these defaults exist so the shared files parse.
		"pageTitle":     func() string { return "Live Ninja" },
		"pagePath":      func() string { return "" },
		"pageBodyClass": func() string { return "" },
	})
	root, err := root.ParseFS(fsys, "templates/layouts/*.html", "templates/partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("webapp: parse layouts/partials: %w", err)
	}

	pageFiles, err := fs.Glob(fsys, "templates/pages/*.html")
	if err != nil {
		return nil, fmt.Errorf("webapp: glob pages: %w", err)
	}
	if len(pageFiles) == 0 {
		return nil, fmt.Errorf("webapp: no page templates found under templates/pages")
	}

	r := &Renderer{pages: make(map[string]*template.Template, len(pageFiles))}
	for _, f := range pageFiles {
		name := "pages/" + strings.TrimSuffix(path.Base(f), ".html")
		meta := pageMetas[name]
		if meta.Title == "" {
			meta.Title = "Live Ninja"
		}

		t, err := root.Clone()
		if err != nil {
			return nil, fmt.Errorf("webapp: clone template set for %s: %w", name, err)
		}
		m := meta // capture per iteration
		t = t.Funcs(template.FuncMap{
			"pageTitle":     func() string { return m.Title },
			"pagePath":      func() string { return m.Path },
			"pageBodyClass": func() string { return m.BodyClass },
		})
		if t, err = t.ParseFS(fsys, f); err != nil {
			return nil, fmt.Errorf("webapp: parse %s: %w", f, err)
		}
		r.pages[name] = t
	}
	return r, nil
}

// Load implements fiber.Views; parsing happened in NewRenderer.
func (r *Renderer) Load() error { return nil }

// Render implements fiber.Views: executes the page's "base" template with
// the bind passed straight through. Buffered so a mid-render failure
// never leaks half a page.
func (r *Renderer) Render(w io.Writer, name string, bind interface{}, _ ...string) error {
	t, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("webapp: unknown page template %q", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", bind); err != nil {
		return fmt.Errorf("webapp: render %s: %w", name, err)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// themeAttrOf pulls a ThemeAttr string field off any struct bind (via
// reflection so binds without the field — or nil binds — render with no
// data-theme attribute and fall back to prefers-color-scheme).
func themeAttrOf(bind interface{}) string {
	if bind == nil {
		return ""
	}
	rv := reflect.ValueOf(bind)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return ""
	}
	f := rv.FieldByName("ThemeAttr")
	if f.IsValid() && f.Kind() == reflect.String {
		return f.String()
	}
	return ""
}

// errorPageView is the bind for pages/error.
type errorPageView struct {
	Status  int
	Heading string
	Message string
}

// RegisterPageRoutes mounts the SSR page routes that pages_routes.go owns:
// GET / (landing, or redirect to /conversation when signed in),
// GET /conversation and GET /downloads (both redirect to / when signed
// out). GET /settings is registered by RegisterSettingsRoutes
// (settings_routes.go) because its handler SSRs the settings document.
// Must be registered after ExtractAuthContext so the cookie/JWT auth gate
// sees the request context.
func RegisterPageRoutes(app *fiber.App, deps *Deps) {
	app.Get("/", handleLandingPage(deps))
	app.Get("/conversation", handleConversationPage(deps))
	app.Get("/downloads", handleDownloadsPage(deps))
	app.Get("/memory", handleClientDataPage(deps, "pages/memory"))
	app.Get("/history", handleClientDataPage(deps, "pages/history"))
	app.Get("/personas", handleClientDataPage(deps, "pages/personas"))
}

// webPageUser resolves the signed-in user for a plain browser page
// navigation: the authorizer/Bearer context first (ExtractAuthContext),
// then the HttpOnly web refresh cookie (read-only session validation —
// resolveWebSessionUser, settings_routes.go). "" = not signed in.
func webPageUser(c *fiber.Ctx, deps *Deps) string {
	if uid := UserID(c); uid != "" {
		return uid
	}
	return resolveWebSessionUser(c, deps)
}

func handleLandingPage(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// An authenticated visitor never sees the landing page (spec §1.2).
		if webPageUser(c, deps) != "" {
			return c.Redirect("/conversation", fiber.StatusFound)
		}
		c.Set(fiber.HeaderCacheControl, "no-cache")
		return c.Render("pages/landing", nil)
	}
}

func handleConversationPage(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if webPageUser(c, deps) == "" {
			return c.Redirect("/", fiber.StatusFound)
		}
		c.Set(fiber.HeaderCacheControl, "no-cache")
		return c.Render("pages/conversation", nil)
	}
}

// handleDownloadsPage serves the M9 Download Center shell (FR-DLV-05).
// The deliverable list itself is fetched client-side by downloads.mjs
// via GET /api/v1/deliverables (Query-backed, paginated) — the page
// renders a real loading state on first paint, so no SSR data island is
// needed here (unlike /settings, which SSRs its document by spec §3.2).
func handleDownloadsPage(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if webPageUser(c, deps) == "" {
			return c.Redirect("/", fiber.StatusFound)
		}
		c.Set(fiber.HeaderCacheControl, "no-cache")
		return c.Render("pages/downloads", nil)
	}
}

// handleClientDataPage serves the M10/M11 client-rendered shells (/memory
// and /history). Like /downloads, the data is fetched client-side
// (memory.mjs / history.mjs) against the Query-backed /api/v1 surface and
// each page renders a real loading state on first paint, so no SSR data
// island is needed.
func handleClientDataPage(deps *Deps, page string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if webPageUser(c, deps) == "" {
			return c.Redirect("/", fiber.StatusFound)
		}
		c.Set(fiber.HeaderCacheControl, "no-cache")
		return c.Render(page, nil)
	}
}

// wantsHTMLPage reports whether a request should get an HTML error page
// rather than the JSON envelope: browser-shaped GET/HEAD navigations
// outside the API/auth/static surfaces.
func wantsHTMLPage(c *fiber.Ctx) bool {
	switch c.Method() {
	case fiber.MethodGet, fiber.MethodHead:
	default:
		return false
	}
	p := c.Path()
	for _, prefix := range []string{"/api/", "/auth/", "/static/", "/.well-known/"} {
		if strings.HasPrefix(p, prefix) {
			return false
		}
	}
	return strings.Contains(c.Get(fiber.HeaderAccept), "text/html")
}

// NotFoundHandler is the catch-all: HTML error page for browser
// navigations, the pre-existing JSON envelope for everything else.
func NotFoundHandler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if wantsHTMLPage(c) {
			if err := c.Status(fiber.StatusNotFound).Render("pages/error", errorPageView{
				Status:  fiber.StatusNotFound,
				Heading: "Page not found",
				Message: "That page doesn't exist — it may have moved.",
			}); err == nil {
				return nil
			}
			// Render failure falls through to JSON.
		}
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "not found",
			"path":  c.Path(),
		})
	}
}

// ErrorHandler is the app-level fiber.ErrorHandler: HTML error page for
// browser navigations, the pre-existing {"error": ...} JSON otherwise.
func ErrorHandler() fiber.ErrorHandler {
	return func(c *fiber.Ctx, err error) error {
		code := fiber.StatusInternalServerError
		if fe, ok := err.(*fiber.Error); ok {
			code = fe.Code
		}
		if wantsHTMLPage(c) {
			heading, message := errorCopy(code)
			if rerr := c.Status(code).Render("pages/error", errorPageView{
				Status:  code,
				Heading: heading,
				Message: message,
			}); rerr == nil {
				return nil
			}
		}
		return c.Status(code).JSON(fiber.Map{"error": err.Error()})
	}
}

func errorCopy(code int) (heading, message string) {
	switch code {
	case fiber.StatusNotFound:
		return "Page not found", "That page doesn't exist — it may have moved."
	case fiber.StatusUnauthorized, fiber.StatusForbidden:
		return "Access denied", "You don't have access to that page. Try signing in again."
	default:
		return "Something went wrong", "An unexpected error occurred. Please try again in a moment."
	}
}

// SecurityHeaders applies the page CSP and security headers to every
// text/html response after the handler chain runs (covers the template
// pages AND auth_routes.go's htmlMessage responses), without touching
// JSON/static responses. Cache-Control is defaulted to no-cache for HTML
// only when a handler didn't set one.
func SecurityHeaders() fiber.Handler {
	return func(c *fiber.Ctx) error {
		err := c.Next()
		ct := string(c.Response().Header.ContentType())
		if strings.HasPrefix(ct, "text/html") {
			if len(c.Response().Header.Peek(fiber.HeaderContentSecurityPolicy)) == 0 {
				c.Set(fiber.HeaderContentSecurityPolicy, pageCSP)
			}
			if len(c.Response().Header.Peek(fiber.HeaderCacheControl)) == 0 {
				c.Set(fiber.HeaderCacheControl, "no-cache")
			}
			c.Set(fiber.HeaderReferrerPolicy, "same-origin")
			c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
		}
		return err
	}
}
