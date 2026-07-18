package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// newSettingsAPIApp wires the settings JSON handlers behind a stub auth
// middleware (locals userId=u1) over a FakeDynamo-backed store — the
// same shape RegisterSettingsRoutes mounts in production, minus the SSR
// page and real auth extraction.
func newSettingsAPIApp(t *testing.T) (*fiber.App, *Deps, *testutil.FakeDynamo) {
	t.Helper()
	fake := testutil.NewFakeDynamo()
	deps := &Deps{
		Store: store.NewWithClient(fake, "live-ninja"),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		return c.Next()
	})
	app.Get("/api/v1/settings", handleGetSettings(deps))
	app.Put("/api/v1/settings", handlePutSettings(deps))
	return app, deps, fake
}

func doJSON(t *testing.T, app *fiber.App, method, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, url, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("response is not JSON: %v\n%s", err, raw)
		}
	}
	return resp, out
}

// interceptShadowPublish swaps the package-level fan-out hook for a
// recorder, restoring it when the test ends.
func interceptShadowPublish(t *testing.T) *[]struct {
	UserID  string
	Version int64
	Doc     map[string]any
} {
	t.Helper()
	var calls []struct {
		UserID  string
		Version int64
		Doc     map[string]any
	}
	orig := publishSettingsShadow
	publishSettingsShadow = func(ctx context.Context, deps *Deps, userID string, doc map[string]any, version int64) {
		calls = append(calls, struct {
			UserID  string
			Version int64
			Doc     map[string]any
		}{userID, version, doc})
	}
	t.Cleanup(func() { publishSettingsShadow = orig })
	return &calls
}

func TestGetSettingsSinceFastPath(t *testing.T) {
	app, _, _ := newSettingsAPIApp(t)

	// Nothing stored: synthesized defaults at version 1. A caller already
	// holding v1 gets the cheap {changed:false} answer with no document.
	resp, out := doJSON(t, app, "GET", "/api/v1/settings?since=1", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if out["changed"] != false || out["version"] != float64(1) {
		t.Errorf("want {changed:false, version:1}, got %v", out)
	}
	if _, present := out["settings"]; present {
		t.Errorf("fast path must not carry the settings document")
	}
}

func TestGetSettingsSinceDeliversNewerDoc(t *testing.T) {
	app, _, _ := newSettingsAPIApp(t)

	resp, out := doJSON(t, app, "GET", "/api/v1/settings?since=0", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if out["changed"] != true || out["version"] != float64(1) {
		t.Errorf("want {changed:true, version:1}, got changed=%v version=%v", out["changed"], out["version"])
	}
	settings, ok := out["settings"].(map[string]any)
	if !ok || settings["voice"] != "cedar" {
		t.Errorf("expected the full document with voice cedar, got %v", out["settings"])
	}
}

func TestGetSettingsSinceInvalid(t *testing.T) {
	app, _, _ := newSettingsAPIApp(t)
	for _, q := range []string{"abc", "-1", "1.5"} {
		resp, _ := doJSON(t, app, "GET", "/api/v1/settings?since="+q, nil)
		if resp.StatusCode != 400 {
			t.Errorf("since=%s: status = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestGetSettingsWithoutSinceUnchanged(t *testing.T) {
	app, _, _ := newSettingsAPIApp(t)
	resp, out := doJSON(t, app, "GET", "/api/v1/settings", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Bare document — not the reconcile envelope.
	if _, present := out["changed"]; present {
		t.Errorf("plain GET must return the bare document, got %v", out)
	}
	if out["voice"] != "cedar" || out["version"] != float64(1) {
		t.Errorf("unexpected document: %v", out)
	}
}

func TestPutSettingsPublishesShadowAndBumpsSince(t *testing.T) {
	app, _, _ := newSettingsAPIApp(t)
	calls := interceptShadowPublish(t)

	doc := store.DefaultSettings()
	doc["voice"] = "marin"
	resp, out := doJSON(t, app, "PUT", "/api/v1/settings",
		map[string]any{"settings": doc, "version": 1})
	if resp.StatusCode != 200 {
		t.Fatalf("PUT status = %d: %v", resp.StatusCode, out)
	}
	if out["version"] != float64(2) {
		t.Errorf("PUT version = %v, want 2", out["version"])
	}

	if len(*calls) != 1 {
		t.Fatalf("shadow fan-out calls = %d, want 1", len(*calls))
	}
	call := (*calls)[0]
	if call.UserID != "u1" || call.Version != 2 || call.Doc["voice"] != "marin" {
		t.Errorf("fan-out call = %+v, want u1/v2/marin", call)
	}

	// The reconcile poll now sees the new version...
	_, out = doJSON(t, app, "GET", "/api/v1/settings?since=1", nil)
	if out["changed"] != true || out["version"] != float64(2) {
		t.Errorf("post-PUT since=1 = %v, want changed v2", out)
	}
	// ...and a caller already at v2 is back on the fast path.
	_, out = doJSON(t, app, "GET", "/api/v1/settings?since=2", nil)
	if out["changed"] != false {
		t.Errorf("since=2 after v2 write should be unchanged, got %v", out)
	}
}

func TestPutSettingsConflictDoesNotPublish(t *testing.T) {
	app, _, _ := newSettingsAPIApp(t)
	calls := interceptShadowPublish(t)

	// First write lands v2.
	resp, _ := doJSON(t, app, "PUT", "/api/v1/settings",
		map[string]any{"settings": store.DefaultSettings(), "version": 1})
	if resp.StatusCode != 200 {
		t.Fatalf("seed PUT status = %d", resp.StatusCode)
	}

	// A stale writer still expecting v1 must 409 and must NOT fan out.
	resp, out := doJSON(t, app, "PUT", "/api/v1/settings",
		map[string]any{"settings": store.DefaultSettings(), "version": 1})
	if resp.StatusCode != 409 || out["error"] != "version_conflict" {
		t.Fatalf("stale PUT = %d %v, want 409 version_conflict", resp.StatusCode, out)
	}
	if len(*calls) != 1 {
		t.Errorf("conflicted PUT must not publish (calls = %d, want only the seed's 1)", len(*calls))
	}
}
