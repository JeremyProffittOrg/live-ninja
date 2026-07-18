package webapp

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestParseClientVersion(t *testing.T) {
	cases := []struct {
		raw     string
		wantOK  bool
		surface string
		major   int
		minor   int
		patch   int
		build   string
	}{
		{"web/0.9.0+g1a2b3c4", true, "web", 0, 9, 0, "g1a2b3c4"},
		{"android/2.1.0+r48", true, "android", 2, 1, 0, "r48"},
		{"m5stack/1.4.2+20260717-1", true, "m5stack", 1, 4, 2, "20260717-1"},
		{"", false, "", 0, 0, 0, ""},
		{"desktop/1.0.0+abc", false, "", 0, 0, 0, ""}, // unrecognized surface
		{"web/1.0+abc", false, "", 0, 0, 0, ""},       // not MAJOR.MINOR.PATCH
		{"web/1.0.0", false, "", 0, 0, 0, ""},         // missing +build
		{"web/1.0.0+", false, "", 0, 0, 0, ""},        // empty build
		{"web/v1.0.0+abc", false, "", 0, 0, 0, ""},    // leading "v" not allowed
	}
	for _, tc := range cases {
		cv, ok := parseClientVersion(tc.raw)
		if ok != tc.wantOK {
			t.Errorf("parseClientVersion(%q) ok = %v, want %v", tc.raw, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if cv.surface != tc.surface || cv.major != tc.major || cv.minor != tc.minor || cv.patch != tc.patch || cv.build != tc.build {
			t.Errorf("parseClientVersion(%q) = %+v, want surface=%s %d.%d.%d build=%s",
				tc.raw, cv, tc.surface, tc.major, tc.minor, tc.patch, tc.build)
		}
	}
}

func TestSemverLess(t *testing.T) {
	cases := []struct {
		a, b [3]int
		want bool
	}{
		{[3]int{0, 4, 9}, [3]int{0, 5, 0}, true},
		{[3]int{0, 5, 0}, [3]int{0, 5, 0}, false},
		{[3]int{0, 5, 1}, [3]int{0, 5, 0}, false},
		{[3]int{1, 0, 0}, [3]int{0, 9, 9}, false},
		{[3]int{0, 9, 9}, [3]int{1, 0, 0}, true},
	}
	for _, tc := range cases {
		got := semverLess(tc.a[0], tc.a[1], tc.a[2], tc.b[0], tc.b[1], tc.b[2])
		if got != tc.want {
			t.Errorf("semverLess(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCompatStatus(t *testing.T) {
	versions := compatVersionSet{
		min:         map[string]string{"web": "0.5.0", "android": "1.0.0", "m5stack": "1.0.0"},
		recommended: map[string]string{"web": "0.9.0", "android": "2.1.0", "m5stack": "1.4.2"},
	}

	// Missing/malformed header: unsupported, generic message.
	status, msg := compatStatus(versions, clientVersion{}, false)
	if status != "unsupported" || msg == "" {
		t.Errorf("no header: status=%q msg=%q, want unsupported + non-empty message", status, msg)
	}

	// Below min -> unsupported.
	status, msg = compatStatus(versions, clientVersion{surface: "android", major: 0, minor: 9, patch: 0}, true)
	if status != "unsupported" || msg == "" {
		t.Errorf("below min: status=%q msg=%q, want unsupported", status, msg)
	}

	// At min, at recommended major -> supported.
	status, msg = compatStatus(versions, clientVersion{surface: "web", major: 0, minor: 5, patch: 0}, true)
	if status != "supported" || msg != "" {
		t.Errorf("at min: status=%q msg=%q, want supported + empty message", status, msg)
	}

	// At min, exactly one major behind recommended -> still supported
	// ("more than one MAJOR version" per headers.md, not "any").
	status, msg = compatStatus(versions, clientVersion{surface: "android", major: 1, minor: 0, patch: 0}, true)
	if status != "supported" {
		t.Errorf("android one major behind recommended (2-1=1, not >1): status=%q, want supported", status)
	}

	// At/above min but more than one major behind recommended ->
	// deprecated.
	depVersions := compatVersionSet{
		min:         map[string]string{"android": "1.0.0"},
		recommended: map[string]string{"android": "3.5.0"},
	}
	status, msg = compatStatus(depVersions, clientVersion{surface: "android", major: 1, minor: 2, patch: 0}, true)
	if status != "deprecated" || msg == "" {
		t.Errorf("android two majors behind recommended: status=%q msg=%q, want deprecated + non-empty message", status, msg)
	}
}

func TestHandleCompatRoute(t *testing.T) {
	t.Setenv("MIN_SUPPORTED_CLIENT_VERSION_ANDROID", "1.0.0")
	t.Setenv("RECOMMENDED_CLIENT_VERSION_ANDROID", "2.1.0")

	app := fiber.New()
	RegisterCompatRoute(app, &Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	// Supported client.
	req := httptest.NewRequest(http.MethodGet, "/v1/compat", nil)
	req.Header.Set("X-LN-Client", "android/2.1.0+r48")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// No X-LN-Client at all -> still 200 (compat is advisory, never 5xx),
	// but status field reports unsupported (can't infer surface/version).
	req2 := httptest.NewRequest(http.MethodGet, "/v1/compat", nil)
	resp2, err := app.Test(req2)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("no-header status = %d, want 200 (compat never errors the request)", resp2.StatusCode)
	}
}

func TestVersionMiddlewareSetsServerHeader(t *testing.T) {
	old := BuildVersion
	BuildVersion = "1.2.3+deadbee"
	defer func() { BuildVersion = old }()

	app := fiber.New()
	deps := &Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app.Use(VersionMiddleware(deps))
	app.Get("/anything", func(c *fiber.Ctx) error { return c.SendString("ok") })

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if got := resp.Header.Get("X-LN-Server"); got != "1.2.3+deadbee" {
		t.Errorf("X-LN-Server = %q, want 1.2.3+deadbee", got)
	}
}

func TestVersionMiddlewareGatesBelowMinExceptWeb(t *testing.T) {
	t.Setenv("MIN_SUPPORTED_CLIENT_VERSION_ANDROID", "1.0.0")
	t.Setenv("MIN_SUPPORTED_CLIENT_VERSION_WEB", "0.5.0")

	app := fiber.New()
	deps := &Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app.Use(VersionMiddleware(deps))
	app.Get("/api/v1/me", func(c *fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/v1/compat", func(c *fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/auth/refresh", func(c *fiber.Ctx) error { return c.SendString("ok") })

	// Android below min on a normal route -> 426.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("X-LN-Client", "android/0.5.0+r1")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("android below min: status = %d, want 426", resp.StatusCode)
	}

	// Web below min on the SAME route -> never gated (200).
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req2.Header.Set("X-LN-Client", "web/0.1.0+abc")
	resp2, err := app.Test(req2)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("web below min: status = %d, want 200 (never gated)", resp2.StatusCode)
	}

	// Android below min on the compat route -> exempt (200), so a stuck
	// device can still learn why it's unsupported.
	req3 := httptest.NewRequest(http.MethodGet, "/v1/compat", nil)
	req3.Header.Set("X-LN-Client", "android/0.5.0+r1")
	resp3, err := app.Test(req3)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("android below min on /v1/compat: status = %d, want 200 (exempt)", resp3.StatusCode)
	}

	// Android below min on /auth/refresh -> exempt (200), bootstrap must
	// stay reachable.
	req4 := httptest.NewRequest(http.MethodGet, "/auth/refresh", nil)
	req4.Header.Set("X-LN-Client", "android/0.5.0+r1")
	resp4, err := app.Test(req4)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("android below min on /auth/refresh: status = %d, want 200 (exempt)", resp4.StatusCode)
	}

	// Malformed/missing header -> never gated, even on a normal route.
	req5 := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	resp5, err := app.Test(req5)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp5.StatusCode != http.StatusOK {
		t.Fatalf("missing header: status = %d, want 200 (never gated)", resp5.StatusCode)
	}
}
