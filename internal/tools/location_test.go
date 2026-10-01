package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// fakeLocationUpstream serves the two Open-Meteo endpoints
// set_current_location calls: name search (geocode) and forecast (which,
// with timezone=auto, names the IANA zone at a coordinate).
type fakeLocationUpstream struct {
	results       []geoCandidate
	timezone      string
	forecastFails bool
	searches      atomic.Int32
	forecasts     atomic.Int32
	lastLatitude  string
	lastTZParam   string
}

func withLocationUpstream(t *testing.T, f *fakeLocationUpstream) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/forecast":
			f.forecasts.Add(1)
			f.lastLatitude = r.URL.Query().Get("latitude")
			f.lastTZParam = r.URL.Query().Get("timezone")
			if f.forecastFails {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"timezone": f.timezone})
		default:
			f.searches.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"results": f.results})
		}
	}))
	t.Cleanup(srv.Close)
	oldGeo, oldForecast := geocodeURL, forecastURL
	geocodeURL, forecastURL = srv.URL+"/search", srv.URL+"/forecast"
	t.Cleanup(func() { geocodeURL, forecastURL = oldGeo, oldForecast })
}

// locationDeps wires a real Store over the fake table and a profile loader
// that reads it back, so each test sees exactly what the tool persisted.
func locationDeps(now time.Time) *Deps {
	deps := newTestDeps()
	deps.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	deps.Now = func() time.Time { return now }
	deps.Profile = func(ctx context.Context, userID string) store.Profile {
		p, _ := deps.Store.GetProfile(ctx, userID)
		return p
	}
	return deps
}

func seedHome(t *testing.T, deps *Deps) {
	t.Helper()
	doc := store.DefaultSettings()
	doc["profile"].(map[string]any)["homeLocation"] = map[string]any{
		"label": charlotteNC.Label, "city": charlotteNC.City, "admin1": charlotteNC.Admin1,
		"country": charlotteNC.Country, "lat": charlotteNC.Lat, "lon": charlotteNC.Lon,
		"timezone": charlotteNC.Timezone,
	}
	_, err := deps.Store.PutSettings(context.Background(), "user-1", doc, 1)
	require.NoError(t, err)
}

var denverCandidate = geoCandidate{Name: "Denver", Admin1: "Colorado", Country: "United States",
	CountryCode: "US", Latitude: 39.7392, Longitude: -104.9903, Timezone: "America/Denver"}

// 2026-10-01 18:00 UTC is 12:00 PM MDT in Denver and 2:00 PM EDT at home.
var locationNow = time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)

func TestSetCurrentLocationByPlaceName(t *testing.T) {
	up := &fakeLocationUpstream{results: []geoCandidate{denverCandidate}}
	withLocationUpstream(t, up)
	deps := locationDeps(locationNow)
	seedHome(t, deps)

	out, terr := handleSetCurrentLocation(context.Background(), deps,
		Invocation{UserID: "user-1"}, map[string]any{"place": "Denver"})
	require.Nil(t, terr)
	assert.Equal(t, "set", out["status"])
	assert.Equal(t, store.CurrentLocationSourceCommand, out["source"])
	assert.Equal(t, "Thursday, October 1, 2026 at 12:00 PM MDT", out["localTime"])
	loc := out["location"].(map[string]any)
	assert.Equal(t, "Denver, Colorado, United States", loc["label"])
	assert.Equal(t, "America/Denver", loc["timezone"])
	assert.EqualValues(t, 0, up.forecasts.Load(), "the geocoder named the zone; no second lookup")

	p := deps.Profile(context.Background(), "user-1")
	require.NotNil(t, p.CurrentLocation)
	assert.Equal(t, "America/Denver", p.Timezone())
	assert.Equal(t, "Denver", p.CurrentLocation.City)
	assert.Equal(t, "2026-10-01T18:00:00Z", p.CurrentLocationSetAt)
	require.NotNil(t, p.HomeLocation, "home is untouched")
}

func TestSetCurrentLocationPlaceWithoutZoneLooksItUp(t *testing.T) {
	cand := denverCandidate
	cand.Timezone = ""
	up := &fakeLocationUpstream{results: []geoCandidate{cand}, timezone: "America/Denver"}
	withLocationUpstream(t, up)
	deps := locationDeps(locationNow)

	out, terr := handleSetCurrentLocation(context.Background(), deps,
		Invocation{UserID: "user-1"}, map[string]any{"place": "Denver", "label": "the conference hotel"})
	require.Nil(t, terr)
	assert.EqualValues(t, 1, up.forecasts.Load())
	assert.Equal(t, "auto", up.lastTZParam)
	loc := out["location"].(map[string]any)
	assert.Equal(t, "the conference hotel", loc["label"], "an explicit label wins")
	assert.Equal(t, "America/Denver", loc["timezone"])
}

func TestSetCurrentLocationByGPS(t *testing.T) {
	cases := []struct {
		name      string
		args      map[string]any
		wantLabel string
	}{
		{"coordinates label themselves", map[string]any{"latitude": 39.74, "longitude": -104.99},
			"39.7400, -104.9900"},
		{"explicit label", map[string]any{"latitude": 39.74, "longitude": -104.99, "label": "the cabin"},
			"the cabin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeLocationUpstream{timezone: "America/Denver"}
			withLocationUpstream(t, up)
			deps := locationDeps(locationNow)

			out, terr := handleSetCurrentLocation(context.Background(), deps,
				Invocation{UserID: "user-1"}, tc.args)
			require.Nil(t, terr)
			assert.Equal(t, store.CurrentLocationSourceGPS, out["source"])
			assert.Equal(t, "Thursday, October 1, 2026 at 12:00 PM MDT", out["localTime"])
			assert.EqualValues(t, 0, up.searches.Load(), "GPS never geocodes a name")
			assert.Equal(t, "39.7400", up.lastLatitude)
			loc := out["location"].(map[string]any)
			assert.Equal(t, tc.wantLabel, loc["label"])
			assert.InDelta(t, 39.74, loc["lat"], 1e-9)
			assert.InDelta(t, -104.99, loc["lon"], 1e-9)

			p := deps.Profile(context.Background(), "user-1")
			require.NotNil(t, p.CurrentLocation)
			assert.Equal(t, store.CurrentLocationSourceGPS, p.CurrentLocationSource)
			assert.Equal(t, "America/Denver", p.TimezoneOrDefault())
		})
	}
}

func TestSetCurrentLocationHomeClears(t *testing.T) {
	up := &fakeLocationUpstream{results: []geoCandidate{denverCandidate}}
	withLocationUpstream(t, up)
	deps := locationDeps(locationNow)
	seedHome(t, deps)
	ctx := context.Background()
	inv := Invocation{UserID: "user-1"}

	_, terr := handleSetCurrentLocation(ctx, deps, inv, map[string]any{"place": "Denver"})
	require.Nil(t, terr)

	out, terr := handleSetCurrentLocation(ctx, deps, inv, map[string]any{"home": true})
	require.Nil(t, terr)
	assert.Equal(t, "cleared", out["status"])
	assert.Equal(t, store.LocationKindHome, out["source"])
	assert.Equal(t, "Thursday, October 1, 2026 at 2:00 PM EDT", out["localTime"],
		"the clock goes back to home's zone")
	assert.Equal(t, charlotteNC.Label, out["location"].(map[string]any)["label"])
	assert.Nil(t, deps.Profile(ctx, "user-1").CurrentLocation)

	out, terr = handleSetCurrentLocation(ctx, deps, inv, map[string]any{"home": true})
	require.Nil(t, terr)
	assert.Equal(t, "already_home", out["status"])
}

// With no home on file, "I'm back home" still answers with a real clock —
// the default zone, said out loud — never UTC.
func TestSetCurrentLocationHomeWithoutHomeUsesDefaultZone(t *testing.T) {
	deps := locationDeps(locationNow)
	out, terr := handleSetCurrentLocation(context.Background(), deps,
		Invocation{UserID: "user-1"}, map[string]any{"home": true})
	require.Nil(t, terr)
	assert.Equal(t, "already_home", out["status"])
	assert.Equal(t, "Thursday, October 1, 2026 at 2:00 PM EDT", out["localTime"])
	assert.NotContains(t, out, "location")
	assert.Contains(t, out["note"], store.DefaultTimezone)
}

func TestSetCurrentLocationArgumentShapes(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"nothing", map[string]any{}},
		{"home false alone", map[string]any{"home": false}},
		{"place and gps", map[string]any{"place": "Denver", "latitude": 39.7, "longitude": -104.9}},
		{"place and home", map[string]any{"place": "Denver", "home": true}},
		{"gps and home", map[string]any{"latitude": 39.7, "longitude": -104.9, "home": true}},
		{"latitude alone", map[string]any{"latitude": 39.7}},
		{"longitude alone", map[string]any{"longitude": -104.9}},
		{"null island", map[string]any{"latitude": 0.0, "longitude": 0.0}},
		{"blank place", map[string]any{"place": "   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeLocationUpstream{results: []geoCandidate{denverCandidate}, timezone: "America/Denver"}
			withLocationUpstream(t, up)
			deps := locationDeps(locationNow)
			_, terr := handleSetCurrentLocation(context.Background(), deps,
				Invocation{UserID: "user-1"}, tc.args)
			require.NotNil(t, terr)
			assert.Equal(t, CodeInvalidArgs, terr.Code)
			assert.Nil(t, deps.Profile(context.Background(), "user-1").CurrentLocation,
				"a rejected call must store nothing")
		})
	}
}

// The schema gate enforces the coordinate ranges before the handler runs.
func TestSetCurrentLocationSchemaRejectsOutOfRangeCoordinates(t *testing.T) {
	r := newTestRegistry(t, locationDeps(locationNow))
	for _, args := range []map[string]any{
		{"latitude": 90.5, "longitude": 0.0},
		{"latitude": 10.0, "longitude": -180.5},
	} {
		inv := invocation("set_current_location", args)
		inv.IdempotencyKey = "k"
		res := r.Invoke(context.Background(), inv)
		require.False(t, res.OK)
		assert.Equal(t, CodeInvalidArgs, res.Error.Code)
	}
}

func TestSetCurrentLocationTimezoneLookupFailureStoresNothing(t *testing.T) {
	up := &fakeLocationUpstream{forecastFails: true}
	withLocationUpstream(t, up)
	deps := locationDeps(locationNow)
	_, terr := handleSetCurrentLocation(context.Background(), deps,
		Invocation{UserID: "user-1"}, map[string]any{"latitude": 39.74, "longitude": -104.99})
	require.NotNil(t, terr)
	assert.Equal(t, CodeUpstreamError, terr.Code)
	assert.Nil(t, deps.Profile(context.Background(), "user-1").CurrentLocation)
}

func TestSetCurrentLocationUnknownUpstreamZoneStoresNothing(t *testing.T) {
	up := &fakeLocationUpstream{timezone: "Mars/Olympus_Mons"}
	withLocationUpstream(t, up)
	deps := locationDeps(locationNow)
	_, terr := handleSetCurrentLocation(context.Background(), deps,
		Invocation{UserID: "user-1"}, map[string]any{"latitude": 39.74, "longitude": -104.99})
	require.NotNil(t, terr)
	assert.Equal(t, CodeUpstreamError, terr.Code)
	assert.Nil(t, deps.Profile(context.Background(), "user-1").CurrentLocation)
}

// The reported local time carries the zone abbreviation, so the two 1:30 AMs
// of a DST fall-back night (and the spring-forward jump) are unambiguous.
func TestSetCurrentLocationLocalTimeAcrossDST(t *testing.T) {
	ny := geoCandidate{Name: "New York", Admin1: "New York", Country: "United States",
		Latitude: 40.7128, Longitude: -74.0060, Timezone: "America/New_York"}
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{"fall-back first 1:30 (EDT)", time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC),
			"Sunday, November 1, 2026 at 1:30 AM EDT"},
		{"fall-back second 1:30 (EST)", time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC),
			"Sunday, November 1, 2026 at 1:30 AM EST"},
		{"just before spring-forward", time.Date(2026, 3, 8, 6, 59, 0, 0, time.UTC),
			"Sunday, March 8, 2026 at 1:59 AM EST"},
		{"just after spring-forward", time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC),
			"Sunday, March 8, 2026 at 3:00 AM EDT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withLocationUpstream(t, &fakeLocationUpstream{results: []geoCandidate{ny}})
			deps := locationDeps(tc.now)
			out, terr := handleSetCurrentLocation(context.Background(), deps,
				Invocation{UserID: "user-1"}, map[string]any{"place": "New York"})
			require.Nil(t, terr)
			assert.Equal(t, tc.want, out["localTime"])
		})
	}
}

func TestSetCurrentLocationIsRegisteredSideEffecting(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	def, ok := r.tools["set_current_location"]
	require.True(t, ok, "set_current_location must be in the catalog")
	assert.True(t, def.SideEffecting)

	names := make([]string, 0)
	for _, d := range definitions() {
		names = append(names, d.Name)
	}
	weatherAt := indexOf(names, "get_weather")
	require.GreaterOrEqual(t, weatherAt, 0)
	assert.Equal(t, "set_current_location", names[weatherAt+1],
		"set_current_location follows get_weather in the catalog")
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

func TestProfileClockNeverUTC(t *testing.T) {
	cur := store.Location{Label: "Denver", Lat: 39.7, Lon: -104.9, Timezone: "America/Denver"}
	home := charlotteNC
	stale := store.Location{Label: "x", Lat: 1, Lon: 2, Timezone: "Mars/Olympus_Mons"}
	cases := []struct {
		name string
		p    store.Profile
		want string
	}{
		{"empty profile", store.Profile{}, "America/New_York"},
		{"unknown zone", store.Profile{HomeLocation: &stale}, "America/New_York"},
		{"home", store.Profile{HomeLocation: &home}, "America/New_York"},
		{"current over home", store.Profile{CurrentLocation: &cur, HomeLocation: &home}, "America/Denver"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, profileClock(tc.p).String())
		})
	}
}
