package tools

// M15 regression tests for the exact geocoding shapes that failed in prod
// (base-knowledge-plan.md P3): the owner's observation was "city fails, zip
// works", which is the "City, ST" compound returning zero results, plus the
// count=1 global-first-match problem ("Paris" → France from a US home).

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// charlotteNC is the owner's real home shape: coordinates near Huntersville,
// North Carolina. Used as the proximity anchor throughout.
var charlotteNC = store.Location{
	Label: "Huntersville, North Carolina, United States", City: "Huntersville",
	Admin1: "North Carolina", Country: "United States", PostalCode: "28078",
	Lat: 35.4107, Lon: -80.8428, Timezone: "America/New_York",
}

func TestSplitPlaceQuery(t *testing.T) {
	cases := []struct {
		in, name, hint string
	}{
		// The shape that returned ZERO results before M15: the name index has
		// no "Huntersville, NC" entry, only "Huntersville".
		{"Huntersville, NC", "Huntersville", "NC"},
		{"Paris, TX", "Paris", "TX"},
		{"Paris, France", "Paris", "France"},
		{"Charlotte", "Charlotte", ""},
		{"28078", "28078", ""},
		{"  London ,  UK ", "London", "UK"},
		{"Paris, Ile-de-France, France", "Paris", "Ile-de-France, France"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			name, hint := splitPlaceQuery(tc.in)
			assert.Equal(t, tc.name, name, "search name")
			assert.Equal(t, tc.hint, hint, "region hint")
		})
	}
}

func TestRankCandidatesPicksTheMeantPlace(t *testing.T) {
	parisFrance := geoCandidate{Name: "Paris", Admin1: "Île-de-France", Country: "France",
		CountryCode: "FR", Latitude: 48.8566, Longitude: 2.3522}
	parisTexas := geoCandidate{Name: "Paris", Admin1: "Texas", Country: "United States",
		CountryCode: "US", Latitude: 33.6609, Longitude: -95.5555}
	charlotteNCCand := geoCandidate{Name: "Charlotte", Admin1: "North Carolina",
		Country: "United States", CountryCode: "US", Latitude: 35.2271, Longitude: -80.8431}
	charlotteMI := geoCandidate{Name: "Charlotte", Admin1: "Michigan",
		Country: "United States", CountryCode: "US", Latitude: 42.5637, Longitude: -84.8358}

	cases := []struct {
		name  string
		cands []geoCandidate
		hint  string
		home  store.Location
		want  string // expected Admin1
	}{
		{
			// "Paris, TX" must NOT resolve to France — the whole point of the
			// hint. Note the France candidate is first (population order).
			name:  "explicit US state hint beats the more populous global match",
			cands: []geoCandidate{parisFrance, parisTexas},
			hint:  "TX",
			home:  charlotteNC,
			want:  "Texas",
		},
		{
			// The mirror case: an explicit country hint must survive a home
			// on the other side of the world. Proximity is a tiebreak, never
			// an override.
			name:  "explicit country hint wins over home proximity",
			cands: []geoCandidate{parisTexas, parisFrance},
			hint:  "France",
			home:  charlotteNC,
			want:  "Île-de-France",
		},
		{
			name:  "no hint: the nearer same-name place wins for this user",
			cands: []geoCandidate{charlotteMI, charlotteNCCand},
			hint:  "",
			home:  charlotteNC,
			want:  "North Carolina",
		},
		{
			// With nothing known about the user, behaviour must match the old
			// count=1 path: take the geocoder's own (population) ordering.
			name:  "no hint and no profile: first (most populous) result",
			cands: []geoCandidate{parisFrance, parisTexas},
			hint:  "",
			home:  store.Location{},
			want:  "Île-de-France",
		},
		{
			name:  "full state name hint also matches",
			cands: []geoCandidate{charlotteMI, charlotteNCCand},
			hint:  "North Carolina",
			home:  store.Location{},
			want:  "North Carolina",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rankCandidates(tc.cands, tc.hint, tc.home)
			assert.Equal(t, tc.want, got.Admin1)
		})
	}
}

func TestUSStateAbbreviationsResolve(t *testing.T) {
	assert.Equal(t, "north carolina", usStateName("NC"))
	assert.Equal(t, "north carolina", usStateName("nc"))
	assert.Equal(t, "texas", usStateName(" TX "))
	assert.Equal(t, "", usStateName("XX"))
	assert.Equal(t, "", usStateName("France"))
}

// fakeGeoServer stands in for Open-Meteo: it records the query it was asked
// and returns the canned candidates, so a test can assert BOTH the ranking
// and that the compound was split before the call.
type fakeGeoServer struct {
	lastName  string
	lastCount string
	results   []geoCandidate
	forecast  bool
}

func (f *fakeGeoServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/forecast" {
			f.forecast = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"current": map[string]any{"time": "2026-07-24T12:00", "temperature_2m": 88.1,
					"apparent_temperature": 94.0, "relative_humidity_2m": 61.0,
					"weather_code": 2, "wind_speed_10m": 7.0},
				"daily": map[string]any{"time": []string{"2026-07-24"}, "weather_code": []int{2},
					"temperature_2m_max": []float64{91}, "temperature_2m_min": []float64{72},
					"precipitation_probability_max": []float64{20}},
			})
			return
		}
		f.lastName = r.URL.Query().Get("name")
		f.lastCount = r.URL.Query().Get("count")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": f.results})
	}
}

// withFakeUpstreams points the package's geocode/forecast URLs at a test
// server for the duration of one test.
func withFakeUpstreams(t *testing.T, f *fakeGeoServer) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	oldGeo, oldForecast := geocodeURL, forecastURL
	geocodeURL, forecastURL = srv.URL+"/search", srv.URL+"/forecast"
	t.Cleanup(func() { geocodeURL, forecastURL = oldGeo, oldForecast })
}

func weatherDeps(profile store.Profile) *Deps {
	return &Deps{
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
		Now:         time.Now,
		Profile:     func(context.Context, string) store.Profile { return profile },
		Reauthorize: func(context.Context, string) error { return nil },
	}
}

// The headline M15 behaviour: no location argument at all, and the answer is
// still about the right place — with NO geocoding request made.
func TestGetWeatherWithNoLocationUsesProfileHome(t *testing.T) {
	fake := &fakeGeoServer{}
	withFakeUpstreams(t, fake)

	deps := weatherDeps(store.Profile{HomeLocation: &charlotteNC, Units: store.UnitsImperial})
	out, terr := handleGetWeather(context.Background(), deps,
		Invocation{UserID: "user-1"}, map[string]any{})

	require.Nil(t, terr)
	assert.Empty(t, fake.lastName, "no location argument must mean NO geocoding call at all")
	assert.True(t, fake.forecast, "the forecast leg must still run")

	loc := out["location"].(map[string]any)
	assert.Equal(t, charlotteNC.Label, loc["name"])
	assert.Equal(t, "profile-home", loc["source"])
	assert.Equal(t, charlotteNC.Lat, loc["latitude"])
	assert.Equal(t, "imperial", out["units"])
}

func TestGetWeatherUnitsDefaultToProfile(t *testing.T) {
	fake := &fakeGeoServer{}
	withFakeUpstreams(t, fake)

	deps := weatherDeps(store.Profile{HomeLocation: &charlotteNC, Units: store.UnitsMetric})
	out, terr := handleGetWeather(context.Background(), deps,
		Invocation{UserID: "user-1"}, map[string]any{})
	require.Nil(t, terr)
	assert.Equal(t, "metric", out["units"], "profile units are the default")

	// An explicit argument ("...in fahrenheit") still wins over the profile.
	out, terr = handleGetWeather(context.Background(), deps,
		Invocation{UserID: "user-1"}, map[string]any{"units": "imperial"})
	require.Nil(t, terr)
	assert.Equal(t, "imperial", out["units"], "an explicit units argument overrides the profile")
}

// "Huntersville, NC" — zero results before M15 because the compound was sent
// verbatim. Now the name alone is searched and the state ranks the result.
func TestGetWeatherResolvesCityStateCompound(t *testing.T) {
	fake := &fakeGeoServer{results: []geoCandidate{
		{Name: "Huntersville", Admin1: "North Carolina", Country: "United States",
			CountryCode: "US", Latitude: 35.4107, Longitude: -80.8428, Timezone: "America/New_York"},
	}}
	withFakeUpstreams(t, fake)

	deps := weatherDeps(store.Profile{HomeLocation: &charlotteNC})
	out, terr := handleGetWeather(context.Background(), deps,
		Invocation{UserID: "user-1"}, map[string]any{"location": "Huntersville, NC"})

	require.Nil(t, terr)
	assert.Equal(t, "Huntersville", fake.lastName,
		"the compound must be split — the geocoder has no 'Huntersville, NC' entry")
	assert.Equal(t, "5", fake.lastCount, "several candidates are needed to rank")
	loc := out["location"].(map[string]any)
	assert.Equal(t, "Huntersville, North Carolina, United States", loc["name"])
	assert.Equal(t, "geocoded", loc["source"])
}

// A bare postal code worked before and must keep working — it carries no
// comma, so it reaches the index untouched.
func TestGetWeatherPostalCodePassesThrough(t *testing.T) {
	fake := &fakeGeoServer{results: []geoCandidate{
		{Name: "Huntersville", Admin1: "North Carolina", Country: "United States",
			CountryCode: "US", Latitude: 35.4107, Longitude: -80.8428},
	}}
	withFakeUpstreams(t, fake)

	deps := weatherDeps(store.Profile{})
	_, terr := handleGetWeather(context.Background(), deps,
		Invocation{UserID: "user-1"}, map[string]any{"location": "28078"})
	require.Nil(t, terr)
	assert.Equal(t, "28078", fake.lastName)
}

// With neither an argument nor a profile home there is nothing to answer
// about: the tool must say so in a way the model can act on, not guess.
func TestGetWeatherWithoutLocationOrProfileAsks(t *testing.T) {
	fake := &fakeGeoServer{}
	withFakeUpstreams(t, fake)

	deps := weatherDeps(store.Profile{})
	_, terr := handleGetWeather(context.Background(), deps,
		Invocation{UserID: "user-1"}, map[string]any{})

	require.NotNil(t, terr)
	assert.Equal(t, CodeInvalidArgs, terr.Code)
	assert.Contains(t, terr.Message, "home location")
}

// The manifest must advertise location as optional, or the model will keep
// supplying one out of obligation and the geocode-free path never runs.
func TestGetWeatherLocationIsOptionalInTheManifest(t *testing.T) {
	def := getWeatherDefinition()
	for _, p := range def.Params {
		if p.Name == "location" {
			assert.False(t, p.Required, "location must be optional (M15)")
			return
		}
	}
	t.Fatal("get_weather has no location param")
}
