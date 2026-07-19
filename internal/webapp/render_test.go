// Render smoke tests for the SSR shell (pages_routes.go + assets.go):
// every page template must execute against a representative bind, the
// asset() func must resolve fingerprinted URLs, and the security-header
// constants must reference the real spec values. Template execution
// errors otherwise only surface at request time in production.
package webapp

import (
	"bytes"
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/web"
)

func newTestShell(t *testing.T) (*Assets, *Renderer) {
	t.Helper()
	assets, err := NewAssets(web.Files)
	if err != nil {
		t.Fatalf("NewAssets: %v", err)
	}
	rend, err := NewRenderer(web.Files, assets)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	return assets, rend
}

func TestRenderAllPages(t *testing.T) {
	_, rend := newTestShell(t)

	cases := []struct {
		name     string
		bind     interface{}
		contains []string
	}{
		{
			name: "pages/landing",
			bind: nil,
			contains: []string{
				"<!doctype html>",
				`href="/auth/lwa/login"`,
				"Continue with Amazon",
				"<title>Live Ninja — your private realtime voice assistant</title>",
			},
		},
		{
			name: "pages/conversation",
			bind: nil,
			contains: []string{
				`id="pttBtn"`,
				`id="statePill"`,
				`id="statusText"`,
				`id="wakeToggle"`,
				`role="log"`,
				`id="composerInput"`,
				// Settings moved inline into the docked drawer (owner
				// 2026-07-19) — the standalone /settings page is gone, so
				// this page's render must carry its controls.
				`id="personaPreset"`,
				"Each persona carries its own voice and accent",
				"Sign out everywhere",
			},
		},
		{
			name: "pages/error",
			bind: errorPageView{Status: 404, Heading: "Page not found", Message: "That page doesn't exist."},
			contains: []string{
				"404",
				"Page not found",
				"Back to Live Ninja",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := rend.Render(&buf, tc.name, tc.bind); err != nil {
				t.Fatalf("render %s: %v", tc.name, err)
			}
			out := buf.String()
			for _, want := range tc.contains {
				if !strings.Contains(out, want) {
					t.Errorf("%s output missing %q", tc.name, want)
				}
			}
		})
	}
}

func TestRenderUnknownPageErrors(t *testing.T) {
	_, rend := newTestShell(t)
	var buf bytes.Buffer
	if err := rend.Render(&buf, "pages/nope", nil); err == nil {
		t.Fatal("expected error for unknown page template")
	}
}

func TestAssetFingerprinting(t *testing.T) {
	assets, rend := newTestShell(t)

	hashed := assets.AssetPath("/static/css/app.css")
	if hashed == "/static/css/app.css" {
		t.Fatalf("expected fingerprinted path for app.css, got %q", hashed)
	}
	if !strings.HasPrefix(hashed, "/static/css/app.") || !strings.HasSuffix(hashed, ".css") {
		t.Fatalf("unexpected fingerprinted shape: %q", hashed)
	}

	// Unknown logical paths pass through unchanged (concurrent-workstream
	// tolerance documented in assets.go).
	if got := assets.AssetPath("/static/js/not-written-yet.mjs"); got != "/static/js/not-written-yet.mjs" {
		t.Fatalf("unknown asset should pass through, got %q", got)
	}

	// Rendered pages must reference the fingerprinted stylesheet.
	var buf bytes.Buffer
	if err := rend.Render(&buf, "pages/landing", nil); err != nil {
		t.Fatalf("render landing: %v", err)
	}
	if !strings.Contains(buf.String(), hashed) {
		t.Fatalf("landing page does not reference fingerprinted stylesheet %q", hashed)
	}
}

func TestPageCSPMatchesSpec(t *testing.T) {
	for _, want := range []string{
		"connect-src 'self' https://api.openai.com",
		// wasm-unsafe-eval: WebAssembly compilation only (onnxruntime wake
		// word), never JS eval.
		"script-src 'self' 'wasm-unsafe-eval'",
		"media-src 'self' blob:",
		"worker-src 'self' blob:",
	} {
		if !strings.Contains(pageCSP, want) {
			t.Errorf("pageCSP missing %q", want)
		}
	}
	if strings.Contains(pageCSP, "script-src 'self' 'unsafe-inline'") {
		t.Error("pageCSP must never allow inline scripts")
	}
}
