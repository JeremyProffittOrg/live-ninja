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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DefaultTimezone is the IANA zone assumed when neither a current nor a home
// location carries one (owner decision 2026-10-01: US Eastern, EST/EDT). It
// replaces the old "no timezone means UTC" behaviour, which put every clock
// and naive reminder time four or five hours off for a user who had not yet
// picked a home.
const DefaultTimezone = "America/New_York"

// Current-location sources: how profile.currentLocation was set.
const (
	CurrentLocationSourceCommand = "command" // a spoken place name, geocoded
	CurrentLocationSourceGPS     = "gps"     // coordinates the user gave
)

// Effective-location kinds reported by Profile.EffectiveLocation.
const (
	LocationKindCurrent = "current"
	LocationKindHome    = "home"
)

// currentLocationKey is the profile key the current location lives under.
const currentLocationKey = "currentLocation"

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
	DisplayName  string    `dynamodbav:"displayName"`
	Pronouns     string    `dynamodbav:"pronouns"`
	HomeLocation *Location `dynamodbav:"homeLocation"`
	WorkLocation *Location `dynamodbav:"workLocation"`
	// CurrentLocation is where the user says they are right now ("I'm in
	// Denver", or GPS coordinates). It overrides home for time, weather and
	// "here" until cleared ("I'm back home"). It is account-wide, never
	// per-device: LoadProfileForDevice always takes it from the account
	// profile, so a device's About You override can neither hide nor pin one.
	CurrentLocation *Location `dynamodbav:"currentLocation"`
	// CurrentLocationSetAt (RFC3339 UTC) and CurrentLocationSource
	// ("command"|"gps") describe CurrentLocation; both are "" when it is
	// unset. They are stored inside the currentLocation object.
	CurrentLocationSetAt  string      `dynamodbav:"-"`
	CurrentLocationSource string      `dynamodbav:"-"`
	Units                 string      `dynamodbav:"units"`
	Locale                string      `dynamodbav:"locale"`
	ContactEmail          string      `dynamodbav:"contactEmail"`
	QuietHours            *QuietHours `dynamodbav:"quietHours"`
	Notes                 []string    `dynamodbav:"notes"`
}

// Empty reports whether the profile carries nothing worth telling the model.
// An empty profile mints exactly as sessions did before M15 — no BASE
// KNOWLEDGE block at all, rather than a block full of blanks.
func (p Profile) Empty() bool {
	return p.DisplayName == "" &&
		p.Pronouns == "" &&
		!p.Home().Resolved() &&
		!p.Work().Resolved() &&
		!p.Current().Resolved() &&
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

// Current returns the current location, or a zero Location when unset.
func (p Profile) Current() Location {
	if p.CurrentLocation == nil {
		return Location{}
	}
	return *p.CurrentLocation
}

// EffectiveLocation is the location "here" means: the current location when
// one is set, else home. kind is LocationKindCurrent, LocationKindHome, or ""
// when neither is resolved (the returned Location is then zero). Work is
// deliberately not a fallback: "the weather here" from someone who has only a
// work address on file is a question to ask, not a guess to make.
func (p Profile) EffectiveLocation() (Location, string) {
	if cur := p.Current(); cur.Resolved() {
		return cur, LocationKindCurrent
	}
	if home := p.Home(); home.Resolved() {
		return home, LocationKindHome
	}
	return Location{}, ""
}

// UnitsOrDefault returns the profile's unit system, defaulting to imperial
// (the pre-M15 hardcoded behaviour) when unset or unrecognized.
func (p Profile) UnitsOrDefault() string {
	if p.Units == UnitsMetric {
		return UnitsMetric
	}
	return UnitsImperial
}

// Timezone returns the best-known IANA timezone id for the user: the current
// location's, then home's, then work's, then "". Anything that renders a clock
// or interprets a local time must use TimezoneOrDefault instead; "" survives
// here only so a caller can tell "nothing on file" from a real zone (the RCA
// profile render reports exactly that).
func (p Profile) Timezone() string {
	if tz := p.Current().Timezone; tz != "" {
		return tz
	}
	if tz := p.Home().Timezone; tz != "" {
		return tz
	}
	return p.Work().Timezone
}

// TimezoneOrDefault is Timezone with DefaultTimezone in place of "".
func (p Profile) TimezoneOrDefault() string {
	if tz := p.Timezone(); tz != "" {
		return tz
	}
	return DefaultTimezone
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

// GetProfileForDevice is GetProfile over the named host's effective About
// You section.
func (s *Store) GetProfileForDevice(ctx context.Context, userID, deviceID string) (Profile, error) {
	if userID == "" {
		return Profile{}, fmt.Errorf("store: userID is required")
	}
	return LoadProfileForDevice(ctx, s.client, s.table, userID, deviceID), nil
}

// LoadProfile is GetProfile's dependency-injected form for callers that hold
// a raw getter rather than a *Store (the realtime broker holds exactly that,
// mirroring ResolveSessionVoice's single-read posture). It never returns an
// error by design — every failure path degrades to the zero profile.
func LoadProfile(ctx context.Context, g SettingsGetter, table, userID string) Profile {
	return LoadProfileForDevice(ctx, g, table, userID, "")
}

// LoadProfileForDevice projects profile from the named host's effective
// About You section. It is still a single projected GetItem and retains
// LoadProfile's fail-open behavior.
func LoadProfileForDevice(ctx context.Context, g SettingsGetter, table, userID, deviceID string) Profile {
	if g == nil || table == "" || userID == "" {
		return Profile{}
	}
	out, err := g.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: "USER#" + userID},
			"sk": &types.AttributeValueMemberS{Value: settingsSK},
		},
		ProjectionExpression: aws.String("#p, #o"),
		ExpressionAttributeNames: map[string]string{
			"#p": profileAttr,
			"#o": "deviceOverrides",
		},
	})
	if err != nil || len(out.Item) == 0 {
		return Profile{}
	}
	var raw map[string]any
	if attributevalue.UnmarshalMap(out.Item, &raw) != nil {
		return Profile{}
	}
	p := ProfileFromDoc(EffectiveSettings(raw, deviceID))
	if deviceID != "" {
		// The current location is account-wide: where the user is does not
		// depend on which device asks. Take it from the account profile so a
		// device's About You override can neither hide nor pin one.
		withCurrentFrom(&p, ProfileFromDoc(raw))
	}
	return p
}

// withCurrentFrom copies the current-location fields of src onto p.
func withCurrentFrom(p *Profile, src Profile) {
	p.CurrentLocation = src.CurrentLocation
	p.CurrentLocationSetAt = src.CurrentLocationSetAt
	p.CurrentLocationSource = src.CurrentLocationSource
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
	if loc, ok := locationFromAny(raw[currentLocationKey]); ok {
		p.CurrentLocation = &loc
		cur, _ := raw[currentLocationKey].(map[string]any)
		p.CurrentLocationSetAt = docStr(cur, "setAt")
		p.CurrentLocationSource = docStr(cur, "source")
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
	if p.CurrentLocation != nil && !p.CurrentLocation.Resolved() {
		p.CurrentLocation = nil
	}
	if p.CurrentLocation == nil {
		p.CurrentLocationSetAt, p.CurrentLocationSource = "", ""
	}
	if p.QuietHours != nil && !p.QuietHours.Set() {
		p.QuietHours = nil
	}
	return p
}

// ---- current location: the versioned set / clear write path ----

// ErrInvalidCurrentLocation is returned by SetCurrentLocation for a location
// that is not resolved, has out-of-range coordinates, carries no loadable
// timezone, or names an unknown source.
var ErrInvalidCurrentLocation = errors.New("store: invalid current location")

// SetCurrentLocation stores loc as profile.currentLocation (account-wide),
// stamped with setAt and source. It rides the same optimistic-concurrency path
// as every other settings write (GetSettings → mutate → PutSettings with the
// version read, retried on a lost race), so the settings version advances and
// every surface's sync sees the change. Only profile.currentLocation is
// replaced; every other profile key and every unknown field survive. Returns
// the committed settings version.
func (s *Store) SetCurrentLocation(ctx context.Context, userID string, loc Location, source string, setAt time.Time) (int64, error) {
	if userID == "" {
		return 0, errors.New("store: userID is required")
	}
	// Every text field is collapsed to one line: the label comes from the
	// model's label argument or a geocoder and is rendered verbatim into
	// every session's BASE KNOWLEDGE block, so a newline in it could open a
	// line that poses as a user-confirmed fact or an instruction.
	loc.Label = oneLine(loc.Label)
	loc.Timezone = strings.TrimSpace(loc.Timezone)
	if err := validateCurrentLocation(loc, source); err != nil {
		return 0, err
	}
	value := map[string]any{
		"label":      loc.Label,
		"postalCode": oneLine(loc.PostalCode),
		"city":       oneLine(loc.City),
		"admin1":     oneLine(loc.Admin1),
		"country":    oneLine(loc.Country),
		"lat":        loc.Lat,
		"lon":        loc.Lon,
		"timezone":   loc.Timezone,
		"setAt":      setAt.UTC().Format(time.RFC3339),
		"source":     source,
	}
	version, _, err := s.mutateProfile(ctx, userID, func(profile map[string]any) bool {
		profile[currentLocationKey] = value
		return true
	})
	return version, err
}

// ClearCurrentLocation removes profile.currentLocation ("I'm back home")
// through the same versioned path. cleared is false — and nothing is written
// — when no current location was set.
func (s *Store) ClearCurrentLocation(ctx context.Context, userID string) (cleared bool, version int64, err error) {
	if userID == "" {
		return false, 0, errors.New("store: userID is required")
	}
	version, changed, err := s.mutateProfile(ctx, userID, func(profile map[string]any) bool {
		if _, present := profile[currentLocationKey]; !present {
			return false
		}
		delete(profile, currentLocationKey)
		return true
	})
	return changed, version, err
}

// mutateProfile applies fn to the account-level settings.profile map and
// writes the whole document back iff fn reports a change, retrying a lost
// optimistic-concurrency race up to autoApplyMaxAttempts times. It returns
// the committed (or, when nothing changed, the current) version.
func (s *Store) mutateProfile(ctx context.Context, userID string, fn func(profile map[string]any) bool) (int64, bool, error) {
	for attempt := 0; attempt < autoApplyMaxAttempts; attempt++ {
		doc, err := s.GetSettings(ctx, userID)
		if err != nil {
			return 0, false, err
		}
		expected := settingsDocVersion(doc)
		profile, ok := doc[profileAttr].(map[string]any)
		if !ok {
			profile = map[string]any{}
			doc[profileAttr] = profile
		}
		if !fn(profile) {
			return expected, false, nil
		}
		newVersion, err := s.PutSettings(ctx, userID, doc, expected)
		if errors.Is(err, ErrVersionConflict) {
			continue // another surface wrote first — re-read and re-apply
		}
		if err != nil {
			return 0, false, err
		}
		return newVersion, true, nil
	}
	return 0, false, ErrVersionConflict
}

// oneLine collapses every whitespace run (newlines included) to one space.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func validateCurrentLocation(loc Location, source string) error {
	switch {
	case source != CurrentLocationSourceCommand && source != CurrentLocationSourceGPS:
		return fmt.Errorf("%w: source must be %q or %q", ErrInvalidCurrentLocation,
			CurrentLocationSourceCommand, CurrentLocationSourceGPS)
	case !loc.Resolved():
		return fmt.Errorf("%w: a label and coordinates are required", ErrInvalidCurrentLocation)
	case loc.Lat < -90 || loc.Lat > 90 || loc.Lon < -180 || loc.Lon > 180:
		return fmt.Errorf("%w: coordinates out of range", ErrInvalidCurrentLocation)
	case loc.Timezone == "":
		return fmt.Errorf("%w: a timezone is required", ErrInvalidCurrentLocation)
	}
	if _, err := time.LoadLocation(loc.Timezone); err != nil {
		return fmt.Errorf("%w: unknown timezone %q", ErrInvalidCurrentLocation, loc.Timezone)
	}
	return nil
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
