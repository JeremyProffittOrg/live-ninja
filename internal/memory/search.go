package memory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"unicode"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// DefaultTopK is how many entities memory_search returns when the caller
// doesn't say otherwise.
const DefaultTopK = 8

// MaxTopK bounds the result size regardless of caller input.
const MaxTopK = 25

// SearchResult pairs a recalled entity with a match score. 1 means the
// name contains the query or one of its words. A lower score means the
// match was only in the attributes.
type SearchResult struct {
	Entity store.Entity `json:"entity"`
	Score  float64      `json:"score"`
}

// Search lists the user's ENT# items (one prefix Query) and returns those
// whose name or attributes contain the query, or a word from it. topK <= 0
// means DefaultTopK.
func (m *Service) Search(ctx context.Context, userID, query string, topK int) ([]SearchResult, error) {
	if userID == "" {
		return nil, errors.New("memory: userID is required")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("memory: query is required")
	}
	if topK <= 0 {
		topK = DefaultTopK
	}
	if topK > MaxTopK {
		topK = MaxTopK
	}

	ents, err := m.Store.ListEntities(ctx, userID, "")
	if err != nil {
		return nil, err
	}
	results := make([]SearchResult, 0, topK)
	for _, e := range ents {
		score, ok := matchEntity(e, query)
		if !ok {
			continue
		}
		results = append(results, SearchResult{Entity: e, Score: score})
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Entity.Name < results[j].Entity.Name
	})
	if len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}

// matchEntity reports whether query text appears in the entity. A whole
// query inside the name scores 1. A word of four or more letters inside
// the name also scores 1. A match only in the attributes scores 0.6.
func matchEntity(e store.Entity, query string) (float64, bool) {
	name := strings.ToLower(e.Name)
	blob := name + "\n" + strings.ToLower(entityText(e))
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return 0, false
	}
	if strings.Contains(name, q) {
		return 1, true
	}
	if strings.Contains(blob, q) {
		return 0.6, true
	}
	best := 0.0
	for _, tok := range queryWords(q) {
		if len(tok) < 4 {
			continue
		}
		if strings.Contains(name, tok) {
			best = 1
		} else if best < 0.6 && strings.Contains(blob, tok) {
			best = 0.6
		}
	}
	return best, best > 0
}

func entityText(e store.Entity) string {
	var b strings.Builder
	keys := make([]string, 0, len(e.Attrs))
	for k := range e.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(" ")
		b.WriteString(flattenAttr(e.Attrs[k]))
		b.WriteString("\n")
	}
	for _, r := range e.Relations {
		b.WriteString(r.Type)
		b.WriteString(" ")
		b.WriteString(r.TargetID)
		b.WriteString("\n")
	}
	return b.String()
}

func flattenAttr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []string:
		return strings.Join(t, " ")
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, flattenAttr(item))
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func queryWords(q string) []string {
	return strings.FieldsFunc(q, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
