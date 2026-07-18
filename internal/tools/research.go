package tools

// web_research — the M10 recency-filtered research tool (FR-MEM-08/09).
// Real and keyless: recent material comes from the Hacker News Algolia
// search_by_date API (free, no key) filtered to the recency window (guide
// default: 30 days), encyclopedic background comes from the existing
// Wikipedia lookup, and an optional direct URL fetch is allowed ONLY for
// the authoritative-source allow-list (anthropic.com / openai.com per the
// default "AI is an emerging technology" guide). Every result carries a
// date so responses can cite source dates as the guide directs.

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// defaultResearchDays is the recency window applied when the caller does
// not pass one (FR-MEM-08: default 30 days).
const defaultResearchDays = 30

// maxDirectFetchChars caps the text returned from a direct allow-listed
// page fetch so one tool call can't flood the model context.
const maxDirectFetchChars = 6000

// hnSearchByDateURL is a var (not a const) so unit tests can point it at
// an httptest fixture.
var hnSearchByDateURL = "https://hn.algolia.com/api/v1/search_by_date"

// researchAllowedDomains is the authoritative-source allow-list for direct
// URL fetches: the requested host must equal one of these registrable
// domains or be a subdomain of one. Var for tests.
var researchAllowedDomains = []string{"anthropic.com", "openai.com"}

func webResearchDefinition() *Definition {
	return &Definition{
		Name: "web_research",
		Description: "Research a topic with a recency filter: returns recent items (with their " +
			"publication dates) plus encyclopedic background. Use for anything time-sensitive — " +
			"news, releases, developments — and always cite the date of what you report. " +
			"Optionally fetch one specific page from an allow-listed authoritative source " +
			"(anthropic.com, openai.com).",
		Params: []ParamSpec{
			{Name: "query", Type: "string", Required: true, MinLen: 1, MaxLen: 300,
				Description: "The topic to research, e.g. 'Anthropic model release'."},
			{Name: "days", Type: "integer", Min: floatPtr(1), Max: floatPtr(365),
				Description: "Only include items newer than this many days (default 30, per the active guide's recency directive)."},
			{Name: "url", Type: "string", MaxLen: 500,
				Description: "Optional: an exact https page URL to fetch directly instead of searching. Only allow-listed authoritative domains (anthropic.com, openai.com) are fetched."},
		},
		Handler: handleWebResearch,
	}
}

// hnSearchResponse is the subset of the Algolia HN response we consume.
type hnSearchResponse struct {
	Hits []struct {
		Title     string `json:"title"`
		URL       string `json:"url"`
		CreatedAt string `json:"created_at"`
		Points    int    `json:"points"`
		ObjectID  string `json:"objectID"`
	} `json:"hits"`
}

func handleWebResearch(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if rawURL, ok := args["url"].(string); ok && strings.TrimSpace(rawURL) != "" {
		return researchDirectFetch(ctx, deps, strings.TrimSpace(rawURL))
	}

	query := args["query"].(string)
	days := defaultResearchDays
	if d, ok := args["days"].(int); ok {
		days = d
	}
	now := deps.Now().UTC()
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)

	recent, hnErr := hnRecentSearch(ctx, deps, query, cutoff)
	if hnErr != nil {
		deps.Log.Error("tools: web_research recent search failed", "error", hnErr.Error())
	}

	// Background leg reuses the existing Wikipedia lookup; a miss or an
	// upstream failure here just drops the background section.
	background, wikiErr := handleWebLookup(ctx, deps, inv, map[string]any{"query": query})

	if hnErr != nil && wikiErr != nil {
		return nil, toolErrf(CodeUpstreamError, "the research sources are unavailable right now")
	}

	out := map[string]any{
		"query":       query,
		"recencyDays": days,
		"asOf":        now.Format("2006-01-02"),
		"note": "Cite the listed date alongside any time-sensitive fact. 'recent' items are " +
			"within the recency window; 'background' is encyclopedic and may be older.",
	}
	if hnErr == nil {
		out["recent"] = recent
		out["recentCount"] = len(recent)
	}
	if wikiErr == nil {
		out["background"] = background
	}
	return out, nil
}

// hnRecentSearch queries the HN Algolia date-ordered index restricted to
// stories created after cutoff, returning date-cited result entries.
func hnRecentSearch(ctx context.Context, deps *Deps, query string, cutoff time.Time) ([]map[string]any, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("tags", "story")
	q.Set("hitsPerPage", "6")
	q.Set("numericFilters", fmt.Sprintf("created_at_i>=%d", cutoff.Unix()))

	var resp hnSearchResponse
	if err := httpGetJSON(ctx, deps.HTTPClient, hnSearchByDateURL+"?"+q.Encode(), &resp); err != nil {
		return nil, err
	}

	results := make([]map[string]any, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		title := strings.TrimSpace(h.Title)
		if title == "" {
			continue
		}
		link := h.URL
		if link == "" {
			link = "https://news.ycombinator.com/item?id=" + h.ObjectID
		}
		date := h.CreatedAt
		if len(date) >= 10 {
			date = date[:10] // ISO date part
		}
		results = append(results, map[string]any{
			"title":  title,
			"url":    link,
			"date":   date,
			"points": h.Points,
		})
	}
	return results, nil
}

// researchDirectFetch retrieves one page from an allow-listed authoritative
// domain and returns its readable text with the fetch date cited.
func researchDirectFetch(ctx context.Context, deps *Deps, rawURL string) (map[string]any, *ToolError) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, toolErrf(CodeInvalidArgs, "url must be a valid https URL")
	}
	if !researchHostAllowed(u.Hostname()) {
		return nil, toolErrf(CodeInvalidArgs,
			"url host %q is not on the authoritative-source allow-list (%s)",
			u.Hostname(), strings.Join(researchAllowedDomains, ", "))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, toolErrf(CodeInvalidArgs, "url must be a valid https URL")
	}
	req.Header.Set("User-Agent", toolsUserAgent)
	req.Header.Set("Accept", "text/html, text/plain;q=0.9, */*;q=0.5")

	resp, err := deps.HTTPClient.Do(req)
	if err != nil {
		deps.Log.Error("tools: web_research direct fetch failed", "error", err.Error(), "url", rawURL)
		return nil, toolErrf(CodeUpstreamError, "the page could not be fetched right now")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, toolErrf(CodeUpstreamError, "the source returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		deps.Log.Error("tools: web_research direct read failed", "error", err.Error(), "url", rawURL)
		return nil, toolErrf(CodeUpstreamError, "the page could not be read")
	}

	text := stripHTML(string(body))
	if text == "" {
		return nil, toolErrf(CodeNotFound, "the page had no readable text content")
	}
	truncated := false
	if runes := []rune(text); len(runes) > maxDirectFetchChars {
		text = string(runes[:maxDirectFetchChars])
		truncated = true
	}

	out := map[string]any{
		"mode":      "direct",
		"url":       rawURL,
		"fetchedAt": deps.Now().UTC().Format("2006-01-02"),
		"content":   text,
		"note":      "Fetched directly from an allow-listed authoritative source; cite the source and the fetch date.",
	}
	if truncated {
		out["truncated"] = true
	}
	return out, nil
}

// researchHostAllowed reports whether host is an allow-listed domain or a
// subdomain of one (suffix match on a dot boundary — "notopenai.com" does
// not match "openai.com").
func researchHostAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, d := range researchAllowedDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	htmlTagRe     = regexp.MustCompile(`(?s)<[^>]*>`)
)

// stripHTML reduces an HTML document to whitespace-normalized readable
// text: script/style blocks removed, tags removed, entities unescaped.
func stripHTML(s string) string {
	s = scriptStyleRe.ReplaceAllString(s, " ")
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}
