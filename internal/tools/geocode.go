package tools

// Geocoding for get_weather (M15, base-knowledge-plan.md P3).
//
// The old behaviour was one query with count=1: the raw user/model string went
// straight to Open-Meteo's name search and the first global hit won. That fails
// in two ways the owner hit daily:
//
//   - "Huntersville, NC" returns ZERO results. The name index matches bare
//     names ("Charlotte") and postal codes ("28078"), not "City, ST" compounds
//     — which is exactly the shape a US-centric model produces.
//   - "Paris" resolves to France even for a user in Texas, because count=1
//     takes the most populous global match with no notion of where the user is.
//
// The fix is to split the compound, ask for several candidates, and rank them
// against what we actually know about the user (their profile home), rather
// than trusting position 1 of an unranked global list.

import (
	"context"
	"math"
	"net/url"
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// geoCandidate is one ranked geocoder hit.
type geoCandidate struct {
	Name        string  `json:"name"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Admin1      string  `json:"admin1"`
	Timezone    string  `json:"timezone"`
}

// Label renders the human-readable place name reported back to the model.
func (c geoCandidate) Label() string {
	parts := []string{c.Name}
	if c.Admin1 != "" {
		parts = append(parts, c.Admin1)
	}
	if c.Country != "" {
		parts = append(parts, c.Country)
	}
	return strings.Join(parts, ", ")
}

type geocodeSearchResponse struct {
	Results []geoCandidate `json:"results"`
}

// geocodeCandidateCount is how many hits to ask for before ranking. Five is
// enough to contain the right "Paris"/"Springfield" in practice without
// bloating the response.
const geocodeCandidateCount = 5

// resolvePlace geocodes a free-form place string into a single best
// candidate, using home (when resolved) to break ties toward the user's part
// of the world.
//
// The query is split on the first comma: the head is what the name index is
// searched for, the tail is treated as an admin1/country hint used only for
// ranking. A bare postal code is passed through whole — the index handles
// those natively and they carry no comma anyway.
func resolvePlace(ctx context.Context, deps *Deps, query string, home store.Location) (geoCandidate, *ToolError) {
	name, hint := splitPlaceQuery(query)
	if name == "" {
		return geoCandidate{}, toolErrf(CodeInvalidArgs, "location must name a place")
	}

	gq := url.Values{}
	gq.Set("name", name)
	gq.Set("count", itoa(geocodeCandidateCount))
	gq.Set("language", "en")
	gq.Set("format", "json")

	var resp geocodeSearchResponse
	if err := httpGetJSON(ctx, deps.HTTPClient, geocodeURL+"?"+gq.Encode(), &resp); err != nil {
		deps.Log.Error("tools: geocoding failed", "error", err.Error())
		return geoCandidate{}, toolErrf(CodeUpstreamError, "the weather service is unavailable right now")
	}
	if len(resp.Results) == 0 {
		return geoCandidate{}, toolErrf(CodeNotFound, "no place found matching %q", query)
	}

	return rankCandidates(resp.Results, hint, home), nil
}

// splitPlaceQuery separates "Huntersville, NC" into ("Huntersville", "NC").
// A query without a comma is all name and no hint. Only the FIRST comma
// splits, so "Paris, Ile-de-France, France" keeps its whole tail as the hint.
func splitPlaceQuery(query string) (name, hint string) {
	q := strings.TrimSpace(query)
	head, tail, found := strings.Cut(q, ",")
	if !found {
		return q, ""
	}
	return strings.TrimSpace(head), strings.TrimSpace(tail)
}

// rankCandidates picks the best hit. Scoring, in descending weight:
//
//	+40  the hint names this candidate's admin1 (state/province), directly
//	     or via a US state abbreviation ("NC" → "North Carolina")
//	+30  the hint names this candidate's country or country code
//	+10  no hint at all, but the candidate is in the same country as home
//	 0-9 proximity to home, closer is better (also the tiebreaker)
//
// Ties fall back to the geocoder's own order, which is population-ranked —
// so with no profile and no hint the behaviour matches the old count=1 path.
func rankCandidates(cands []geoCandidate, hint string, home store.Location) geoCandidate {
	best := cands[0]
	bestScore := math.Inf(-1)
	for i, c := range cands {
		score := scoreCandidate(c, hint, home)
		// Strictly-greater keeps the earliest (most populous) of equals.
		if score > bestScore {
			best, bestScore = cands[i], score
		}
	}
	return best
}

func scoreCandidate(c geoCandidate, hint string, home store.Location) float64 {
	var score float64
	h := strings.ToLower(strings.TrimSpace(hint))

	if h != "" {
		admin := strings.ToLower(c.Admin1)
		if admin != "" && (h == admin || strings.Contains(h, admin) ||
			usStateName(h) == admin) {
			score += 40
		}
		country := strings.ToLower(c.Country)
		code := strings.ToLower(c.CountryCode)
		if (country != "" && (h == country || strings.Contains(h, country))) ||
			(code != "" && h == code) {
			score += 30
		}
	}

	if home.Resolved() {
		if h == "" && c.Country != "" && strings.EqualFold(c.Country, home.Country) {
			score += 10
		}
		// Proximity: 9 points at zero distance decaying to ~0 at half the
		// globe. Never enough to outrank an explicit admin/country hint —
		// "Paris, France" from a Texas home still resolves to France.
		km := haversineKM(home.Lat, home.Lon, c.Latitude, c.Longitude)
		score += 9 * math.Exp(-km/2000)
	}
	return score
}

// usStateName delegates to the shared table in internal/store so the weather
// tool and the settings location picker rank "City, ST" identically.
func usStateName(abbr string) string { return store.USStateName(abbr) }

// haversineKM is the great-circle distance between two coordinates.
func haversineKM(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusKM = 6371.0
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusKM * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// itoa avoids importing strconv in this file for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
