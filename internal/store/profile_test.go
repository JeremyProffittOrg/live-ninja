package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func denverLoc() Location {
	return Location{
		Label: "Denver, Colorado, United States", City: "Denver", Admin1: "Colorado",
		Country: "United States", Lat: 39.7392, Lon: -104.9903, Timezone: "America/Denver",
	}
}

func huntersvilleLoc() *Location {
	return &Location{
		Label: "Huntersville, North Carolina, United States", City: "Huntersville",
		Admin1: "North Carolina", Country: "United States",
		Lat: 35.4107, Lon: -80.8428, Timezone: "America/New_York",
	}
}

// The fallback order is the contract every clock and reminder relies on:
// current → home → work → America/New_York. Never UTC.
func TestProfileTimezoneFallbackOrder(t *testing.T) {
	cur := denverLoc()
	home := huntersvilleLoc()
	work := &Location{Label: "Office", Lat: 41.88, Lon: -87.63, Timezone: "America/Chicago"}
	curNoTZ := denverLoc()
	curNoTZ.Timezone = ""

	cases := []struct {
		name        string
		p           Profile
		wantTZ      string
		wantDefault string
	}{
		{"nothing on file", Profile{}, "", DefaultTimezone},
		{"work only", Profile{WorkLocation: work}, "America/Chicago", "America/Chicago"},
		{"home beats work", Profile{HomeLocation: home, WorkLocation: work}, "America/New_York", "America/New_York"},
		{"current beats home and work",
			Profile{CurrentLocation: &cur, HomeLocation: home, WorkLocation: work}, "America/Denver", "America/Denver"},
		{"current without a zone falls through to home",
			Profile{CurrentLocation: &curNoTZ, HomeLocation: home}, "America/New_York", "America/New_York"},
		{"current alone", Profile{CurrentLocation: &cur}, "America/Denver", "America/Denver"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantTZ, tc.p.Timezone())
			assert.Equal(t, tc.wantDefault, tc.p.TimezoneOrDefault())
		})
	}
	assert.Equal(t, "America/New_York", DefaultTimezone)
}

func TestProfileEffectiveLocation(t *testing.T) {
	cur := denverLoc()
	home := huntersvilleLoc()
	unresolved := Location{Label: "", Lat: 0, Lon: 0}

	cases := []struct {
		name      string
		p         Profile
		wantLabel string
		wantKind  string
	}{
		{"neither", Profile{}, "", ""},
		{"work is not 'here'", Profile{WorkLocation: home}, "", ""},
		{"home", Profile{HomeLocation: home}, home.Label, LocationKindHome},
		{"current overrides home", Profile{CurrentLocation: &cur, HomeLocation: home}, cur.Label, LocationKindCurrent},
		{"unresolved current is ignored", Profile{CurrentLocation: &unresolved, HomeLocation: home},
			home.Label, LocationKindHome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loc, kind := tc.p.EffectiveLocation()
			assert.Equal(t, tc.wantLabel, loc.Label)
			assert.Equal(t, tc.wantKind, kind)
		})
	}
}

func TestProfileFromDocParsesCurrentLocation(t *testing.T) {
	doc := map[string]any{"profile": map[string]any{
		"currentLocation": map[string]any{
			"label": " Denver ", "city": "Denver", "lat": 39.7392, "lon": -104.9903,
			"timezone": "America/Denver", "setAt": "2026-10-01T14:00:00Z", "source": "gps",
		},
	}}
	p := ProfileFromDoc(doc)
	require.NotNil(t, p.CurrentLocation)
	assert.Equal(t, "Denver", p.CurrentLocation.Label)
	assert.Equal(t, "America/Denver", p.CurrentLocation.Timezone)
	assert.Equal(t, "2026-10-01T14:00:00Z", p.CurrentLocationSetAt)
	assert.Equal(t, CurrentLocationSourceGPS, p.CurrentLocationSource)
	assert.False(t, p.Empty(), "a profile with only a current location is not empty")

	// Null, malformed and coordinate-less values are "not set", and carry no
	// stray setAt/source.
	for name, v := range map[string]any{
		"null":           nil,
		"string":         "Denver",
		"no coordinates": map[string]any{"label": "Denver", "setAt": "2026-10-01T14:00:00Z", "source": "command"},
	} {
		t.Run(name, func(t *testing.T) {
			p := ProfileFromDoc(map[string]any{"profile": map[string]any{"currentLocation": v}})
			assert.Nil(t, p.CurrentLocation)
			assert.Empty(t, p.CurrentLocationSetAt)
			assert.Empty(t, p.CurrentLocationSource)
		})
	}
}

func TestSetAndClearCurrentLocationPreservesTheRestOfTheProfile(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	doc := DefaultSettings()
	profile := doc["profile"].(map[string]any)
	profile["displayName"] = "Jeremy"
	profile["homeLocation"] = map[string]any{
		"label": "Huntersville, North Carolina, United States", "city": "Huntersville",
		"lat": 35.4107, "lon": -80.8428, "timezone": "America/New_York",
	}
	profile["futureKey"] = "kept" // additive field from a newer client
	doc["voice"] = "ballad"
	v, err := st.PutSettings(ctx, "user-1", doc, 1)
	require.NoError(t, err)
	require.EqualValues(t, 2, v)

	setAt := time.Date(2026, 10, 1, 14, 0, 0, 0, time.FixedZone("MDT", -6*3600))
	v, err = st.SetCurrentLocation(ctx, "user-1", denverLoc(), CurrentLocationSourceCommand, setAt)
	require.NoError(t, err)
	assert.EqualValues(t, 3, v, "the write must advance the settings version so every surface syncs")

	p, err := st.GetProfile(ctx, "user-1")
	require.NoError(t, err)
	require.NotNil(t, p.CurrentLocation)
	assert.Equal(t, "America/Denver", p.Timezone())
	assert.Equal(t, "2026-10-01T20:00:00Z", p.CurrentLocationSetAt, "setAt is stored in UTC")
	assert.Equal(t, CurrentLocationSourceCommand, p.CurrentLocationSource)
	assert.Equal(t, "Jeremy", p.DisplayName)
	require.NotNil(t, p.HomeLocation, "home must survive the current-location write")

	got, err := st.GetSettings(ctx, "user-1")
	require.NoError(t, err)
	assert.Equal(t, "ballad", got["voice"])
	assert.Equal(t, "kept", got["profile"].(map[string]any)["futureKey"])

	cleared, v, err := st.ClearCurrentLocation(ctx, "user-1")
	require.NoError(t, err)
	assert.True(t, cleared)
	assert.EqualValues(t, 4, v)
	p, err = st.GetProfile(ctx, "user-1")
	require.NoError(t, err)
	assert.Nil(t, p.CurrentLocation)
	assert.Equal(t, "America/New_York", p.Timezone(), "home applies again")
	require.NotNil(t, p.HomeLocation)

	// A second clear is a no-op: nothing written, version unchanged.
	cleared, v, err = st.ClearCurrentLocation(ctx, "user-1")
	require.NoError(t, err)
	assert.False(t, cleared)
	assert.EqualValues(t, 4, v)
}

// A user who never saved settings still gets a current location: the write
// lands on the synthesized default document as its first version.
func TestSetCurrentLocationOnFreshAccount(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	v, err := st.SetCurrentLocation(ctx, "user-1", denverLoc(), CurrentLocationSourceGPS, time.Now())
	require.NoError(t, err)
	assert.EqualValues(t, 2, v)
	got, err := st.GetSettings(ctx, "user-1")
	require.NoError(t, err)
	assert.Equal(t, "cedar", got["voice"], "the defaults are written alongside")
	p := ProfileFromDoc(got)
	require.NotNil(t, p.CurrentLocation)
}

func TestSetCurrentLocationValidates(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	ok := denverLoc()

	noTZ := ok
	noTZ.Timezone = ""
	badTZ := ok
	badTZ.Timezone = "Mars/Olympus_Mons"
	noLabel := ok
	noLabel.Label = "  "
	badLat := ok
	badLat.Lat = 91
	badLon := ok
	badLon.Lon = -181
	nullIsland := ok
	nullIsland.Lat, nullIsland.Lon = 0, 0

	cases := []struct {
		name   string
		loc    Location
		source string
	}{
		{"unknown source", ok, "satellite"},
		{"empty source", ok, ""},
		{"no timezone", noTZ, CurrentLocationSourceGPS},
		{"unknown timezone", badTZ, CurrentLocationSourceGPS},
		{"no label", noLabel, CurrentLocationSourceGPS},
		{"latitude out of range", badLat, CurrentLocationSourceGPS},
		{"longitude out of range", badLon, CurrentLocationSourceGPS},
		{"0,0 is not a location", nullIsland, CurrentLocationSourceGPS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.SetCurrentLocation(ctx, "user-1", tc.loc, tc.source, time.Now())
			require.ErrorIs(t, err, ErrInvalidCurrentLocation)
		})
	}
	_, err := st.SetCurrentLocation(ctx, "", ok, CurrentLocationSourceGPS, time.Now())
	require.Error(t, err)
}

// The current location is account-wide. A device whose About You section
// is overridden — even with a stale echoed currentLocation — still sees the
// account's current location, and loses it when the account clears it.
func TestCurrentLocationIsAccountWideForDeviceProfiles(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	doc := DefaultSettings()
	require.NoError(t, ApplySettingsSection(doc, SettingsSectionAboutYou, map[string]any{
		"profile": map[string]any{
			"displayName": "Kitchen",
			"currentLocation": map[string]any{
				"label": "Stale Paris", "lat": 48.85, "lon": 2.35, "timezone": "Europe/Paris",
			},
		},
	}, []string{"device-1"}, false, false, time.Now()))
	_, err := st.PutSettings(ctx, "user-1", doc, 1)
	require.NoError(t, err)

	device, err := st.GetProfileForDevice(ctx, "user-1", "device-1")
	require.NoError(t, err)
	assert.Equal(t, "Kitchen", device.DisplayName, "the rest of the override still applies")
	assert.Nil(t, device.CurrentLocation, "a device override cannot pin a current location")

	_, err = st.SetCurrentLocation(ctx, "user-1", denverLoc(), CurrentLocationSourceCommand, time.Now())
	require.NoError(t, err)
	device, err = st.GetProfileForDevice(ctx, "user-1", "device-1")
	require.NoError(t, err)
	require.NotNil(t, device.CurrentLocation)
	assert.Equal(t, "America/Denver", device.Timezone())
}

// Inherit / apply-all clears a currentLocation echoed into a device override
// as a known field rather than keeping it as foreign additive data.
func TestInheritClearsEchoedCurrentLocation(t *testing.T) {
	doc := DefaultSettings()
	now := time.Now()
	require.NoError(t, ApplySettingsSection(doc, SettingsSectionAboutYou, map[string]any{
		"profile": map[string]any{"currentLocation": map[string]any{
			"label": "x", "lat": 1.0, "lon": 2.0, "timezone": "UTC", "setAt": "s", "source": "gps",
		}},
	}, []string{"device-1"}, false, false, now))
	require.NoError(t, ApplySettingsSection(doc, SettingsSectionAboutYou, nil,
		[]string{"device-1"}, false, true, now))
	overrides, _ := doc["deviceOverrides"].(map[string]any)
	assert.NotContains(t, overrides, "device-1")
}
