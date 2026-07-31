package ghost

import "testing"

// fleet mirrors the real listing shape: ghost-cli returns most-recently-pushed
// first, so index order is meaningful and the matcher's tie-break relies on it.
var fleet = []Repo{
	{Repo: "JeremyProffittOrg/live-ninja", Owner: "JeremyProffittOrg", Name: "live-ninja"},
	{Repo: "JeremyProffittOrg/ghost-cli", Owner: "JeremyProffittOrg", Name: "ghost-cli"},
	{Repo: "JeremyProffittOrg/aws-cost-reporting", Owner: "JeremyProffittOrg", Name: "aws-cost-reporting"},
	{Repo: "JeremyProffittOrg/credential-rotation", Owner: "JeremyProffittOrg", Name: "credential-rotation"},
	{Repo: "JeremyProffittOrg/ghost-agent-docs", Owner: "JeremyProffittOrg", Name: "ghost-agent-docs"},
}

func TestNormalizeCollapsesSpokenVariation(t *testing.T) {
	for _, in := range []string{"live-ninja", "Live Ninja", "LIVE  NINJA!", "live_ninja", "liveninja"} {
		if got := Normalize(in); got != "liveninja" {
			t.Errorf("Normalize(%q) = %q, want liveninja", in, got)
		}
	}
}

// The whole point of the matcher: what the owner SAYS has to reach what the repo
// is CALLED, across hyphens, spacing, case and spoken padding.
func TestBestResolvesSpokenNames(t *testing.T) {
	cases := map[string]string{
		"live ninja":                   "JeremyProffittOrg/live-ninja",
		"Live-Ninja":                   "JeremyProffittOrg/live-ninja",
		"liveninja":                    "JeremyProffittOrg/live-ninja",
		"the live ninja repo":          "JeremyProffittOrg/live-ninja",
		"JeremyProffittOrg/live-ninja": "JeremyProffittOrg/live-ninja",
		"cost reporting":               "JeremyProffittOrg/aws-cost-reporting",
		"aws cost reporting":           "JeremyProffittOrg/aws-cost-reporting",
		"credential rotation":          "JeremyProffittOrg/credential-rotation",
		"update the live ninja app":    "JeremyProffittOrg/live-ninja",
	}
	for query, want := range cases {
		t.Run(query, func(t *testing.T) {
			got, ok := Best(fleet, query)
			if !ok {
				t.Fatalf("Best(%q) found nothing; ranked=%v", query, Rank(fleet, query))
			}
			if got.Repo != want {
				t.Errorf("Best(%q) = %q, want %q", query, got.Repo, want)
			}
		})
	}
}

// Launching a coding agent on the wrong repo is expensive to undo, so a close
// call must return "ask them" rather than a confident guess.
func TestBestRefusesAmbiguousQueries(t *testing.T) {
	// "ghost" matches both ghost-cli (prefix) and ghost-agent-docs (prefix).
	if repo, ok := Best(fleet, "ghost"); ok {
		t.Errorf("Best(\"ghost\") confidently returned %q; it is ambiguous", repo.Repo)
	}
	ranked := Rank(fleet, "ghost")
	if len(ranked) < 2 {
		t.Fatalf("expected at least two candidates for \"ghost\", got %d", len(ranked))
	}
	names := Candidates(ranked, 2)
	if len(names) != 2 {
		t.Fatalf("Candidates returned %d, want 2", len(names))
	}
}

// A near-miss full "owner/name" is exactly what the model sends when it has
// almost the right repo. Scoring only against the name half would find nothing
// (the owner prefix drowns the signal), leaving the owner with a flat failure
// instead of a choice.
func TestRankHandlesNearMissFullNames(t *testing.T) {
	ranked := Rank(fleet, "JeremyProffittOrg/ghost")
	if len(ranked) == 0 {
		t.Fatal("a near-miss owner/name produced no candidates")
	}
	var sawCLI bool
	for _, m := range Candidates(ranked, 4) {
		if m.Repo == "JeremyProffittOrg/ghost-cli" {
			sawCLI = true
		}
	}
	if !sawCLI {
		t.Errorf("ghost-cli is not among the candidates for %q: %v", "JeremyProffittOrg/ghost", ranked)
	}

	// And a near-miss on the name half still resolves confidently when only one
	// repo can fit.
	got, ok := Best(fleet, "JeremyProffittOrg/live-ninj")
	if !ok || got.Repo != "JeremyProffittOrg/live-ninja" {
		t.Errorf("Best = %+v/%v, want live-ninja", got, ok)
	}
}

func TestBestReturnsNothingForAnUnknownName(t *testing.T) {
	for _, q := range []string{"", "   ", "quantum teleporter", "!!!"} {
		if repo, ok := Best(fleet, q); ok {
			t.Errorf("Best(%q) returned %q, want no match", q, repo.Repo)
		}
	}
}

// An exact full name always outranks a merely-containing one, whatever the
// recency bonus does.
func TestExactNameBeatsSubstring(t *testing.T) {
	repos := []Repo{
		{Repo: "o/ghost-cli-extras", Owner: "o", Name: "ghost-cli-extras"},
		{Repo: "o/ghost-cli", Owner: "o", Name: "ghost-cli"},
	}
	got, ok := Best(repos, "ghost cli")
	if !ok {
		t.Fatal("no match")
	}
	if got.Repo != "o/ghost-cli" {
		t.Errorf("Best = %q, want the exact o/ghost-cli even though it is listed second", got.Repo)
	}
}

// Within one score tier the more-recently-pushed repo wins, because that is
// overwhelmingly the one being talked about.
func TestRecencyBreaksTiesWithinATier(t *testing.T) {
	repos := []Repo{
		{Repo: "o/alpha-service", Owner: "o", Name: "alpha-service"},
		{Repo: "o/beta-service", Owner: "o", Name: "beta-service"},
	}
	ranked := Rank(repos, "service")
	if len(ranked) != 2 {
		t.Fatalf("ranked %d repos, want 2", len(ranked))
	}
	if ranked[0].Repo.Repo != "o/alpha-service" {
		t.Errorf("top match = %q, want the first-listed o/alpha-service", ranked[0].Repo.Repo)
	}
}

// Find is the launch path and must not fuzz: the fuzzy step already happened
// when the owner picked from the candidate list.
func TestFindIsExact(t *testing.T) {
	if _, ok := Find(fleet, "JeremyProffittOrg/live-ninja"); !ok {
		t.Error("Find missed an exact repo")
	}
	if _, ok := Find(fleet, "jeremyproffittorg/LIVE-NINJA"); !ok {
		t.Error("Find should be case-insensitive on an exact full name")
	}
	for _, q := range []string{"live ninja", "live-ninja", "JeremyProffittOrg/live"} {
		if _, ok := Find(fleet, q); ok {
			t.Errorf("Find(%q) matched; it must require the exact owner/name", q)
		}
	}
}

func TestCandidatesBounds(t *testing.T) {
	ranked := Rank(fleet, "ghost")
	if got := Candidates(ranked, 0); got != nil {
		t.Errorf("Candidates(_, 0) = %v, want nil", got)
	}
	if got := Candidates(ranked, 99); len(got) != len(ranked) {
		t.Errorf("Candidates(_, 99) = %d, want all %d", len(got), len(ranked))
	}
	if got := Candidates(nil, 5); got != nil {
		t.Errorf("Candidates(nil, 5) = %v, want nil", got)
	}
}
