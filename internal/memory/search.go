package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// DefaultTopK is how many entities memory_search returns when the caller
// doesn't say otherwise.
const DefaultTopK = 8

// MaxTopK bounds the result size regardless of caller input.
const MaxTopK = 25

// SearchResult pairs a recalled entity with its cosine similarity score.
type SearchResult struct {
	Entity store.Entity `json:"entity"`
	Score  float64      `json:"score"`
}

// Search embeds the query, ranks it against every vector in the user's
// EMB partition (single-partition Query, hard-capped at
// store.MaxEmbeddingsPerUser — see the DynamoDB-fallback note in
// internal/store/entities.go), and returns the topK entities by cosine
// similarity. Vectors whose ENT item has vanished (a forget that raced
// this search) are skipped. topK <= 0 means DefaultTopK.
func (m *Service) Search(ctx context.Context, userID, query string, topK int) ([]SearchResult, error) {
	if userID == "" {
		return nil, errors.New("memory: userID is required")
	}
	if query == "" {
		return nil, errors.New("memory: query is required")
	}
	if topK <= 0 {
		topK = DefaultTopK
	}
	if topK > MaxTopK {
		topK = MaxTopK
	}

	qvec, err := m.Embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("memory: embed query: %w", err)
	}

	embs, err := m.Store.ListEmbeddings(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(embs) == 0 {
		return []SearchResult{}, nil
	}

	type scored struct {
		emb   store.Embedding
		score float64
	}
	ranked := make([]scored, 0, len(embs))
	for _, e := range embs {
		ranked = append(ranked, scored{emb: e, score: Cosine(qvec, e.Vector)})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	results := make([]SearchResult, 0, topK)
	for _, r := range ranked {
		if len(results) >= topK {
			break
		}
		// The EMB item carries entityType, so this is a single GetItem key
		// lookup back to ENT#<type>#<id> — no partition re-read, no Scan.
		ent, err := m.Store.GetEntity(ctx, userID, r.emb.EntityType, r.emb.EntityID)
		if err != nil {
			return nil, err
		}
		if ent == nil {
			continue // forgotten mid-flight; its EMB will be cleaned up by forget
		}
		results = append(results, SearchResult{Entity: *ent, Score: r.score})
	}
	return results, nil
}
