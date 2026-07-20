package tools

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
)

// web_lookup is a keyless encyclopedia lookup built on Wikipedia's free
// APIs: opensearch to resolve the query to article titles, then the REST
// summary endpoint for a concise extract of the best match. No API key,
// no scraping — both are supported public endpoints.

// Vars (not consts) so unit tests — including web_research's background
// leg, which reuses this lookup — can point them at httptest fixtures.
var (
	wikiOpenSearchURL = "https://en.wikipedia.org/w/api.php"
	wikiSummaryURL    = "https://en.wikipedia.org/api/rest_v1/page/summary/"
)

func webLookupDefinition() *Definition {
	return &Definition{
		Name: "web_lookup",
		Description: "Look up a factual topic (encyclopedia-style summary) on Wikipedia and return a " +
			"concise summary with a source link. Use for people, places, things, and definitions.",
		Params: []ParamSpec{
			{Name: "query", Type: "string", Required: true, MinLen: 1, MaxLen: 200,
				Description: "The topic to look up, e.g. 'Grace Hopper' or 'transit method astronomy'."},
		},
		Handler: handleWebLookup,
	}
}

type wikiSummary struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Extract     string `json:"extract"`
	ContentURLs struct {
		Desktop struct {
			Page string `json:"page"`
		} `json:"desktop"`
	} `json:"content_urls"`
}

func handleWebLookup(ctx context.Context, deps *Deps, _ Invocation, args map[string]any) (map[string]any, *ToolError) {
	query := args["query"].(string)

	// Leg 1: opensearch — response is a positional JSON array:
	// [query, [titles...], [descriptions...], [urls...]].
	q := url.Values{}
	q.Set("action", "opensearch")
	q.Set("search", query)
	q.Set("limit", "4")
	q.Set("namespace", "0")
	q.Set("format", "json")
	var raw []json.RawMessage
	if err := httpGetJSON(ctx, deps.HTTPClient, wikiOpenSearchURL+"?"+q.Encode(), &raw); err != nil {
		deps.Log.Error("tools: wikipedia search failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "the lookup service is unavailable right now")
	}
	var titles []string
	if len(raw) >= 2 {
		_ = json.Unmarshal(raw[1], &titles)
	}
	if len(titles) == 0 {
		return nil, toolErrf(CodeNotFound, "no article found for %q", query)
	}

	// Leg 2: summary of the best match.
	best := titles[0]
	var summary wikiSummary
	titlePath := url.PathEscape(strings.ReplaceAll(best, " ", "_"))
	if err := httpGetJSON(ctx, deps.HTTPClient, wikiSummaryURL+titlePath, &summary); err != nil {
		deps.Log.Error("tools: wikipedia summary failed", "error", err.Error(), "title", best)
		return nil, toolErrf(CodeUpstreamError, "the lookup service is unavailable right now")
	}
	if summary.Extract == "" {
		return nil, toolErrf(CodeNotFound, "no readable summary available for %q", best)
	}

	out := map[string]any{
		"title":   summary.Title,
		"summary": summary.Extract,
		"source":  summary.ContentURLs.Desktop.Page,
	}
	if summary.Description != "" {
		out["description"] = summary.Description
	}
	if len(titles) > 1 {
		out["alternatives"] = titles[1:]
	}
	return out, nil
}
