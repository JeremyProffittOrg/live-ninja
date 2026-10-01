package realtime

// Base Knowledge directive (M15, base-knowledge-plan.md P1/P2).
//
// Before this, session instructions were persona text + the memory/silence
// directives and nothing else: the model knew nothing about *who* it was
// talking to, *where* they were, or even what time it was. That produced a
// specific, daily class of failure — "what's the weather" forced a location
// question every single time, "what's today" was unanswerable, and personal
// facts depended on the model remembering to call memory_search.
//
// BuildBaseKnowledge renders the stable half of that context into a compact
// block appended to every mint, on every engine and on the fallback path.
// Three properties are load-bearing:
//
//   - Server-composed. The block is built here from the stored profile, never
//     from anything a client sent. Same anti-injection posture as persona
//     resolution: clients send IDs, the server owns instruction text.
//   - Always current. The local date/time is computed at mint from the
//     effective IANA timezone — the current location's, then home's, then
//     work's, then America/New_York — so every session starts with a correct
//     clock rather than the model's training-time guess, and never in UTC.
//   - Degradable. An empty profile yields only the clock line. Nothing here
//     can fail a mint.

import (
	"fmt"
	"strings"
	"time"

	// Embeds the IANA timezone database in the binary. Lambda's
	// provided.al2023 runtime image carries no /usr/share/zoneinfo, so
	// time.LoadLocation would fail for every zone and the clock line — the
	// single most valuable line in this block — would silently fall back to
	// UTC. This import is the fix; do not remove it.
	_ "time/tzdata"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// baseKnowledgeHeader introduces the block. The wording tells the model both
// what the facts are and how much authority they carry: they are current and
// user-confirmed, so it should use them rather than asking, but they are also
// not a licence to invent detail beyond them.
const baseKnowledgeHeader = "\n\nBASE KNOWLEDGE — current, user-confirmed facts about the person " +
	"you are talking to. Treat these as true and use them directly instead of asking. If a request " +
	"depends on a fact that is not listed here, ask or search memory rather than guessing.\n"

// BuildBaseKnowledge renders the profile into the instruction block appended
// at mint. now is the mint-time clock (injected so tests are deterministic);
// it is rendered in the profile's effective timezone.
//
// An empty profile still gets the clock line (in store.DefaultTimezone): a
// session that does not know the time answers "what time is it" in UTC or
// from its training data, and both are wrong.
func BuildBaseKnowledge(p store.Profile, now time.Time) string {
	zone, tzLabel := effectiveClock(p)
	clock := clockLine(now.In(zone), tzLabel)
	if p.Empty() {
		return baseKnowledgeHeader + clock
	}

	var lines []string

	if name := p.DisplayName; name != "" {
		line := "- You are speaking with " + name + "."
		if p.Pronouns != "" {
			line = strings.TrimSuffix(line, ".") + " (pronouns: " + p.Pronouns + ")."
		}
		lines = append(lines, line)
	} else if p.Pronouns != "" {
		lines = append(lines, "- The user's pronouns are "+p.Pronouns+".")
	}

	lines = append(lines, clock)

	cur := p.Current()
	if cur.Resolved() {
		lines = append(lines, fmt.Sprintf("- Current location (set %s, by %s): %s (%.4f, %.4f). "+
			"Use this, not home, for time, weather, and 'here'.%s",
			setAtDate(p.CurrentLocationSetAt, zone), sourceLabel(p.CurrentLocationSource),
			strings.Join(strings.Fields(cur.Label), " "), cur.Lat, cur.Lon, locationToolHint()))
	}
	if home := p.Home(); home.Resolved() {
		hint := locationToolHint()
		if cur.Resolved() {
			// The weather default is the current location while one is set;
			// telling the model home is the default would contradict it.
			hint = ""
		}
		lines = append(lines, fmt.Sprintf("- Home / default location: %s (latitude %.4f, longitude %.4f).%s",
			home.Label, home.Lat, home.Lon, hint))
	}
	if work := p.Work(); work.Resolved() {
		lines = append(lines, "- Work location: "+work.Label+".")
	}

	unitsPhrase := "Fahrenheit, miles, and other US customary units"
	if p.UnitsOrDefault() == store.UnitsMetric {
		unitsPhrase = "Celsius, kilometres, and other metric units"
	}
	lines = append(lines, "- Preferred units: "+p.UnitsOrDefault()+" — report measurements in "+unitsPhrase+".")

	if p.Locale != "" {
		lines = append(lines, "- Locale: "+p.Locale+".")
	}
	if p.ContactEmail != "" {
		lines = append(lines, "- The user's own email address is "+p.ContactEmail+
			" (use it when they ask you to send something to them; sending anywhere else still requires their confirmation).")
	}
	if q := p.QuietHours; q != nil && q.Set() {
		lines = append(lines, fmt.Sprintf("- Quiet hours are %s to %s local time; avoid proactive contact then.", q.Start, q.End))
	}
	for _, note := range p.Notes {
		lines = append(lines, "- "+strings.TrimSuffix(strings.TrimSpace(note), ".")+".")
	}

	return baseKnowledgeHeader + strings.Join(lines, "\n")
}

// effectiveClock picks the zone the clock is rendered in and the label that
// names it. Zone order: current → home → work → store.DefaultTimezone. An
// unknown stored zone id degrades to the default, labelled so the model knows
// the zone is assumed rather than confirmed.
func effectiveClock(p store.Profile) (*time.Location, string) {
	tz := p.Timezone()
	if tz == "" {
		return locationForTZ(store.DefaultTimezone), store.DefaultTimezone + " (default)"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return locationForTZ(store.DefaultTimezone),
			store.DefaultTimezone + " (default; the stored zone " + tz + " is unknown)"
	}
	if cur := p.Current(); cur.Resolved() && cur.Timezone == tz {
		return loc, tz + ", current location"
	}
	return loc, tz
}

// clockLine renders the clock. This is the line that fixes "what time is it",
// "what's today", "is it late", and every relative-date calculation the model
// would otherwise botch.
func clockLine(local time.Time, tzLabel string) string {
	return fmt.Sprintf("- Right now it is %s (%s, %s). Use this for anything time- or date-relative.",
		local.Format("Monday, January 2, 2006 at 3:04 PM"), local.Format("MST"), tzLabel)
}

// setAtDate renders profile.currentLocation.setAt as a calendar date in the
// clock's zone, or "at an unknown time" when it is missing or malformed (an
// older or hand-edited record must not break the line).
func setAtDate(setAt string, zone *time.Location) string {
	t, err := time.Parse(time.RFC3339, setAt)
	if err != nil {
		return "at an unknown time"
	}
	return t.In(zone).Format("January 2, 2006")
}

// sourceLabel names how the current location was set, in words.
func sourceLabel(source string) string {
	switch source {
	case store.CurrentLocationSourceGPS:
		return "GPS coordinates"
	case store.CurrentLocationSourceCommand:
		return "the user's command"
	}
	return "the user"
}

// locationToolHint tells the model it does not need to supply a location to
// the weather tool. Without this it keeps passing one out of habit — which is
// exactly the geocoding path M15 exists to avoid.
func locationToolHint() string {
	return " Tools that take a location default to this one, so call get_weather with no location " +
		"argument unless the user names a different place."
}

// locationForTZ resolves an IANA id to a *time.Location, degrading to UTC
// only if even that id cannot load (a stale/renamed zone must not panic a
// mint; with time/tzdata embedded the default zone always loads).
func locationForTZ(tz string) *time.Location {
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "" {
		return time.UTC
	}
	return loc
}
