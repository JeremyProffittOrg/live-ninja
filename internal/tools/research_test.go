package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// researchFixture stands in for all three upstreams (HN Algolia +
// Wikipedia opensearch/summary) behind one httptest server, and records
// the HN query parameters for assertions.
type researchFixture struct {
	server *httptest.Server

	hnStatus   int
	wikiStatus int
	lastHNURL  *url.URL

	restore func()
}

func newResearchFixture(t *testing.T) *researchFixture {
	t.Helper()
	f := &researchFixture{hnStatus: http.StatusOK, wikiStatus: http.StatusOK}

	mux := http.NewServeMux()
	mux.HandleFunc("/hn", func(w http.ResponseWriter, r *http.Request) {
		f.lastHNURL = r.URL
		if f.hnStatus != http.StatusOK {
			w.WriteHeader(f.hnStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hits": []map[string]any{
				{"title": "Claude ships new realtime API", "url": "https://example.com/claude",
					"created_at": "2026-07-10T12:00:00Z", "points": 321, "objectID": "101"},
				{"title": "Show HN: keyless research", "url": "",
					"created_at": "2026-07-01T08:30:00Z", "points": 42, "objectID": "102"},
			},
		})
	})
	mux.HandleFunc("/w/api.php", func(w http.ResponseWriter, r *http.Request) {
		if f.wikiStatus != http.StatusOK {
			w.WriteHeader(f.wikiStatus)
			return
		}
		_, _ = w.Write([]byte(`["ai",["Artificial intelligence"],[""],["https://en.wikipedia.org/wiki/Artificial_intelligence"]]`))
	})
	mux.HandleFunc("/api/rest_v1/page/summary/", func(w http.ResponseWriter, r *http.Request) {
		if f.wikiStatus != http.StatusOK {
			w.WriteHeader(f.wikiStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"title":   "Artificial intelligence",
			"extract": "AI is intelligence demonstrated by machines.",
			"content_urls": map[string]any{
				"desktop": map[string]any{"page": "https://en.wikipedia.org/wiki/Artificial_intelligence"},
			},
		})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	prevHN, prevOpen, prevSummary := hnSearchByDateURL, wikiOpenSearchURL, wikiSummaryURL
	hnSearchByDateURL = f.server.URL + "/hn"
	wikiOpenSearchURL = f.server.URL + "/w/api.php"
	wikiSummaryURL = f.server.URL + "/api/rest_v1/page/summary/"
	f.restore = func() {
		hnSearchByDateURL, wikiOpenSearchURL, wikiSummaryURL = prevHN, prevOpen, prevSummary
	}
	t.Cleanup(f.restore)
	return f
}

func TestWebResearchRecentAndBackground(t *testing.T) {
	fx := newResearchFixture(t)
	deps := newTestDeps()
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	deps.Now = func() time.Time { return fixedNow }
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), invocation("web_research", map[string]any{
		"query": "claude realtime",
	}))
	require.True(t, res.OK, "error: %+v", res.Error)

	// Default recency window is 30 days and is passed to HN as a
	// created_at_i floor.
	assert.Equal(t, defaultResearchDays, res.Output["recencyDays"])
	assert.Equal(t, "2026-07-17", res.Output["asOf"])
	require.NotNil(t, fx.lastHNURL)
	wantCutoff := fixedNow.Add(-30 * 24 * time.Hour).Unix()
	assert.Equal(t, fmt.Sprintf("created_at_i>=%d", wantCutoff), fx.lastHNURL.Query().Get("numericFilters"))
	assert.Equal(t, "claude realtime", fx.lastHNURL.Query().Get("query"))

	// Recent items carry cited dates; a missing story URL falls back to
	// the HN item link.
	recent := res.Output["recent"].([]map[string]any)
	require.Len(t, recent, 2)
	assert.Equal(t, "2026-07-10", recent[0]["date"])
	assert.Equal(t, "https://example.com/claude", recent[0]["url"])
	assert.Equal(t, "https://news.ycombinator.com/item?id=102", recent[1]["url"])

	// Background leg comes from the Wikipedia lookup.
	background := res.Output["background"].(map[string]any)
	assert.Equal(t, "Artificial intelligence", background["title"])

	note := res.Output["note"].(string)
	assert.Contains(t, note, "Cite the listed date")
}

func TestWebResearchCustomDays(t *testing.T) {
	fx := newResearchFixture(t)
	deps := newTestDeps()
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	deps.Now = func() time.Time { return fixedNow }
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), invocation("web_research", map[string]any{
		"query": "x", "days": float64(7),
	}))
	require.True(t, res.OK)
	assert.Equal(t, 7, res.Output["recencyDays"])
	wantCutoff := fixedNow.Add(-7 * 24 * time.Hour).Unix()
	assert.Equal(t, fmt.Sprintf("created_at_i>=%d", wantCutoff), fx.lastHNURL.Query().Get("numericFilters"))
}

func TestWebResearchSurvivesOneUpstreamDown(t *testing.T) {
	fx := newResearchFixture(t)
	r := newTestRegistry(t, newTestDeps())

	// HN down, Wikipedia up: still OK, background only.
	fx.hnStatus = http.StatusInternalServerError
	res := r.Invoke(context.Background(), invocation("web_research", map[string]any{"query": "x"}))
	require.True(t, res.OK)
	_, hasRecent := res.Output["recent"]
	assert.False(t, hasRecent)
	assert.NotNil(t, res.Output["background"])

	// Both down: upstream_error.
	fx.wikiStatus = http.StatusInternalServerError
	res = r.Invoke(context.Background(), invocation("web_research", map[string]any{"query": "x"}))
	require.False(t, res.OK)
	assert.Equal(t, CodeUpstreamError, res.Error.Code)
}

func TestWebResearchDirectFetchAllowListEnforced(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())

	for name, raw := range map[string]string{
		"non-allow-listed host": "https://evil.example.com/page",
		"lookalike domain":      "https://notopenai.com/docs",
		"http not https":        "http://openai.com/docs",
	} {
		t.Run(name, func(t *testing.T) {
			res := r.Invoke(context.Background(), invocation("web_research", map[string]any{
				"query": "x", "url": raw,
			}))
			require.False(t, res.OK)
			assert.Equal(t, CodeInvalidArgs, res.Error.Code)
		})
	}
}

func TestWebResearchDirectFetchStripsHTML(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>Doc</title><style>p{color:red}</style>` +
			`<script>alert("no")</script></head>` +
			`<body><h1>Model &amp; safety</h1><p>Real   content here.</p></body></html>`))
	}))
	defer ts.Close()

	// Allow-list the test server's host and use its TLS-trusting client.
	tsURL, err := url.Parse(ts.URL)
	require.NoError(t, err)
	prevDomains := researchAllowedDomains
	researchAllowedDomains = append([]string{tsURL.Hostname()}, prevDomains...)
	defer func() { researchAllowedDomains = prevDomains }()

	deps := newTestDeps()
	deps.HTTPClient = ts.Client()
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), invocation("web_research", map[string]any{
		"query": "x", "url": ts.URL + "/announcement",
	}))
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "direct", res.Output["mode"])
	content := res.Output["content"].(string)
	assert.Contains(t, content, "Model & safety Real content here.")
	assert.NotContains(t, content, "<p>")
	assert.NotContains(t, content, "alert")
	assert.NotContains(t, content, "color:red")
	assert.NotEmpty(t, res.Output["fetchedAt"])
}

func TestResearchHostAllowed(t *testing.T) {
	assert.True(t, researchHostAllowed("anthropic.com"))
	assert.True(t, researchHostAllowed("www.anthropic.com"))
	assert.True(t, researchHostAllowed("docs.ANTHROPIC.com"))
	assert.True(t, researchHostAllowed("platform.openai.com"))
	assert.False(t, researchHostAllowed("notanthropic.com"))
	assert.False(t, researchHostAllowed("anthropic.com.evil.net"))
	assert.False(t, researchHostAllowed("example.com"))
}

func TestStripHTML(t *testing.T) {
	in := "<div>a&nbsp;b</div>\n\n<span>c</span>"
	out := stripHTML(in)
	assert.Equal(t, "a b c", strings.Join(strings.Fields(out), " "))
}

// TestWebResearchDirectFetchBlocksRedirectOffAllowList is the regression for
// the SSRF finding: an allow-listed host that 3xx-redirects to a target NOT on
// the allow-list must be blocked at the redirect hop (CheckRedirect fires
// before the next hop is dialed, so no second server is needed).
func TestWebResearchDirectFetchBlocksRedirectOffAllowList(t *testing.T) {
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Open-redirect to a DIFFERENT host that is not allow-listed.
		http.Redirect(w, r, "https://attacker.example.net/latest/meta-data", http.StatusFound)
	}))
	defer redirector.Close()

	redirURL, err := url.Parse(redirector.URL)
	require.NoError(t, err)
	prevDomains := researchAllowedDomains
	researchAllowedDomains = append([]string{redirURL.Hostname()}, prevDomains...) // only the redirector
	defer func() { researchAllowedDomains = prevDomains }()

	deps := newTestDeps()
	deps.HTTPClient = redirector.Client()
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), invocation("web_research", map[string]any{
		"query": "x", "url": redirector.URL + "/open-redirect",
	}))
	require.False(t, res.OK, "redirect to a non-allow-listed host must be blocked, got %+v", res.Output)
}
