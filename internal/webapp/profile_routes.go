package webapp

// Base Knowledge profile HTTP surface (M15).
//
// Two routes plus the validator the settings PUT path calls:
//
//	GET  /api/v1/geocode?q=…      place typeahead backing the location pickers
//	POST /api/v1/profile/suggest  memory-derived prefill for the "About you" form
//
// The geocode route exists because of a house UI rule: a field whose valid
// values are enumerable or queryable must be a picker, never a blind text box.
// A home location is exactly that — so the form queries this endpoint and
// SAVES THE RESOLVED RECORD (label + lat/lon + timezone), rather than storing
// whatever the user typed and hoping a geocoder can re-derive it later at
// question time. That single decision is what makes get_weather able to skip
// geocoding entirely.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/memory"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// geocodeHTTPClient is the shared client for the keyless upstream. Package
// scoped (rather than on Deps) so this is the only place that decides the
// timeout; tests swap it.
var geocodeHTTPClient = &http.Client{Timeout: geocodeTimeout}

// geocodeSearchURL is Open-Meteo's keyless geocoding search (the same
// upstream internal/tools uses for get_weather, so a location the picker
// resolves is a location the weather tool can definitely serve).
const geocodeSearchURL = "https://geocoding-api.open-meteo.com/v1/search"

// geocodeUserAgent identifies our calls to the free upstream.
const geocodeUserAgent = "live-ninja/1.0 (https://live.jeremy.ninja; proffitt.jeremy@gmail.com)"

// geocodeResultLimit caps suggestions returned to the picker. Ten is enough
// to disambiguate any real place while staying one screenful.
const geocodeResultLimit = 10

// geocodeTimeout bounds the upstream call; the picker is interactive, so a
// slow geocoder must fail fast rather than hang the form.
const geocodeTimeout = 6 * time.Second

// RegisterProfileRoutes mounts the profile-support API. svc may be nil (no
// Bedrock embedder configured); the suggest route then answers 503
// not_configured, exactly like the rest of the memory surface.
func RegisterProfileRoutes(app *fiber.App, deps *Deps, svc *memory.Service) {
	api := app.Group("/api/v1", RequireAuth())
	api.Get("/geocode", handleGeocode(deps))
	api.Post("/profile/suggest", handleProfileSuggest(deps, svc))
}

// geocodeSuggestion is one pickable place, already in the exact shape the
// settings document stores — the client saves what it selected verbatim, so
// there is no second resolution step that could drift.
type geocodeSuggestion struct {
	Label      string  `json:"label"`
	City       string  `json:"city,omitempty"`
	Admin1     string  `json:"admin1,omitempty"`
	Country    string  `json:"country,omitempty"`
	PostalCode string  `json:"postalCode,omitempty"`
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
	Timezone   string  `json:"timezone,omitempty"`
}

// handleGeocode serves the location typeahead. Query values under two
// characters return an empty list rather than an error: the client debounces
// and calls on every keystroke, and an empty result is the honest answer to
// "T".
func handleGeocode(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		q := strings.TrimSpace(c.Query("q"))
		if len([]rune(q)) < 2 {
			return c.JSON(fiber.Map{"results": []geocodeSuggestion{}})
		}
		if len(q) > 120 {
			q = q[:120]
		}

		ctx, cancel := context.WithTimeout(c.Context(), geocodeTimeout)
		defer cancel()

		results, err := geocodeSearch(ctx, geocodeHTTPClient, q)
		if err != nil {
			deps.Log.Warn("webapp: geocode lookup failed", "error", err.Error())
			return c.Status(http.StatusBadGateway).JSON(fiber.Map{
				"error":   "geocode_unavailable",
				"message": "The place lookup service is unavailable right now. Try again in a moment.",
			})
		}
		return c.JSON(fiber.Map{"results": results})
	}
}

// geocodeSearch queries the upstream and projects its hits into storable
// suggestions. A postal-code query ("28078") and a name query ("Huntersville")
// both work — the upstream index handles each — and the "City, ST" compound is
// split before the call for the same reason internal/tools/geocode.go splits
// it: the index has no entry for the compound form.
func geocodeSearch(ctx context.Context, client *http.Client, q string) ([]geocodeSuggestion, error) {
	name, hint := q, ""
	if head, tail, found := strings.Cut(q, ","); found {
		name, hint = strings.TrimSpace(head), strings.TrimSpace(tail)
	}

	params := url.Values{}
	params.Set("name", name)
	params.Set("count", fmt.Sprintf("%d", geocodeResultLimit))
	params.Set("language", "en")
	params.Set("format", "json")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geocodeSearchURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", geocodeUserAgent)
	req.Header.Set("Accept", "application/json")

	if client == nil {
		client = &http.Client{Timeout: geocodeTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("geocode: upstream status %d", resp.StatusCode)
	}

	var payload struct {
		Results []struct {
			Name        string   `json:"name"`
			Latitude    float64  `json:"latitude"`
			Longitude   float64  `json:"longitude"`
			Country     string   `json:"country"`
			CountryCode string   `json:"country_code"`
			Admin1      string   `json:"admin1"`
			Postcodes   []string `json:"postcodes"`
			Timezone    string   `json:"timezone"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	out := make([]geocodeSuggestion, 0, len(payload.Results))
	for _, r := range payload.Results {
		// When the user typed a hint ("…, NC"), drop candidates that plainly
		// contradict it so the list they see is the list they meant.
		if hint != "" && !matchesHint(hint, r.Admin1, r.Country, r.CountryCode) {
			continue
		}
		s := geocodeSuggestion{
			City:     r.Name,
			Admin1:   r.Admin1,
			Country:  r.Country,
			Lat:      r.Latitude,
			Lon:      r.Longitude,
			Timezone: r.Timezone,
		}
		if len(r.Postcodes) > 0 {
			s.PostalCode = r.Postcodes[0]
		}
		s.Label = suggestionLabel(r.Name, r.Admin1, r.Country)
		out = append(out, s)
	}
	// A hint that filtered everything out is more likely a bad hint than a
	// bad place — fall back to the unfiltered list rather than an empty one.
	if len(out) == 0 && hint != "" {
		for _, r := range payload.Results {
			out = append(out, geocodeSuggestion{
				City: r.Name, Admin1: r.Admin1, Country: r.Country,
				Lat: r.Latitude, Lon: r.Longitude, Timezone: r.Timezone,
				Label: suggestionLabel(r.Name, r.Admin1, r.Country),
			})
		}
	}
	return out, nil
}

func suggestionLabel(name, admin1, country string) string {
	parts := []string{name}
	if admin1 != "" {
		parts = append(parts, admin1)
	}
	if country != "" {
		parts = append(parts, country)
	}
	return strings.Join(parts, ", ")
}

// matchesHint reports whether a candidate is consistent with the user's typed
// region hint, accepting US state abbreviations ("NC" → "North Carolina").
func matchesHint(hint, admin1, country, countryCode string) bool {
	h := strings.ToLower(strings.TrimSpace(hint))
	if h == "" {
		return true
	}
	for _, v := range []string{admin1, country} {
		if v == "" {
			continue
		}
		lv := strings.ToLower(v)
		if h == lv || strings.Contains(h, lv) || strings.Contains(lv, h) {
			return true
		}
	}
	if countryCode != "" && h == strings.ToLower(countryCode) {
		return true
	}
	return store.USStateName(h) != "" && store.USStateName(h) == strings.ToLower(admin1)
}

// handleProfileSuggest is the assisted seed (M15 "bootstrap from memory"):
// it searches the caller's own memory layer for the facts an "About you"
// form wants and returns them as SUGGESTIONS for the owner to confirm.
//
// It deliberately does not write anything. A silently-copied home address
// that happens to be wrong poisons every weather and time answer afterwards,
// which is precisely the failure this milestone exists to end — so the human
// confirms in the form, always.
func handleProfileSuggest(deps *Deps, svc *memory.Service) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		if svc == nil {
			return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{
				"error":   "not_configured",
				"message": "The memory layer is not available, so there is nothing to suggest from.",
			})
		}

		ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
		defer cancel()

		suggestions := map[string]any{}
		var sources []string

		for _, probe := range []struct {
			field string
			query string
		}{
			{"displayName", "the user's name"},
			{"homeLocation", "home address where the user lives"},
			{"workLocation", "where the user works, employer address"},
			{"contactEmail", "the user's email address"},
		} {
			hits, err := svc.Search(ctx, userID, probe.query, 3)
			if err != nil {
				deps.Log.Warn("webapp: profile suggest search failed",
					"field", probe.field, "error", err.Error())
				continue
			}
			for _, h := range hits {
				text := entitySummary(h.Entity)
				if text == "" {
					continue
				}
				suggestions[probe.field] = text
				sources = append(sources, probe.field+": "+text+
					" (from your memory entity \""+h.Entity.Name+"\")")
				break
			}
		}

		return c.JSON(fiber.Map{
			"suggestions": suggestions,
			"sources":     sources,
			"note": "These come from your memory layer and are unconfirmed. Nothing was saved — " +
				"review each one, pick a real location from the search box, then Save.",
		})
	}
}

// entitySummary renders a memory entity as the one-line fact a form field can
// be prefilled with: its most address/value-shaped attribute, else its name.
func entitySummary(e store.Entity) string {
	for _, key := range []string{"address", "value", "email", "location", "description", "text"} {
		if v, ok := e.Attrs[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return strings.TrimSpace(e.Name)
}

// validateProfile checks and normalizes the profile section of a settings
// PUT. Every field is optional; the whole section may be absent. Anything
// malformed is REJECTED rather than silently dropped, because a half-written
// profile is worse than none: it would put confidently wrong facts into every
// session's instructions.
func validateProfile(doc map[string]any) string {
	raw, present := doc["profile"]
	if !present || raw == nil {
		return ""
	}
	p, ok := raw.(map[string]any)
	if !ok {
		return "profile must be an object"
	}

	if msg := checkProfileString(p, "displayName", 80); msg != "" {
		return msg
	}
	if msg := checkProfileString(p, "pronouns", 32); msg != "" {
		return msg
	}
	if msg := checkProfileString(p, "locale", 20); msg != "" {
		return msg
	}
	if msg := checkProfileString(p, "contactEmail", 254); msg != "" {
		return msg
	}
	if email, _ := p["contactEmail"].(string); email != "" && !strings.Contains(email, "@") {
		return "profile.contactEmail must be an email address"
	}

	switch u := p["units"].(type) {
	case nil:
		p["units"] = store.UnitsImperial
	case string:
		if !oneOf(u, store.UnitsImperial, store.UnitsMetric) {
			return "profile.units must be imperial or metric"
		}
	default:
		return "profile.units must be a string"
	}

	for _, key := range []string{"homeLocation", "workLocation"} {
		if msg := validateProfileLocation(p, key); msg != "" {
			return msg
		}
	}

	switch q := p["quietHours"].(type) {
	case nil:
		// absent / explicitly cleared
	case map[string]any:
		start, _ := q["start"].(string)
		end, _ := q["end"].(string)
		if start == "" && end == "" {
			p["quietHours"] = nil
			break
		}
		if !isHHMM(start) || !isHHMM(end) {
			return "profile.quietHours start and end must both be 24h HH:MM times"
		}
	default:
		return "profile.quietHours must be an object or null"
	}

	switch notes := p["notes"].(type) {
	case nil:
		p["notes"] = []any{}
	case []any:
		if len(notes) > 20 {
			return "profile.notes is limited to 20 entries"
		}
		cleaned := make([]any, 0, len(notes))
		for _, n := range notes {
			s, ok := n.(string)
			if !ok {
				return "profile.notes entries must be strings"
			}
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if len([]rune(s)) > 200 {
				return "profile.notes entries must be at most 200 characters"
			}
			cleaned = append(cleaned, s)
		}
		p["notes"] = cleaned
	default:
		return "profile.notes must be an array of strings"
	}

	return ""
}

// validateProfileLocation enforces the geocode-verified shape. A location
// without usable coordinates is rejected outright — the only supported way to
// set one is picking a geocoder result, and accepting a coordinate-less
// location would quietly recreate the free-text field this design removed.
func validateProfileLocation(p map[string]any, key string) string {
	raw, present := p[key]
	if !present || raw == nil {
		return ""
	}
	loc, ok := raw.(map[string]any)
	if !ok {
		return "profile." + key + " must be an object or null"
	}
	// An empty object is how a client clears the field.
	if len(loc) == 0 {
		p[key] = nil
		return ""
	}

	label, _ := loc["label"].(string)
	if strings.TrimSpace(label) == "" || len([]rune(label)) > 160 {
		return "profile." + key + ".label must be a non-empty label of at most 160 characters"
	}
	lat, latOK := numberVal(loc["lat"])
	lon, lonOK := numberVal(loc["lon"])
	if !latOK || !lonOK {
		return "profile." + key + " must carry numeric lat and lon — pick a place from the search results"
	}
	if lat < -90 || lat > 90 {
		return "profile." + key + ".lat must be between -90 and 90"
	}
	if lon < -180 || lon > 180 {
		return "profile." + key + ".lon must be between -180 and 180"
	}
	loc["lat"], loc["lon"] = lat, lon

	for field, max := range map[string]int{
		"postalCode": 16, "city": 80, "admin1": 80, "country": 80, "timezone": 64,
	} {
		if v, present := loc[field]; present && v != nil {
			s, ok := v.(string)
			if !ok || len([]rune(s)) > max {
				return fmt.Sprintf("profile.%s.%s must be a string of at most %d characters", key, field, max)
			}
		}
	}
	if tz, _ := loc["timezone"].(string); tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return "profile." + key + ".timezone must be a valid IANA timezone id"
		}
	}
	return ""
}

// isHHMM reports whether s is a 24-hour HH:MM time.
func isHHMM(s string) bool {
	_, err := time.Parse("15:04", s)
	return err == nil
}

// checkProfileString bounds one optional string field and normalizes an
// absent/null value to "".
func checkProfileString(p map[string]any, key string, max int) string {
	switch v := p[key].(type) {
	case nil:
		p[key] = ""
	case string:
		trimmed := strings.TrimSpace(v)
		if len([]rune(trimmed)) > max {
			return fmt.Sprintf("profile.%s must be at most %d characters", key, max)
		}
		p[key] = trimmed
	default:
		return "profile." + key + " must be a string"
	}
	return ""
}
