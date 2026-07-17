package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// get_weather is a keyless, real forecast tool built on Open-Meteo
// (open-meteo.com): free geocoding + forecast APIs, no API key, generous
// rate limits. Two GETs per call: resolve the location name to
// coordinates, then fetch current conditions + a short daily forecast.

const (
	geocodeURL  = "https://geocoding-api.open-meteo.com/v1/search"
	forecastURL = "https://api.open-meteo.com/v1/forecast"

	// toolsUserAgent identifies our calls to the free upstream APIs
	// (both Open-Meteo and Wikipedia ask for a descriptive UA).
	toolsUserAgent = "live-ninja/1.0 (https://live.jeremy.ninja; proffitt.jeremy@gmail.com)"
)

func getWeatherDefinition() *Definition {
	return &Definition{
		Name: "get_weather",
		Description: "Get current weather conditions and a short daily forecast for a named place " +
			"(city, town, or 'city, state/country').",
		Params: []ParamSpec{
			{Name: "location", Type: "string", Required: true, MinLen: 2, MaxLen: 120,
				Description: "Place name to look up, e.g. 'Charlotte' or 'Paris, France'."},
			{Name: "days", Type: "integer", Min: floatPtr(1), Max: floatPtr(7),
				Description: "How many days of forecast to return (default 3)."},
			{Name: "units", Type: "string", Enum: []string{"imperial", "metric"},
				Description: "Unit system for temperatures and wind (default imperial)."},
		},
		Handler: handleGetWeather,
	}
}

type geocodeResponse struct {
	Results []struct {
		Name      string  `json:"name"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Country   string  `json:"country"`
		Admin1    string  `json:"admin1"`
		Timezone  string  `json:"timezone"`
	} `json:"results"`
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

func handleGetWeather(ctx context.Context, deps *Deps, _ Invocation, args map[string]any) (map[string]any, *ToolError) {
	location := args["location"].(string)
	days := 3
	if d, ok := args["days"].(int); ok {
		days = d
	}
	units := "imperial"
	if u, ok := args["units"].(string); ok && u != "" {
		units = u
	}

	// Leg 1: geocode.
	gq := url.Values{}
	gq.Set("name", location)
	gq.Set("count", "1")
	gq.Set("language", "en")
	gq.Set("format", "json")
	var geo geocodeResponse
	if err := httpGetJSON(ctx, deps.HTTPClient, geocodeURL+"?"+gq.Encode(), &geo); err != nil {
		deps.Log.Error("tools: geocoding failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "the weather service is unavailable right now")
	}
	if len(geo.Results) == 0 {
		return nil, toolErrf(CodeNotFound, "no place found matching %q", location)
	}
	place := geo.Results[0]

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

	locName := place.Name
	if place.Admin1 != "" {
		locName += ", " + place.Admin1
	}
	if place.Country != "" {
		locName += ", " + place.Country
	}

	return map[string]any{
		"location": map[string]any{
			"name":      locName,
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
