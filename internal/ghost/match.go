package ghost

import (
	"sort"
	"strings"
	"unicode"
)

// Matching a spoken application name to a repository.
//
// The input is a transcript, not a typed string: "live ninja", "liveninja",
// "the live-ninja repo", "ghost CLI" and "ghost see ell eye" all have to land on
// the same place, and the model has already had a pass at cleaning it up. So the
// comparison is done on a NORMALIZED form — lowercase, letters and digits only —
// which collapses the whole space of hyphen/space/case variation that separates
// how a repo is written from how it is said.
//
// The matcher deliberately does not pick a winner when it is unsure. It returns
// ranked candidates and lets the caller hand them back to the model, because
// "did you mean ghost-cli or ghost-agent?" is a better voice interaction than a
// confident launch on the wrong repository.

// Match is one candidate repository with its score.
type Match struct {
	Repo  Repo
	Score int
}

// Score tiers. The gaps are wide so a tier always beats the one below it
// regardless of the position bonus.
const (
	scoreExactFull   = 1000 // "owner/name" said in full
	scoreExactName   = 900  // the name half, exactly
	scorePrefix      = 700  // the name starts with the query
	scoreContains    = 500  // the name contains the query
	scoreQueryHolds  = 400  // the query contains the name (spoken padding: "the X repo")
	scoreTokenSubset = 300  // every query word appears in the name
)

// confidentMargin is how far ahead the top match must be before the caller may
// treat it as settled. Two repos that both score "contains" are exactly the
// ambiguity this is here to catch.
const confidentMargin = 150

// Normalize reduces a repo name or a spoken phrase to comparable form:
// lowercase, letters and digits only. "Live-Ninja", "live ninja" and "LIVE
// NINJA!" all become "liveninja".
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tokens splits a spoken phrase into normalized words, dropping the filler that
// survives transcription ("the live ninja repo" → ["live","ninja"]).
func tokens(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		switch f {
		case "the", "a", "an", "repo", "repository", "project", "app", "application", "my":
			continue
		}
		out = append(out, f)
	}
	return out
}

// Rank scores every repo against a spoken query, best first. Repos that do not
// match at all are omitted. Ties are broken by the repo's position in the input
// slice, which ghost-cli has already sorted most-recently-pushed first — so
// between two equally good textual matches, the one being worked on wins.
func Rank(repos []Repo, query string) []Match {
	nq := Normalize(query)
	if nq == "" {
		return nil
	}
	qTokens := tokens(query)

	// A query may arrive as a full "owner/name" — that is what the model echoes
	// back from a listing, and what it sends when it has ALMOST the right name.
	// Scoring such a query only against the repo's name half finds nothing,
	// because the owner prefix drowns it, so the name half of the query is
	// scored too and the better of the two wins. Without this, a near-miss like
	// "JeremyProffittOrg/ghost" yields no candidates at all — precisely the case
	// where the owner most needs to be offered a choice.
	var nqName string
	if _, name, ok := strings.Cut(query, "/"); ok {
		nqName = Normalize(name)
	}
	var nameTokens []string
	if nqName != "" {
		if _, name, ok := strings.Cut(query, "/"); ok {
			nameTokens = tokens(name)
		}
	}

	matches := make([]Match, 0, len(repos))
	for i, repo := range repos {
		score := scoreRepo(repo, nq, qTokens)
		if nqName != "" {
			if alt := scoreRepo(repo, nqName, nameTokens); alt > score {
				score = alt
			}
		}
		if score == 0 {
			continue
		}
		// Recency bonus: strictly smaller than the smallest gap between tiers, so
		// it only ever orders repos WITHIN a tier.
		if i < 100 {
			score += 100 - i
		}
		matches = append(matches, Match{Repo: repo, Score: score})
	}
	sort.SliceStable(matches, func(a, b int) bool { return matches[a].Score > matches[b].Score })
	return matches
}

func scoreRepo(repo Repo, nq string, qTokens []string) int {
	nFull := Normalize(repo.Repo)
	nName := Normalize(repo.Name)

	switch {
	case nFull == nq:
		return scoreExactFull
	case nName == nq:
		return scoreExactName
	case strings.HasPrefix(nName, nq):
		return scorePrefix
	case strings.Contains(nName, nq):
		return scoreContains
	case nName != "" && strings.Contains(nq, nName):
		// "update the live ninja app" contains "liveninja".
		return scoreQueryHolds
	}

	// Every spoken word appears somewhere in the repo name: catches word-order
	// scrambles and partial names ("ninja live", "cost reporting" → aws-cost-reporting).
	if len(qTokens) > 0 {
		all := true
		for _, t := range qTokens {
			if !strings.Contains(nName, Normalize(t)) {
				all = false
				break
			}
		}
		if all {
			return scoreTokenSubset
		}
	}
	return 0
}

// Best returns the single unambiguous match, if there is one. ok is false when
// nothing matched OR when the runner-up is close enough that the caller should
// ask instead of guessing — launching a coding agent on the wrong repository is
// not a cheap mistake to undo.
func Best(repos []Repo, query string) (Repo, bool) {
	ranked := Rank(repos, query)
	if len(ranked) == 0 {
		return Repo{}, false
	}
	if len(ranked) > 1 && ranked[0].Score-ranked[1].Score < confidentMargin {
		return Repo{}, false
	}
	return ranked[0].Repo, true
}

// Find resolves an exact "owner/name" the model echoed back from a previous
// listing. It is deliberately strict — no fuzz — because this is the path a
// launch actually takes, and the fuzzy step already happened when the owner
// picked from the candidates.
func Find(repos []Repo, full string) (Repo, bool) {
	want := strings.TrimSpace(full)
	for _, r := range repos {
		if strings.EqualFold(r.Repo, want) {
			return r, true
		}
	}
	return Repo{}, false
}

// Candidates trims a ranked list to the top n for handing back to the model.
func Candidates(ranked []Match, n int) []Repo {
	if n <= 0 || len(ranked) == 0 {
		return nil
	}
	if n > len(ranked) {
		n = len(ranked)
	}
	out := make([]Repo, 0, n)
	for _, m := range ranked[:n] {
		out = append(out, m.Repo)
	}
	return out
}
