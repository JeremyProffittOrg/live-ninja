package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// seedConversation writes a fully-tagged conversation the way
// cmd/topics-extract does: TREF rows (+convCount bumps) then the CONV
// record.
func seedConversation(t *testing.T, st *Store, uid string, c *Conversation) {
	t.Helper()
	ctx := context.Background()
	for _, topicID := range c.TopicIDs {
		require.NoError(t, st.PutTopicRef(ctx, uid, &TopicRef{
			TopicID: topicID, SessionID: c.SessionID, TS: c.TS, DeviceID: c.DeviceID,
		}))
		require.NoError(t, st.IncrementTopicConvCount(ctx, uid, topicID, 1))
	}
	require.NoError(t, st.CreateConversation(ctx, uid, c))
}

func TestTopicCRUD(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	require.NoError(t, st.CreateTopic(ctx, "u1", &Topic{TopicID: "t1", Name: "Cooking", Color: "#e6194b"}))
	require.ErrorIs(t, st.CreateTopic(ctx, "u1", &Topic{TopicID: "t1", Name: "Dup"}), ErrAlreadyExists)
	require.Error(t, st.CreateTopic(ctx, "u1", &Topic{TopicID: "bad#id", Name: "X"}))

	got, err := st.GetTopic(ctx, "u1", "t1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Cooking", got.Name)
	assert.NotEmpty(t, got.CreatedAt)

	// Rename + recolor + archive via partial updates; id stays stable.
	name, color, archived := "Recipes", "#3cb44b", true
	require.NoError(t, st.UpdateTopic(ctx, "u1", "t1", TopicUpdate{Name: &name, Color: &color}))
	require.NoError(t, st.UpdateTopic(ctx, "u1", "t1", TopicUpdate{Archived: &archived}))
	got, err = st.GetTopic(ctx, "u1", "t1")
	require.NoError(t, err)
	assert.Equal(t, "Recipes", got.Name)
	assert.Equal(t, "#3cb44b", got.Color)
	assert.True(t, got.Archived)

	require.ErrorIs(t, st.UpdateTopic(ctx, "u1", "missing", TopicUpdate{Name: &name}), ErrNotFound)
	require.ErrorIs(t, st.IncrementTopicConvCount(ctx, "u1", "missing", 1), ErrNotFound)

	// Absent topic reads as nil, other users' topics invisible.
	none, err := st.GetTopic(ctx, "u1", "nope")
	require.NoError(t, err)
	assert.Nil(t, none)
	other, err := st.GetTopic(ctx, "u2", "t1")
	require.NoError(t, err)
	assert.Nil(t, other)

	topics, err := st.ListTopics(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, topics, 1)
}

func TestClaimConversationSession(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// First claim wins and keeps its own timestamp.
	first, err := st.ClaimConversationSession(ctx, "u1", "sessA", "2026-07-18T22:23:25Z")
	require.NoError(t, err)
	assert.False(t, first.Existing)
	assert.Equal(t, "2026-07-18T22:23:25Z", first.TS)
	assert.False(t, first.ClaimedAt.IsZero())

	// A later claim with a different timestamp (second final flush) gets
	// the canonical first timestamp back.
	second, err := st.ClaimConversationSession(ctx, "u1", "sessA", "2026-07-18T22:23:28Z")
	require.NoError(t, err)
	assert.True(t, second.Existing)
	assert.Equal(t, "2026-07-18T22:23:25Z", second.TS)
	assert.False(t, second.ClaimedAt.IsZero())

	// Sessions and users are independent claims.
	otherSess, err := st.ClaimConversationSession(ctx, "u1", "sessB", "2026-07-18T23:00:00Z")
	require.NoError(t, err)
	assert.False(t, otherSess.Existing)
	otherUser, err := st.ClaimConversationSession(ctx, "u2", "sessA", "2026-07-18T23:00:00Z")
	require.NoError(t, err)
	assert.False(t, otherUser.Existing)

	// The marker must never surface in the CONV# list range.
	convs, _, err := st.ListConversations(ctx, "u1", ListConversationsOpts{})
	require.NoError(t, err)
	assert.Empty(t, convs)

	_, err = st.ClaimConversationSession(ctx, "", "s", "ts")
	require.Error(t, err)
}

func TestConversationWriteAndGet(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	require.NoError(t, st.CreateTopic(ctx, "u1", &Topic{TopicID: "t1", Name: "Travel"}))
	conv := &Conversation{
		SessionID: "s1", TS: "2026-07-17T10:00:00Z", DeviceID: "devA",
		Engine: "openai-realtime", Surface: "web", Title: "Trip planning",
		TopicIDs: []string{"t1"}, TurnCount: 4,
	}
	seedConversation(t, st, "u1", conv)

	// Idempotency signal for the extractor.
	require.ErrorIs(t, st.CreateConversation(ctx, "u1", conv), ErrAlreadyExists)
	require.ErrorIs(t, st.PutTopicRef(ctx, "u1", &TopicRef{TopicID: "t1", SessionID: "s1", TS: conv.TS}), ErrAlreadyExists)

	got, err := st.GetConversation(ctx, "u1", conv.ConvID())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{"t1"}, got.TopicIDs)
	assert.Equal(t, "devA", got.DeviceID)

	// Cross-user id is indistinguishable from absent.
	other, err := st.GetConversation(ctx, "u2", conv.ConvID())
	require.NoError(t, err)
	assert.Nil(t, other)

	topic, err := st.GetTopic(ctx, "u1", "t1")
	require.NoError(t, err)
	assert.Equal(t, 1, topic.ConvCount)
}

func TestListConversationsFilters(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	uid := "u1"

	require.NoError(t, st.CreateTopic(ctx, uid, &Topic{TopicID: "cook", Name: "Cooking"}))
	require.NoError(t, st.CreateTopic(ctx, uid, &Topic{TopicID: "trav", Name: "Travel"}))

	seedConversation(t, st, uid, &Conversation{
		SessionID: "s1", TS: "2026-07-01T09:00:00Z", DeviceID: "devA",
		TopicIDs: []string{"cook"},
	})
	seedConversation(t, st, uid, &Conversation{
		SessionID: "s2", TS: "2026-07-10T09:00:00Z", DeviceID: "devB",
		TopicIDs: []string{"cook", "trav"},
	})
	seedConversation(t, st, uid, &Conversation{
		SessionID: "s3", TS: "2026-07-15T09:00:00Z", DeviceID: "devA",
		TopicIDs: []string{"trav"},
	})

	sessionIDs := func(convs []Conversation) []string {
		out := make([]string, 0, len(convs))
		for _, c := range convs {
			out = append(out, c.SessionID)
		}
		return out
	}

	// No filters: everything, newest first.
	all, next, err := st.ListConversations(ctx, uid, ListConversationsOpts{})
	require.NoError(t, err)
	assert.Empty(t, next)
	assert.Equal(t, []string{"s3", "s2", "s1"}, sessionIDs(all))

	// Topic filter.
	cook, _, err := st.ListConversations(ctx, uid, ListConversationsOpts{TopicID: "cook"})
	require.NoError(t, err)
	assert.Equal(t, []string{"s2", "s1"}, sessionIDs(cook))

	// Device filter.
	devA, _, err := st.ListConversations(ctx, uid, ListConversationsOpts{DeviceID: "devA"})
	require.NoError(t, err)
	assert.Equal(t, []string{"s3", "s1"}, sessionIDs(devA))

	// Date range (inclusive).
	mid, _, err := st.ListConversations(ctx, uid, ListConversationsOpts{
		From: "2026-07-05T00:00:00Z", To: "2026-07-10T09:00:00Z",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"s2"}, sessionIDs(mid))

	// Topic + device + date combined.
	combo, _, err := st.ListConversations(ctx, uid, ListConversationsOpts{
		TopicID: "trav", DeviceID: "devA", From: "2026-07-01T00:00:00Z", To: "2026-07-31T00:00:00Z",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"s3"}, sessionIDs(combo))

	// Topic + date excluding the newer match.
	early, _, err := st.ListConversations(ctx, uid, ListConversationsOpts{
		TopicID: "cook", To: "2026-07-05T00:00:00Z",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"s1"}, sessionIDs(early))

	// Another user sees nothing.
	other, _, err := st.ListConversations(ctx, "u2", ListConversationsOpts{})
	require.NoError(t, err)
	assert.Empty(t, other)
}

func TestListConversationsTurnsOverAndCostSummary(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	uid := "u1"

	require.NoError(t, st.CreateTopic(ctx, uid, &Topic{TopicID: "t1", Name: "Work"}))
	seedConversation(t, st, uid, &Conversation{
		SessionID: "short", TS: "2026-07-01T10:00:00Z", TurnCount: 3,
		TopicIDs: []string{"t1"}, CostUSD: 0.05, CostTextTokens: 10, CostAudioTokens: 20,
	})
	seedConversation(t, st, uid, &Conversation{
		SessionID: "long", TS: "2026-07-02T10:00:00Z", TurnCount: 12,
		TopicIDs: []string{"t1"}, CostUSD: 0.25, CostTextTokens: 100, CostAudioTokens: 200,
	})
	seedConversation(t, st, uid, &Conversation{
		SessionID: "nocost", TS: "2026-07-03T10:00:00Z", TurnCount: 9,
	})

	sessionIDs := func(convs []Conversation) []string {
		out := make([]string, 0, len(convs))
		for _, c := range convs {
			out = append(out, c.SessionID)
		}
		return out
	}

	// Cost fields round-trip on the CONV record.
	got, err := st.GetConversation(ctx, uid, "2026-07-02T10:00:00Z#long")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.InDelta(t, 0.25, got.CostUSD, 1e-9)
	assert.Equal(t, 100, got.CostTextTokens)
	assert.Equal(t, 200, got.CostAudioTokens)

	// TurnsOver is strictly greater-than, on the CONV path (Filter) ...
	long, _, err := st.ListConversations(ctx, uid, ListConversationsOpts{TurnsOver: 8})
	require.NoError(t, err)
	assert.Equal(t, []string{"nocost", "long"}, sessionIDs(long))

	// ... and on the TREF path (post-resolve — refs carry no turnCount).
	longTopic, _, err := st.ListConversations(ctx, uid, ListConversationsOpts{TopicID: "t1", TurnsOver: 8})
	require.NoError(t, err)
	assert.Equal(t, []string{"long"}, sessionIDs(longTopic))

	// Boundary: exactly N turns does NOT match TurnsOver=N.
	none, _, err := st.ListConversations(ctx, uid, ListConversationsOpts{TurnsOver: 12})
	require.NoError(t, err)
	assert.Empty(t, none)

	// Month summary: total across the range, counting only costed rows.
	sum, err := st.SumConversationCosts(ctx, uid, "2026-07-01T00:00:00Z", "")
	require.NoError(t, err)
	assert.InDelta(t, 0.30, sum.TotalUSD, 1e-9)
	assert.Equal(t, 3, sum.Conversations)
	assert.Equal(t, 2, sum.Costed)

	// From bound trims older conversations out of the sum.
	sum, err = st.SumConversationCosts(ctx, uid, "2026-07-02T00:00:00Z", "")
	require.NoError(t, err)
	assert.InDelta(t, 0.25, sum.TotalUSD, 1e-9)
	assert.Equal(t, 2, sum.Conversations)
	assert.Equal(t, 1, sum.Costed)

	// Another user's summary is empty.
	sum, err = st.SumConversationCosts(ctx, "u2", "", "")
	require.NoError(t, err)
	assert.Zero(t, sum.TotalUSD)
	assert.Zero(t, sum.Conversations)
}

func TestListConversationsPagination(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	uid := "u1"

	require.NoError(t, st.CreateTopic(ctx, uid, &Topic{TopicID: "t1", Name: "T"}))
	ids := []string{"a", "b", "c", "d", "e"}
	for i, id := range ids {
		seedConversation(t, st, uid, &Conversation{
			SessionID: id,
			TS:        "2026-07-0" + string(rune('1'+i)) + "T00:00:00Z",
			TopicIDs:  []string{"t1"},
		})
	}

	var got []string
	cursor := ""
	pages := 0
	for {
		convs, next, err := st.ListConversations(ctx, uid, ListConversationsOpts{Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		for _, c := range convs {
			got = append(got, c.SessionID)
		}
		pages++
		if next == "" {
			break
		}
		cursor = next
		require.Less(t, pages, 10, "pagination must terminate")
	}
	assert.Equal(t, []string{"e", "d", "c", "b", "a"}, got)

	// A cursor from the un-filtered (CONV#) namespace is rejected by a
	// topic-filtered (TREF#) query rather than mis-walking the partition.
	_, first, err := st.ListConversations(ctx, uid, ListConversationsOpts{Limit: 2})
	require.NoError(t, err)
	require.NotEmpty(t, first)
	_, _, err = st.ListConversations(ctx, uid, ListConversationsOpts{TopicID: "t1", Cursor: first})
	require.Error(t, err)
}

func TestMergeTopicsKeepsTagsStable(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	uid := "u1"

	require.NoError(t, st.CreateTopic(ctx, uid, &Topic{TopicID: "src", Name: "Recipies"})) // the typo'd dup
	require.NoError(t, st.CreateTopic(ctx, uid, &Topic{TopicID: "dst", Name: "Recipes"}))

	// s1 tagged only src; s2 tagged both (merge must not double-count it).
	seedConversation(t, st, uid, &Conversation{
		SessionID: "s1", TS: "2026-07-01T00:00:00Z", DeviceID: "devA", TopicIDs: []string{"src"},
	})
	seedConversation(t, st, uid, &Conversation{
		SessionID: "s2", TS: "2026-07-02T00:00:00Z", DeviceID: "devB", TopicIDs: []string{"src", "dst"},
	})

	require.NoError(t, st.MergeTopics(ctx, uid, "src", "dst"))

	// Source is an archived alias with the forwarding pointer, count 0.
	src, err := st.GetTopic(ctx, uid, "src")
	require.NoError(t, err)
	assert.True(t, src.Archived)
	assert.Equal(t, "dst", src.MergedInto)
	assert.Equal(t, 0, src.ConvCount)

	// Destination owns every conversation exactly once.
	dst, err := st.GetTopic(ctx, uid, "dst")
	require.NoError(t, err)
	assert.Equal(t, 2, dst.ConvCount)

	byDst, _, err := st.ListConversations(ctx, uid, ListConversationsOpts{TopicID: "dst"})
	require.NoError(t, err)
	require.Len(t, byDst, 2)
	assert.Equal(t, "s2", byDst[0].SessionID)
	assert.Equal(t, "s1", byDst[1].SessionID)

	// Nothing remains under the source topic.
	bySrc, _, err := st.ListConversations(ctx, uid, ListConversationsOpts{TopicID: "src"})
	require.NoError(t, err)
	assert.Empty(t, bySrc)

	// Tags stayed stable: the conversation records themselves kept their
	// identity (same ConvID) and their topicIds repointed src → dst,
	// deduplicated.
	c1, err := st.GetConversation(ctx, uid, "2026-07-01T00:00:00Z#s1")
	require.NoError(t, err)
	require.NotNil(t, c1)
	assert.Equal(t, []string{"dst"}, c1.TopicIDs)
	c2, err := st.GetConversation(ctx, uid, "2026-07-02T00:00:00Z#s2")
	require.NoError(t, err)
	require.NotNil(t, c2)
	assert.Equal(t, []string{"dst"}, c2.TopicIDs)

	// Re-running the merge (crash-retry semantics) is a no-op.
	require.NoError(t, st.MergeTopics(ctx, uid, "src", "dst"))
	dst, err = st.GetTopic(ctx, uid, "dst")
	require.NoError(t, err)
	assert.Equal(t, 2, dst.ConvCount)

	require.ErrorIs(t, st.MergeTopics(ctx, uid, "src", "missing"), ErrNotFound)
	require.Error(t, st.MergeTopics(ctx, uid, "dst", "dst"))
}

func TestDeleteTopic(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	uid := "u1"

	require.NoError(t, st.CreateTopic(ctx, uid, &Topic{TopicID: "gone", Name: "Doomed"}))
	require.NoError(t, st.CreateTopic(ctx, uid, &Topic{TopicID: "keep", Name: "Kept"}))

	// Enough TREF rows to exercise BatchWriteItem's 25-per-request cap
	// across multiple batches (batchDeleteKeys), not just a single call.
	const n = 57
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("s%02d", i)
		ts := fmt.Sprintf("2026-07-01T%02d:%02d:00Z", i/60, i%60)
		seedConversation(t, st, uid, &Conversation{
			SessionID: id, TS: ts, DeviceID: "devA", TopicIDs: []string{"gone"},
		})
	}
	// One conversation also tagged with the surviving topic — untouched by
	// the delete of "gone".
	seedConversation(t, st, uid, &Conversation{
		SessionID: "kept-conv", TS: "2026-07-02T00:00:00Z", DeviceID: "devA", TopicIDs: []string{"keep"},
	})

	got, err := st.GetTopic(ctx, uid, "gone")
	require.NoError(t, err)
	require.Equal(t, n, got.ConvCount)

	require.NoError(t, st.DeleteTopic(ctx, uid, "gone"))

	// TOPIC row is gone.
	deleted, err := st.GetTopic(ctx, uid, "gone")
	require.NoError(t, err)
	assert.Nil(t, deleted)

	// Every TREF row under the deleted topic is gone — filtering by it
	// returns empty, not an error and not stale hits.
	byGone, next, err := st.ListConversations(ctx, uid, ListConversationsOpts{TopicID: "gone", Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, byGone)
	assert.Empty(t, next)

	// The surviving topic and its ref are untouched.
	keep, err := st.GetTopic(ctx, uid, "keep")
	require.NoError(t, err)
	require.NotNil(t, keep)
	assert.Equal(t, 1, keep.ConvCount)
	byKeep, _, err := st.ListConversations(ctx, uid, ListConversationsOpts{TopicID: "keep"})
	require.NoError(t, err)
	require.Len(t, byKeep, 1)
	assert.Equal(t, "kept-conv", byKeep[0].SessionID)

	// The tagged conversations themselves are untouched — they still exist
	// and (per the "filter on read" design) still carry the stale topicId;
	// no CONV row is rewritten by DeleteTopic.
	conv, err := st.GetConversation(ctx, uid, "2026-07-01T00:00:00Z#s00")
	require.NoError(t, err)
	require.NotNil(t, conv)
	assert.Equal(t, []string{"gone"}, conv.TopicIDs)

	// Deleting an absent (or already-deleted) topic is ErrNotFound, and
	// re-running it is safe (refs are already gone — no-op deletes).
	require.ErrorIs(t, st.DeleteTopic(ctx, uid, "gone"), ErrNotFound)
	require.ErrorIs(t, st.DeleteTopic(ctx, uid, "never-existed"), ErrNotFound)
}

func TestListSessionTurnsSkipsNothingButOrders(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// Simulate the transcript sink + the broker's seq-0 system marker.
	require.NoError(t, st.ConditionalPut(ctx, "USER#u1", "LOG#sess1#000000",
		map[string]any{"role": "system", "text": "session-start"}, 0))
	require.NoError(t, st.ConditionalPut(ctx, "USER#u1", "LOG#sess1#000002",
		map[string]any{"role": "assistant", "text": "Hi there", "engine": "openai-realtime", "surface": "web"}, 0))
	require.NoError(t, st.ConditionalPut(ctx, "USER#u1", "LOG#sess1#000001",
		map[string]any{"role": "user", "text": "Hello", "engine": "openai-realtime", "surface": "web"}, 0))
	// A different session must not bleed in.
	require.NoError(t, st.ConditionalPut(ctx, "USER#u1", "LOG#sess2#000001",
		map[string]any{"role": "user", "text": "Other"}, 0))

	turns, err := st.ListSessionTurns(ctx, "u1", "sess1")
	require.NoError(t, err)
	require.Len(t, turns, 3)
	assert.Equal(t, "system", turns[0].Role)
	assert.Equal(t, "Hello", turns[1].Text)
	assert.Equal(t, "Hi there", turns[2].Text)
}

// ---- no-Scan proof ----

// recordingDDB wraps the fake and records every DynamoDB operation name.
// Note the store's ddbAPI interface does not even declare Scan — the
// compiler already forbids a Scan on any store path; this test additionally
// proves the M11 read paths stay on Query + key lookups at runtime.
type recordingDDB struct {
	*testutil.FakeDynamo
	calls []string
}

func (r *recordingDDB) Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	r.calls = append(r.calls, "Query")
	return r.FakeDynamo.Query(ctx, params, optFns...)
}

func (r *recordingDDB) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	r.calls = append(r.calls, "GetItem")
	return r.FakeDynamo.GetItem(ctx, params, optFns...)
}

func TestListConversationsUsesOnlyQueryAndKeyLookups(t *testing.T) {
	ctx := context.Background()
	rec := &recordingDDB{FakeDynamo: testutil.NewFakeDynamo()}
	st := NewWithClient(rec, "live-ninja")
	uid := "u1"

	require.NoError(t, st.CreateTopic(ctx, uid, &Topic{TopicID: "t1", Name: "T"}))
	seedConversation(t, st, uid, &Conversation{
		SessionID: "s1", TS: "2026-07-01T00:00:00Z", DeviceID: "devA", TopicIDs: []string{"t1"},
	})

	rec.calls = nil
	for _, opts := range []ListConversationsOpts{
		{},
		{TopicID: "t1"},
		{DeviceID: "devA"},
		{From: "2026-06-01T00:00:00Z", To: "2026-08-01T00:00:00Z"},
		{TopicID: "t1", DeviceID: "devA", From: "2026-06-01T00:00:00Z", To: "2026-08-01T00:00:00Z"},
	} {
		_, _, err := st.ListConversations(ctx, uid, opts)
		require.NoError(t, err)
	}

	require.NotEmpty(t, rec.calls)
	for _, op := range rec.calls {
		assert.Contains(t, []string{"Query", "GetItem"}, op,
			"M11 history serving path must be Query/GetItem only — saw %s", op)
	}
}
