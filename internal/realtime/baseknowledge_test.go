package realtime

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

func testHome() *store.Location {
	return &store.Location{
		Label: "Huntersville, North Carolina, United States", City: "Huntersville",
		Admin1: "North Carolina", Country: "United States", PostalCode: "28078",
		Lat: 35.4107, Lon: -80.8428, Timezone: "America/New_York",
	}
}

// An empty profile carries no blanks — but it does carry the clock, in the
// default zone, so "what time is it" is never answered in UTC.
func TestBuildBaseKnowledgeEmptyProfileYieldsOnlyTheClock(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC) // 08:00 EDT
	got := BuildBaseKnowledge(store.Profile{}, now)
	assert.Equal(t, baseKnowledgeHeader+
		"- Right now it is Friday, July 24, 2026 at 8:00 AM (EDT, America/New_York (default)). "+
		"Use this for anything time- or date-relative.", got)
	assert.NotContains(t, got, "Preferred units", "no unconfirmed facts for an empty profile")
}

func TestBuildBaseKnowledgeRendersTheFacts(t *testing.T) {
	p := store.Profile{
		DisplayName:  "Jeremy",
		Pronouns:     "he/him",
		HomeLocation: testHome(),
		Units:        store.UnitsImperial,
		ContactEmail: "proffitt.jeremy@gmail.com",
		Notes:        []string{"Works in Eastern time", "Prefers short answers"},
	}
	// Noon UTC is 8am in America/New_York — a deliberate cross-boundary check.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	got := BuildBaseKnowledge(p, now)

	assert.Contains(t, got, "BASE KNOWLEDGE")
	assert.Contains(t, got, "Jeremy")
	assert.Contains(t, got, "he/him")
	assert.Contains(t, got, "Huntersville, North Carolina, United States")
	assert.Contains(t, got, "35.4107")
	assert.Contains(t, got, "proffitt.jeremy@gmail.com")
	assert.Contains(t, got, "Works in Eastern time")
	assert.Contains(t, got, "Prefers short answers")
	assert.Contains(t, got, "imperial")
	assert.True(t, strings.HasPrefix(got, "\n\n"), "the block must open its own paragraph")
}

// The clock is the single most valuable line in the block — it is the thing
// the model had no access to at all before M15. It must be rendered in the
// user's zone, not UTC.
func TestBuildBaseKnowledgeClockUsesProfileTimezone(t *testing.T) {
	p := store.Profile{DisplayName: "Jeremy", HomeLocation: testHome()}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC) // 08:00 EDT

	got := BuildBaseKnowledge(p, now)
	assert.Contains(t, got, "Friday, July 24, 2026 at 8:00 AM")
	assert.Contains(t, got, "America/New_York")
	assert.NotContains(t, got, "at 12:00 PM", "the clock must not be rendered in UTC")
}

// Lambda's provided.al2023 image ships no /usr/share/zoneinfo; without the
// embedded tzdata import in baseknowledge.go every zone would silently fall
// back to UTC in production while passing on a developer machine. This test
// is the guard for that import.
func TestTimezoneDatabaseIsAvailable(t *testing.T) {
	for _, tz := range []string{"America/New_York", "Europe/London", "Australia/Sydney", "Asia/Kolkata"} {
		loc, err := time.LoadLocation(tz)
		require.NoError(t, err, "tzdata must be embedded for %s", tz)
		require.NotNil(t, loc)
	}
}

// A stale or renamed zone must degrade, never panic or fail the mint. It
// degrades to the default zone, labelled as assumed — not to UTC.
func TestBuildBaseKnowledgeUnknownTimezoneFallsBackToDefault(t *testing.T) {
	home := testHome()
	home.Timezone = "Mars/Olympus_Mons"
	p := store.Profile{DisplayName: "Jeremy", HomeLocation: home}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	got := BuildBaseKnowledge(p, now)
	assert.Contains(t, got, "at 8:00 AM (EDT, America/New_York (default; the stored zone Mars/Olympus_Mons is unknown))")
	assert.NotContains(t, got, "at 12:00 PM", "an unknown zone must not render in UTC")
}

// A profile with no timezone anywhere still gets a clock — America/New_York,
// labelled honestly so the model knows the zone is assumed.
func TestBuildBaseKnowledgeNoTimezoneUsesDefault(t *testing.T) {
	p := store.Profile{DisplayName: "Jeremy"}
	got := BuildBaseKnowledge(p, time.Date(2026, 1, 24, 12, 0, 0, 0, time.UTC))
	assert.Contains(t, got, "at 7:00 AM (EST, America/New_York (default))")
}

func testDenver() *store.Location {
	return &store.Location{
		Label: "Denver, Colorado, United States", City: "Denver", Admin1: "Colorado",
		Country: "United States", Lat: 39.7392, Lon: -104.9903, Timezone: "America/Denver",
	}
}

// Contract D: the current location overrides home for the clock, and the
// block says so in a line the model can act on, while home stays listed.
func TestBuildBaseKnowledgeCurrentLocationOverridesHome(t *testing.T) {
	p := store.Profile{
		DisplayName:           "Jeremy",
		HomeLocation:          testHome(),
		CurrentLocation:       testDenver(),
		CurrentLocationSetAt:  "2026-10-01T03:30:00Z", // Sept 30 evening in Denver
		CurrentLocationSource: store.CurrentLocationSourceCommand,
	}
	now := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC) // 12:00 MDT

	got := BuildBaseKnowledge(p, now)
	assert.Contains(t, got, "Thursday, October 1, 2026 at 12:00 PM (MDT, America/Denver, current location)")
	assert.Contains(t, got, "- Current location (set September 30, 2026, by the user's command): "+
		"Denver, Colorado, United States (39.7392, -104.9903). Use this, not home, for time, weather, and 'here'.")
	assert.Contains(t, got, "Home / default location: Huntersville", "home stays listed")

	currentAt := strings.Index(got, "- Current location")
	homeAt := strings.Index(got, "- Home / default location")
	require.True(t, currentAt >= 0 && homeAt > currentAt, "current is listed before home")
	// The no-location weather hint belongs to the current location now; on
	// the home line it would contradict the override.
	assert.Equal(t, 1, strings.Count(got, "get_weather with no location"))
	assert.Contains(t, got[currentAt:homeAt], "get_weather with no location")
}

func TestBuildBaseKnowledgeCurrentLocationLineVariants(t *testing.T) {
	cases := []struct {
		name, setAt, source, want string
	}{
		{"gps", "2026-10-01T15:00:00Z", store.CurrentLocationSourceGPS,
			"(set October 1, 2026, by GPS coordinates)"},
		{"missing setAt", "", store.CurrentLocationSourceCommand,
			"(set at an unknown time, by the user's command)"},
		{"malformed setAt and unknown source", "yesterday", "carrier-pigeon",
			"(set at an unknown time, by the user)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := store.Profile{CurrentLocation: testDenver(),
				CurrentLocationSetAt: tc.setAt, CurrentLocationSource: tc.source}
			got := BuildBaseKnowledge(p, time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC))
			assert.Contains(t, got, tc.want)
			assert.Contains(t, got, "America/Denver, current location")
		})
	}
}

// The clock follows DST in whichever zone is in effect; the abbreviation is
// rendered so the two 1:30 AMs of a fall-back night are distinguishable.
func TestBuildBaseKnowledgeClockAcrossDST(t *testing.T) {
	cases := []struct {
		name string
		p    store.Profile
		now  time.Time
		want string
	}{
		{"default zone, before fall-back", store.Profile{}, time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC),
			"1:30 AM (EDT"},
		{"default zone, after fall-back", store.Profile{}, time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC),
			"1:30 AM (EST"},
		{"current zone, after spring-forward", store.Profile{CurrentLocation: testDenver()},
			time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC), "3:00 AM (MDT"},
		{"current zone, before spring-forward", store.Profile{CurrentLocation: testDenver()},
			time.Date(2026, 3, 8, 8, 59, 0, 0, time.UTC), "1:59 AM (MST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Contains(t, BuildBaseKnowledge(tc.p, tc.now), tc.want)
		})
	}
}

func TestBuildBaseKnowledgeMetricUnits(t *testing.T) {
	p := store.Profile{DisplayName: "Jeremy", Units: store.UnitsMetric}
	got := BuildBaseKnowledge(p, time.Now())
	assert.Contains(t, got, "metric")
	assert.Contains(t, got, "Celsius")
}

// With a home on file the model must be told it can call get_weather with no
// location — otherwise it keeps passing one out of habit and the whole
// geocode-free path never runs.
func TestBuildBaseKnowledgeTellsTheModelToOmitLocation(t *testing.T) {
	p := store.Profile{HomeLocation: testHome()}
	got := BuildBaseKnowledge(p, time.Now())
	assert.Contains(t, got, "get_weather with no location")
}

// The block composes after the platform directives and before guides, on
// every engine. This asserts the ordering contract the broker relies on.
func TestBaseKnowledgeComposesAfterSessionDirectives(t *testing.T) {
	p := store.Profile{DisplayName: "Jeremy", HomeLocation: testHome()}
	instructions := "PERSONA." + SessionDirectives + BuildBaseKnowledge(p, time.Now()) + "\n\nGUIDES."

	personaAt := strings.Index(instructions, "PERSONA.")
	memoryAt := strings.Index(instructions, "persistent long-term memory")
	baseAt := strings.Index(instructions, "BASE KNOWLEDGE")
	guidesAt := strings.Index(instructions, "GUIDES.")

	require.True(t, personaAt < memoryAt, "persona first")
	require.True(t, memoryAt < baseAt, "memory directive before base knowledge")
	require.True(t, baseAt < guidesAt, "base knowledge before guides")

	// The ordering assertions above locate the block by its header text, so
	// that header has to be unique in the composed instructions — M16's
	// memoryUsageDirective now talks ABOUT the profile, and if it ever spelled
	// the header verbatim the three Index() calls above would silently start
	// measuring the directive's mention instead of the block itself and pass no
	// matter where the real block ended up.
	require.Equal(t, 1, strings.Count(instructions, "BASE KNOWLEDGE"),
		"the BASE KNOWLEDGE header must appear exactly once — the ordering assertions above "+
			"locate the block by it, so a second mention (e.g. in a directive) makes them vacuous")
}
