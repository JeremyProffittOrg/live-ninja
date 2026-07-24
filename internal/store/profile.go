package store

// Base Knowledge profile (M15, base-knowledge-plan.md P1/P2). The profile is
// the *stable* half of what the assistant knows about its user: name, home,
// timezone, units, contact address. It lives inside the canonical settings
// document (contracts/settings.schema.json `profile`) so it rides the existing
// optimistic-concurrency version and cross-surface sync for free.
//
// It is deliberately NOT the memory layer. Memory (internal/memory) is
// episodic and retrieval-on-demand — the model must *think* to call
// memory_search, and prod proved it often doesn't. Profile facts are always
// relevant, so they are injected server-side into every session's instructions
// (internal/realtime.BuildBaseKnowledge) and used as default arguments for
// profile-aware tools. Neither store writes to the other: memory→profile
// promotion is an owner-confirmed action (M16), never a silent copy.
//
// The typed view below is read-only and lenient by construction: it is
// projected out of the untyped document, every field is optional, and a
// malformed or absent value yields the zero value rather than an error. A
// profile can never take a mint down.

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Units are the two unit systems the profile can select.
const (
	UnitsImperial = "imperial"
	UnitsMetric   = "metric"
)

// Location is one geocode-verified place (settings.schema.json
// $defs/profileLocation). Lat/Lon/Timezone are resolved at save time from a
// GET /api/v1/geocode selection, never at question time — that is the whole
// point: the model never has to name a place it might get wrong, and the
// weather tool never has to guess which "Paris" was meant.
type Location struct {
	Label      string  `dynamodbav:"label"`
	PostalCode string  `dynamodbav:"postalCode"`
	City       string  `dynamodbav:"city"`
	Admin1     string  `dynamodbav:"admin1"`
	Country    string  `dynamodbav:"country"`
	Lat        float64 `dynamodbav:"lat"`
	Lon        float64 `dynamodbav:"lon"`
	Timezone   string  `dynamodbav:"timezone"`
}

// Resolved reports whether the location carries usable coordinates. A
// zero-value Location (never set, or a malformed stored value) is not
// resolved, and every caller treats that as "no location known" rather than
// as the coordinates of Null Island.
func (l Location) Resolved() bool {
	return l.Label != "" && (l.Lat != 0 || l.Lon != 0)
}

// QuietHours is an optional local-time window (24h HH:MM) during which the
// assistant should avoid proactive contact.
type QuietHours struct {
	Start string `dynamodbav:"start"`
	End   string `dynamodbav:"end"`
}

// Set reports whether both ends of the window are populated.
func (q QuietHours) Set() bool { return q.Start != "" && q.End != "" }

// Profile is the typed read view of settings.profile.
type Profile struct {
	DisplayName  string      `dynamodbav:"displayName"`
	Pronouns     string      `dynamodbav:"pronouns"`
	HomeLocation *Location   `dynamodbav:"homeLocation"`
	WorkLocation *Location   `dynamodbav:"workLocation"`
	Units        string      `dynamodbav:"units"`
	Locale       string      `dynamodbav:"locale"`
	ContactEmail string      `dynamodbav:"contactEmail"`
	QuietHours   *QuietHours `dynamodbav:"quietHours"`
	Notes        []string    `dynamodbav:"notes"`
}

// Empty reports whether the profile carries nothing worth telling the model.
// An empty profile mints exactly as sessions did before M15 — no BASE
// KNOWLEDGE block at all, rather than a block full of blanks.
func (p Profile) Empty() bool {
	return p.DisplayName == "" &&
		p.Pronouns == "" &&
		!p.Home().Resolved() &&
		!p.Work().Resolved() &&
		p.Locale == "" &&
		p.ContactEmail == "" &&
		len(p.Notes) == 0
}

// Home returns the home location, or a zero Location when unset.
func (p Profile) Home() Location {
	if p.HomeLocation == nil {
		return Location{}
	}
	return *p.HomeLocation
}

// Work returns the work location, or a zero Location when unset.
func (p Profile) Work() Location {
	if p.WorkLocation == nil {
		return Location{}
	}
	return *p.WorkLocation
}

// UnitsOrDefault returns the profile's unit system, defaulting to imperial
// (the pre-M15 hardcoded behaviour) when unset or unrecognized.
func (p Profile) UnitsOrDefault() string {
	if p.Units == UnitsMetric {
		return UnitsMetric
	}
	return UnitsImperial
}

// Timezone returns the best-known IANA timezone id for the user: the home
// location's, falling back to work, falling back to "". Callers treat "" as
// UTC.
func (p Profile) Timezone() string {
	if tz := p.Home().Timezone; tz != "" {
		return tz
	}
	return p.Work().Timezone
}

// profileAttr is the settings-document attribute the profile lives under.
const profileAttr = "profile"

// SettingsGetter is the single-item read this package needs to project a
// profile out of the settings document. A *dynamodb.Client satisfies it;
// tests inject a fake. (Deliberately duplicated rather than shared with
// internal/realtime's identical interface: neither package should have to
// import the other to read one attribute.)
type SettingsGetter interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

// GetProfile projects just settings.profile for one user in a single
// GetItem — never a Scan, and never the whole document when only the profile
// is wanted. A missing document, a missing profile attribute, or a malformed
// stored value all yield the zero Profile with a nil error: the caller's job
// (mint, tool defaulting) must proceed regardless.
func (s *Store) GetProfile(ctx context.Context, userID string) (Profile, error) {
	if userID == "" {
		return Profile{}, fmt.Errorf("store: userID is required")
	}
	return LoadProfile(ctx, s.client, s.table, userID), nil
}

// LoadProfile is GetProfile's dependency-injected form for callers that hold
// a raw getter rather than a *Store (the realtime broker holds exactly that,
// mirroring ResolveSessionVoice's single-read posture). It never returns an
// error by design — every failure path degrades to the zero profile.
func LoadProfile(ctx context.Context, g SettingsGetter, table, userID string) Profile {
	if g == nil || table == "" || userID == "" {
		return Profile{}
	}
	out, err := g.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: "USER#" + userID},
			"sk": &types.AttributeValueMemberS{Value: settingsSK},
		},
		ProjectionExpression:     aws.String("#p"),
		ExpressionAttributeNames: map[string]string{"#p": profileAttr},
	})
	if err != nil || len(out.Item) == 0 {
		return Profile{}
	}
	var wrapper struct {
		Profile Profile `dynamodbav:"profile"`
	}
	// Best-effort: a malformed profile leaves the zero value, which every
	// consumer already handles as "nothing known about this user".
	_ = attributevalue.UnmarshalMap(out.Item, &wrapper)
	return wrapper.Profile.normalized()
}

// ProfileFromDoc projects a Profile out of an already-loaded settings
// document (the shape GetSettings returns). Used by the HTTP layer, which
// has the whole document in hand and must not issue a second read.
func ProfileFromDoc(doc map[string]any) Profile {
	raw, ok := doc[profileAttr].(map[string]any)
	if !ok {
		return Profile{}
	}
	p := Profile{
		DisplayName:  docStr(raw, "displayName"),
		Pronouns:     docStr(raw, "pronouns"),
		Units:        docStr(raw, "units"),
		Locale:       docStr(raw, "locale"),
		ContactEmail: docStr(raw, "contactEmail"),
	}
	if loc, ok := locationFromAny(raw["homeLocation"]); ok {
		p.HomeLocation = &loc
	}
	if loc, ok := locationFromAny(raw["workLocation"]); ok {
		p.WorkLocation = &loc
	}
	if qh, ok := raw["quietHours"].(map[string]any); ok {
		q := QuietHours{Start: docStr(qh, "start"), End: docStr(qh, "end")}
		if q.Set() {
			p.QuietHours = &q
		}
	}
	if notes, ok := raw["notes"].([]any); ok {
		for _, n := range notes {
			if s, ok := n.(string); ok && strings.TrimSpace(s) != "" {
				p.Notes = append(p.Notes, strings.TrimSpace(s))
			}
		}
	}
	return p.normalized()
}

// normalized trims whitespace and drops locations that never resolved, so
// consumers can trust Resolved() as the only presence check they need.
func (p Profile) normalized() Profile {
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	p.Pronouns = strings.TrimSpace(p.Pronouns)
	p.Locale = strings.TrimSpace(p.Locale)
	p.ContactEmail = strings.TrimSpace(p.ContactEmail)
	if p.HomeLocation != nil && !p.HomeLocation.Resolved() {
		p.HomeLocation = nil
	}
	if p.WorkLocation != nil && !p.WorkLocation.Resolved() {
		p.WorkLocation = nil
	}
	if p.QuietHours != nil && !p.QuietHours.Set() {
		p.QuietHours = nil
	}
	return p
}

// USStateName maps a two-letter US state/territory abbreviation to its full
// lowercase name. It lives here because two independent consumers need the
// same table: the weather tool's candidate ranking (internal/tools/geocode.go)
// and the settings location picker's hint filter (internal/webapp) — both
// exist to make the "City, ST" shape a US-centric model emits resolve to the
// right place. Returns "" for anything unrecognized.
func USStateName(abbr string) string {
	return usStates[strings.ToUpper(strings.TrimSpace(abbr))]
}

var usStates = map[string]string{
	"AL": "alabama", "AK": "alaska", "AZ": "arizona", "AR": "arkansas",
	"CA": "california", "CO": "colorado", "CT": "connecticut", "DE": "delaware",
	"DC": "district of columbia", "FL": "florida", "GA": "georgia", "HI": "hawaii",
	"ID": "idaho", "IL": "illinois", "IN": "indiana", "IA": "iowa",
	"KS": "kansas", "KY": "kentucky", "LA": "louisiana", "ME": "maine",
	"MD": "maryland", "MA": "massachusetts", "MI": "michigan", "MN": "minnesota",
	"MS": "mississippi", "MO": "missouri", "MT": "montana", "NE": "nebraska",
	"NV": "nevada", "NH": "new hampshire", "NJ": "new jersey", "NM": "new mexico",
	"NY": "new york", "NC": "north carolina", "ND": "north dakota", "OH": "ohio",
	"OK": "oklahoma", "OR": "oregon", "PA": "pennsylvania", "RI": "rhode island",
	"SC": "south carolina", "SD": "south dakota", "TN": "tennessee", "TX": "texas",
	"UT": "utah", "VT": "vermont", "VA": "virginia", "WA": "washington",
	"WV": "west virginia", "WI": "wisconsin", "WY": "wyoming",
	"PR": "puerto rico", "VI": "united states virgin islands", "GU": "guam",
}

func docStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// locationFromAny projects one untyped location object. It reports false for
// null, a non-object, or an object without usable coordinates.
func locationFromAny(v any) (Location, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return Location{}, false
	}
	loc := Location{
		Label:      docStr(m, "label"),
		PostalCode: docStr(m, "postalCode"),
		City:       docStr(m, "city"),
		Admin1:     docStr(m, "admin1"),
		Country:    docStr(m, "country"),
		Timezone:   docStr(m, "timezone"),
	}
	loc.Lat, _ = numFromAny(m["lat"])
	loc.Lon, _ = numFromAny(m["lon"])
	if !loc.Resolved() {
		return Location{}, false
	}
	return loc, true
}

// numFromAny coerces the numeric shapes a settings document can carry
// (float64 from JSON, json.Number from the API layer, int from Go callers).
func numFromAny(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case interface{ Float64() (float64, error) }: // json.Number
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
