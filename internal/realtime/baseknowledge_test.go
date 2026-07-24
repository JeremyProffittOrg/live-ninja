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

// An empty profile must cost nothing: sessions mint byte-identically to their
// pre-M15 shape rather than carrying a block full of blanks.
func TestBuildBaseKnowledgeEmptyProfileYieldsNothing(t *testing.T) {
	assert.Equal(t, "", BuildBaseKnowledge(store.Profile{}, time.Now()))
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

// A stale or renamed zone must degrade, never panic or fail the mint.
func TestBuildBaseKnowledgeUnknownTimezoneFallsBackToUTC(t *testing.T) {
	home := testHome()
	home.Timezone = "Mars/Olympus_Mons"
	p := store.Profile{DisplayName: "Jeremy", HomeLocation: home}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	got := BuildBaseKnowledge(p, now)
	assert.Contains(t, got, "at 12:00 PM", "an unknown zone renders in UTC")
	assert.NotEmpty(t, got)
}

// A profile with no timezone anywhere still gets a clock — UTC, labelled
// honestly so the model knows the zone is unconfirmed.
func TestBuildBaseKnowledgeNoTimezoneSaysSo(t *testing.T) {
	p := store.Profile{DisplayName: "Jeremy"}
	got := BuildBaseKnowledge(p, time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC))
	assert.Contains(t, got, "no timezone on file")
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
}
