package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// researchFixture supplies general search results and the separate Wikipedia
// lookup. General research must not fall back to unrelated encyclopedia text.
type researchFixture struct {
	server *httptest.Server

	wikiStatus  int
	lastQuery   string
	lastDays    int
	searchError *ToolError

	restore func()
}

func newResearchFixture(t *testing.T) *researchFixture {
	t.Helper()
	f := &researchFixture{wikiStatus: http.StatusOK}

	mux := http.NewServeMux()
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

	prevOpen, prevSummary := wikiOpenSearchURL, wikiSummaryURL
	wikiOpenSearchURL = f.server.URL + "/w/api.php"
	wikiSummaryURL = f.server.URL + "/api/rest_v1/page/summary/"
	f.restore = func() {
		wikiOpenSearchURL, wikiSummaryURL = prevOpen, prevSummary
	}
	t.Cleanup(f.restore)
	return f
}

func (f *researchFixture) search(_ context.Context, _ Invocation, query string, days int) (*WebResearchResult, *ToolError) {
	f.lastQuery, f.lastDays = query, days
	if f.searchError != nil {
		return nil, f.searchError
	}
	return &WebResearchResult{Summary: "A public news summary with cited evidence.", Sources: []WebResearchSource{{Title: "News report", URL: "https://news.example/report"}, {Title: "Public record", URL: "https://records.example/evidence"}}}, nil
}

func TestWebResearchReturnsGeneralSearchSources(t *testing.T) {
	fx := newResearchFixture(t)
	deps := newTestDeps()
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	deps.Now = func() time.Time { return fixedNow }
	deps.WebResearch = fx.search
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), invocation("web_research", map[string]any{
		"query": "claude realtime",
	}))
	require.True(t, res.OK, "error: %+v", res.Error)

	assert.Equal(t, 0, res.Output["recencyDays"], "no unrequested 30-day restriction")
	assert.Equal(t, "2026-07-17", res.Output["asOf"])
	assert.Equal(t, "claude realtime", fx.lastQuery)
	require.Len(t, res.Output["sources"], 2)
	assert.NotContains(t, res.Output, "background")
	assert.Contains(t, res.Output["note"], "publication dates")
	// A latest-news roundup reports what was found instead of claiming
	// that other developments could not be verified.
	assert.Contains(t, res.Output["note"], "For a latest-news request, report each item found")
}

func TestWebResearchUsesGeneralSearchAndPreservesHistoricalQueries(t *testing.T) {
	newResearchFixture(t) // the old index returns unrelated background
	d := newTestDeps()
	for _, query := range []string{"Williamsburg bank robbery November 2025", "recent India certificate authority security breach"} {
		t.Run(query, func(t *testing.T) {
			called := false
			d.WebResearch = func(_ context.Context, inv Invocation, got string, days int) (*WebResearchResult, *ToolError) {
				called = true
				require.Equal(t, "phone", inv.Surface)
				require.Equal(t, query, got)
				require.Zero(t, days, "an omitted recency window must preserve a historical query")
				return &WebResearchResult{Summary: "A cited news result.", Sources: []WebResearchSource{{Title: "News evidence", URL: "https://news.example/evidence"}}}, nil
			}
			r := newTestRegistry(t, d)
			res := r.Invoke(t.Context(), phoneInvocation("web_research", map[string]any{"query": query}, query))
			require.True(t, res.OK, "%+v", res.Error)
			require.True(t, called, "the phone must reach a general search provider")
			require.Equal(t, "A cited news result.", res.Output["summary"])
		})
	}
}

func TestWebResearchCustomDays(t *testing.T) {
	fx := newResearchFixture(t)
	deps := newTestDeps()
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	deps.Now = func() time.Time { return fixedNow }
	deps.WebResearch = fx.search
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), invocation("web_research", map[string]any{
		"query": "x", "days": float64(7),
	}))
	require.True(t, res.OK)
	assert.Equal(t, 7, res.Output["recencyDays"])
	assert.Equal(t, 7, fx.lastDays)
}

func TestWebResearchFailureDoesNotUseEncyclopediaBackground(t *testing.T) {
	fx := newResearchFixture(t)
	d := newTestDeps()
	d.WebResearch = fx.search
	r := newTestRegistry(t, d)
	fx.searchError = toolErrf(CodeUpstreamError, "search unavailable")
	res := r.Invoke(context.Background(), invocation("web_research", map[string]any{"query": "x"}))
	require.False(t, res.OK)
	assert.Equal(t, CodeUpstreamError, res.Error.Code)
	assert.Nil(t, res.Output)
	require.True(t, r.Invoke(t.Context(), invocation("web_lookup", map[string]any{"query": "x"})).OK, "the encyclopedia is available, but is not a news fallback")
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

// The model once sent a google.com search URL; the allow-list rightly refused
// it, so the url parameter must say that only two named domains are fetched.
func TestWebResearchURLDescriptionForbidsSearchPages(t *testing.T) {
	for _, p := range webResearchDefinition().Params {
		if p.Name == "url" {
			require.Contains(t, p.Description, "Never pass a search-engine or news-site URL")
			require.Contains(t, p.Description, "anthropic.com or openai.com")
			return
		}
	}
	t.Fatal("web_research has no url parameter")
}
