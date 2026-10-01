package webapp

// Route-level tests for POST /api/v1/location/current (location_routes.go)
// over a FakeDynamo-backed store. Only the branches that never reach the
// network (validation, the home and same-spot dedupes, the GPS clear) are
// covered here; the coordinate→timezone write is covered in
// internal/tools/location_test.go.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

func newLocationAPIApp(t *testing.T) (*fiber.App, *store.Store) {
	t.Helper()
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry, err := tools.NewRegistry(&tools.Deps{
		Store:       st,
		Log:         log,
		Reauthorize: func(context.Context, string) error { return nil },
	})
	if err != nil {
		t.Fatalf("tools.NewRegistry: %v", err)
	}
	deps := &Deps{Store: st, Log: log}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		return c.Next()
	})
	app.Post("/api/v1/location/current", handleReportCurrentLocation(deps, registry))
	return app, st
}

var testHome = store.Location{Label: "Huntersville, NC", City: "Huntersville",
	Lat: 35.4107, Lon: -80.8428, Timezone: "America/New_York"}

func seedHome(t *testing.T, st *store.Store) {
	t.Helper()
	doc, err := st.GetSettings(context.Background(), "u1")
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	profile, _ := doc["profile"].(map[string]any)
	profile["homeLocation"] = map[string]any{
		"label": testHome.Label, "city": testHome.City,
		"lat": testHome.Lat, "lon": testHome.Lon, "timezone": testHome.Timezone,
	}
	version := int64(1)
	switch n := doc["version"].(type) {
	case float64:
		version = int64(n)
	case int:
		version = int64(n)
	case int64:
		version = n
	}
	if _, err := st.PutSettings(context.Background(), "u1", doc, version); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}
}

func TestReportCurrentLocationRejectsOutOfRange(t *testing.T) {
	app, _ := newLocationAPIApp(t)
	for _, body := range []map[string]any{
		{"latitude": 90.01, "longitude": 0.5},
		{"latitude": 10, "longitude": -180.5},
	} {
		resp, out := doJSON(t, app, http.MethodPost, "/api/v1/location/current", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v: status = %d, want 400 (%v)", body, resp.StatusCode, out)
		}
	}
}

func TestReportCurrentLocationAtHomeIsNoOp(t *testing.T) {
	app, st := newLocationAPIApp(t)
	seedHome(t, st)
	resp, out := doJSON(t, app, http.MethodPost, "/api/v1/location/current",
		map[string]any{"latitude": testHome.Lat + 0.001, "longitude": testHome.Lon})
	if resp.StatusCode != http.StatusOK || out["status"] != "unchanged" {
		t.Fatalf("fix at home with no current location: %d %v, want 200 unchanged", resp.StatusCode, out)
	}
	p, _ := st.GetProfile(context.Background(), "u1")
	if p.Current().Resolved() {
		t.Errorf("a fix at home must not set a current location, got %+v", p.Current())
	}
}

func TestReportCurrentLocationAtHomeKeepsSpokenLocation(t *testing.T) {
	app, st := newLocationAPIApp(t)
	seedHome(t, st)
	denver := store.Location{Label: "Denver, Colorado", City: "Denver",
		Lat: 39.7392, Lon: -104.9903, Timezone: "America/Denver"}
	if _, err := st.SetCurrentLocation(context.Background(), "u1", denver,
		store.CurrentLocationSourceCommand, time.Now()); err != nil {
		t.Fatalf("SetCurrentLocation: %v", err)
	}
	resp, out := doJSON(t, app, http.MethodPost, "/api/v1/location/current",
		map[string]any{"latitude": testHome.Lat, "longitude": testHome.Lon})
	if resp.StatusCode != http.StatusOK || out["status"] != "unchanged" {
		t.Fatalf("home fix vs spoken location: %d %v, want 200 unchanged", resp.StatusCode, out)
	}
	p, _ := st.GetProfile(context.Background(), "u1")
	if p.Current().Label != denver.Label {
		t.Errorf("a device at home must not clear a spoken location, got %+v", p.Current())
	}
}

func TestReportCurrentLocationBackHomeClearsGPSLocation(t *testing.T) {
	app, st := newLocationAPIApp(t)
	seedHome(t, st)
	trip := store.Location{Label: "39.7392, -104.9903", Lat: 39.7392, Lon: -104.9903,
		Timezone: "America/Denver"}
	if _, err := st.SetCurrentLocation(context.Background(), "u1", trip,
		store.CurrentLocationSourceGPS, time.Now()); err != nil {
		t.Fatalf("SetCurrentLocation: %v", err)
	}
	resp, out := doJSON(t, app, http.MethodPost, "/api/v1/location/current",
		map[string]any{"latitude": testHome.Lat, "longitude": testHome.Lon})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("back-home fix: status = %d (%v)", resp.StatusCode, out)
	}
	p, _ := st.GetProfile(context.Background(), "u1")
	if p.Current().Resolved() {
		t.Errorf("a GPS fix back at home must clear the GPS trip location, got %+v", p.Current())
	}
}
