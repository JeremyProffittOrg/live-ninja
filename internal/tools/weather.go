package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// locationSource labels how the coordinates were chosen, so the model (and
// the tool-call Details panel) can see when an answer came from the profile
// rather than from an argument it supplied.
func locationSource(argLocation string) string {
	if argLocation == "" {
		return "profile-home"
	}
	return "geocoded"
}

// get_weather is a keyless, real forecast tool built on Open-Meteo
// (open-meteo.com): free geocoding + forecast APIs, no API key, generous
// rate limits. Two GETs per call: resolve the location name to
// coordinates, then fetch current conditions + a short daily forecast.

// Upstream endpoints. Vars rather than consts purely as a test seam: the
// geocoding tests point them at an httptest server to assert what was
// actually asked of the geocoder (M15 — that the "City, ST" compound is split
// before the call, and that a profile-home question makes no call at all).
var (
	geocodeURL  = "https://geocoding-api.open-meteo.com/v1/search"
	forecastURL = "https://api.open-meteo.com/v1/forecast"
)

const (

	// toolsUserAgent identifies our calls to the free upstream APIs
	// (both Open-Meteo and Wikipedia ask for a descriptive UA).
	toolsUserAgent = "live-ninja/1.0 (https://live.jeremy.ninja; proffitt.jeremy@gmail.com)"
)

func getWeatherDefinition() *Definition {
	return &Definition{
		Name: "get_weather",
		Description: "Get current weather conditions and a short daily forecast. " +
			"Omit location to use the user's home location from your base knowledge — that is the " +
			"normal case and the most accurate one. Pass location only when the user asks about a " +
			"different place.",
		Params: []ParamSpec{
			// Optional since M15: with a profile home location the handler
			// goes straight to coordinates and skips geocoding entirely, so
			// the single most common weather question needs no argument the
			// model could get wrong.
			{Name: "location", Type: "string", MinLen: 2, MaxLen: 120,
				Description: "Only when asking about somewhere other than home: a place name such as " +
					"'Charlotte', 'Huntersville, NC', 'Paris, France', or a postal code."},
			{Name: "days", Type: "integer", Min: floatPtr(1), Max: floatPtr(7),
				Description: "How many days of forecast to return (default 3)."},
			{Name: "units", Type: "string", Enum: []string{"imperial", "metric"},
				Description: "Unit system for temperatures and wind. Defaults to the user's preferred units."},
		},
		Handler: handleGetWeather,
	}
}

type forecastResponse struct {
	Current struct {
		Time                string  `json:"time"`
		Temperature2m       float64 `json:"temperature_2m"`
		ApparentTemperature float64 `json:"apparent_temperature"`
		RelativeHumidity2m  float64 `json:"relative_humidity_2m"`
		WeatherCode         int     `json:"weather_code"`
		WindSpeed10m        float64 `json:"wind_speed_10m"`
	} `json:"current"`
	Daily struct {
		Time                        []string  `json:"time"`
		WeatherCode                 []int     `json:"weather_code"`
		Temperature2mMax            []float64 `json:"temperature_2m_max"`
		Temperature2mMin            []float64 `json:"temperature_2m_min"`
		PrecipitationProbabilityMax []float64 `json:"precipitation_probability_max"`
	} `json:"daily"`
}

func handleGetWeather(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	profile := deps.profileFor(ctx, inv.UserID)

	days := 3
	if d, ok := args["days"].(int); ok {
		days = d
	}
	// Units come from the profile unless the caller asked for a specific
	// system ("...in celsius"), which still wins.
	units := profile.UnitsOrDefault()
	if u, ok := args["units"].(string); ok && u != "" {
		units = u
	}

	location, _ := args["location"].(string)
	location = strings.TrimSpace(location)

	// Leg 1: resolve coordinates.
	//
	// The happy path has no leg 1 at all: with no location argument and a
	// geocode-verified home in the profile, the coordinates are already known
	// and correct, so the whole class of "which Paris did you mean" and
	// "City, ST returns nothing" failures simply cannot occur.
	var place geoCandidate
	switch {
	case location == "":
		home := profile.Home()
		if !home.Resolved() {
			return nil, toolErrf(CodeInvalidArgs,
				"no location given and no home location is set in the user's profile — "+
					"ask which place they mean, or have them set a home location in Settings")
		}
		// Label() composes name + admin1 + country, so feed it the CITY and
		// let it rebuild the label — passing the already-composed
		// home.Label here would render "Huntersville, North Carolina,
		// United States, North Carolina, United States".
		place = geoCandidate{
			Name:      home.City,
			Latitude:  home.Lat,
			Longitude: home.Lon,
			Country:   home.Country,
			Admin1:    home.Admin1,
			Timezone:  home.Timezone,
		}
		if place.Name == "" {
			// A stored location without a separate city field: use the label
			// verbatim and suppress the parts that would duplicate it.
			place.Name, place.Admin1, place.Country = home.Label, "", ""
		}
	default:
		resolved, terr := resolvePlace(ctx, deps, location, profile.Home())
		if terr != nil {
			return nil, terr
		}
		place = resolved
	}

	// Leg 2: forecast.
	fq := url.Values{}
	fq.Set("latitude", strconv.FormatFloat(place.Latitude, 'f', 4, 64))
	fq.Set("longitude", strconv.FormatFloat(place.Longitude, 'f', 4, 64))
	fq.Set("current", "temperature_2m,apparent_temperature,relative_humidity_2m,weather_code,wind_speed_10m")
	fq.Set("daily", "weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max")
	fq.Set("forecast_days", strconv.Itoa(days))
	fq.Set("timezone", "auto")
	tempUnit, windUnit := "fahrenheit", "mph"
	if units == "metric" {
		tempUnit, windUnit = "celsius", "kmh"
	}
	fq.Set("temperature_unit", tempUnit)
	fq.Set("wind_speed_unit", windUnit)

	var fc forecastResponse
	if err := httpGetJSON(ctx, deps.HTTPClient, forecastURL+"?"+fq.Encode(), &fc); err != nil {
		deps.Log.Error("tools: forecast fetch failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "the weather service is unavailable right now")
	}

	daily := make([]map[string]any, 0, len(fc.Daily.Time))
	for i, day := range fc.Daily.Time {
		entry := map[string]any{"date": day}
		if i < len(fc.Daily.Temperature2mMax) {
			entry["high"] = fc.Daily.Temperature2mMax[i]
		}
		if i < len(fc.Daily.Temperature2mMin) {
			entry["low"] = fc.Daily.Temperature2mMin[i]
		}
		if i < len(fc.Daily.PrecipitationProbabilityMax) {
			entry["precipChancePct"] = fc.Daily.PrecipitationProbabilityMax[i]
		}
		if i < len(fc.Daily.WeatherCode) {
			entry["conditions"] = wmoDescription(fc.Daily.WeatherCode[i])
		}
		daily = append(daily, entry)
	}

	locName := place.Label()

	return map[string]any{
		"location": map[string]any{
			"name":      locName,
			"source":    locationSource(location),
			"latitude":  place.Latitude,
			"longitude": place.Longitude,
			"timezone":  place.Timezone,
		},
		"units": units,
		"current": map[string]any{
			"asOf":        fc.Current.Time,
			"temperature": fc.Current.Temperature2m,
			"feelsLike":   fc.Current.ApparentTemperature,
			"humidityPct": fc.Current.RelativeHumidity2m,
			"windSpeed":   fc.Current.WindSpeed10m,
			"conditions":  wmoDescription(fc.Current.WeatherCode),
		},
		"daily": daily,
	}, nil
}

// httpGetJSON GETs a URL with the shared UA and decodes the JSON body,
// treating any non-2xx as an error. Bodies are capped at 1 MiB — these
// upstreams return small documents; anything bigger is wrong.
func httpGetJSON(ctx context.Context, client *http.Client, rawURL string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", toolsUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET %s: status %d", req.URL.Host, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, target)
}

// wmoDescription maps WMO weather interpretation codes (Open-Meteo's
// weather_code) to speakable text.
func wmoDescription(code int) string {
	switch code {
	case 0:
		return "clear sky"
	case 1:
		return "mainly clear"
	case 2:
		return "partly cloudy"
	case 3:
		return "overcast"
	case 45, 48:
		return "fog"
	case 51, 53, 55:
		return "drizzle"
	case 56, 57:
		return "freezing drizzle"
	case 61:
		return "light rain"
	case 63:
		return "moderate rain"
	case 65:
		return "heavy rain"
	case 66, 67:
		return "freezing rain"
	case 71:
		return "light snow"
	case 73:
		return "moderate snow"
	case 75:
		return "heavy snow"
	case 77:
		return "snow grains"
	case 80, 81, 82:
		return "rain showers"
	case 85, 86:
		return "snow showers"
	case 95:
		return "thunderstorm"
	case 96, 99:
		return "thunderstorm with hail"
	default:
		return fmt.Sprintf("unknown conditions (code %d)", code)
	}
}
