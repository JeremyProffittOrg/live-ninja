package webapp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// A settings document with no profile at all must keep validating — every
// client written before M15 sends exactly that.
func TestValidateProfileAbsentIsFine(t *testing.T) {
	assert.Equal(t, "", validateProfile(map[string]any{}))
	assert.Equal(t, "", validateProfile(map[string]any{"profile": nil}))
}

func TestValidateProfileAcceptsAFullProfile(t *testing.T) {
	doc := map[string]any{"profile": map[string]any{
		"displayName":  " Jeremy ",
		"pronouns":     "he/him",
		"units":        "metric",
		"contactEmail": "proffitt.jeremy@gmail.com",
		"homeLocation": map[string]any{
			"label": "Huntersville, North Carolina, United States",
			"city":  "Huntersville", "admin1": "North Carolina", "country": "United States",
			"postalCode": "28078", "lat": 35.4107, "lon": -80.8428,
			"timezone": "America/New_York",
		},
		"notes": []any{"Works in Eastern time", "  ", "Prefers short answers"},
	}}
	require.Equal(t, "", validateProfile(doc))

	p := doc["profile"].(map[string]any)
	assert.Equal(t, "Jeremy", p["displayName"], "names are trimmed")
	assert.Equal(t, []any{"Works in Eastern time", "Prefers short answers"},
		p["notes"], "blank notes are dropped rather than stored")
}

// The load-bearing rule: a location without coordinates is REJECTED. Accepting
// one would quietly recreate the free-text location field this design exists to
// remove, and every downstream consumer trusts lat/lon to be real.
func TestValidateProfileRejectsUnresolvedLocation(t *testing.T) {
	doc := map[string]any{"profile": map[string]any{
		"homeLocation": map[string]any{"label": "somewhere near the lake"},
	}}
	msg := validateProfile(doc)
	require.NotEqual(t, "", msg)
	assert.Contains(t, msg, "lat and lon")
}

func TestValidateProfileRejectsBadValues(t *testing.T) {
	cases := []struct {
		name    string
		profile map[string]any
		wants   string
	}{
		{"units", map[string]any{"units": "furlongs"}, "imperial or metric"},
		{"email", map[string]any{"contactEmail": "not-an-address"}, "email address"},
		{"latitude range", map[string]any{"homeLocation": map[string]any{
			"label": "Nowhere", "lat": 991.0, "lon": 0.0}}, "between -90 and 90"},
		{"timezone", map[string]any{"homeLocation": map[string]any{
			"label": "Nowhere", "lat": 1.0, "lon": 2.0, "timezone": "Mars/Olympus_Mons"}}, "IANA timezone"},
		{"quiet hours", map[string]any{"quietHours": map[string]any{
			"start": "10pm", "end": "07:00"}}, "24h HH:MM"},
		{"note length", map[string]any{"notes": []any{string(make([]byte, 201))}}, "at most 200"},
		{"note count", map[string]any{"notes": make([]any, 21)}, "20 entries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := validateProfile(map[string]any{"profile": tc.profile})
			require.NotEqual(t, "", msg, "expected a rejection")
			assert.Contains(t, msg, tc.wants)
		})
	}
}

// An empty object is how the client clears a location; it must normalize to
// null rather than storing a coordinate-less husk.
func TestValidateProfileEmptyLocationClears(t *testing.T) {
	doc := map[string]any{"profile": map[string]any{"homeLocation": map[string]any{}}}
	require.Equal(t, "", validateProfile(doc))
	assert.Nil(t, doc["profile"].(map[string]any)["homeLocation"])
}

// Round-trip: what the validator accepts is what the store projects and the
// broker renders. A drift between these two shapes would put an empty block
// into every session while the settings page looked correct.
func TestValidatedProfileProjectsBackOut(t *testing.T) {
	doc := map[string]any{"profile": map[string]any{
		"displayName": "Jeremy",
		"units":       "metric",
		"homeLocation": map[string]any{
			"label": "Huntersville, North Carolina, United States", "city": "Huntersville",
			"admin1": "North Carolina", "country": "United States",
			"lat": 35.4107, "lon": -80.8428, "timezone": "America/New_York",
		},
		"notes": []any{"Works in Eastern time"},
	}}
	require.Equal(t, "", validateProfile(doc))

	p := store.ProfileFromDoc(doc)
	assert.False(t, p.Empty())
	assert.Equal(t, "Jeremy", p.DisplayName)
	assert.Equal(t, store.UnitsMetric, p.UnitsOrDefault())
	assert.Equal(t, "America/New_York", p.Timezone())
	require.True(t, p.Home().Resolved())
	assert.InDelta(t, 35.4107, p.Home().Lat, 0.0001)
	assert.Equal(t, []string{"Works in Eastern time"}, p.Notes)
}

func TestMatchesHintAcceptsStateAbbreviations(t *testing.T) {
	assert.True(t, matchesHint("NC", "North Carolina", "United States", "US"))
	assert.True(t, matchesHint("North Carolina", "North Carolina", "United States", "US"))
	assert.True(t, matchesHint("France", "Île-de-France", "France", "FR"))
	assert.True(t, matchesHint("fr", "Île-de-France", "France", "FR"))
	assert.False(t, matchesHint("TX", "North Carolina", "United States", "US"))
}
