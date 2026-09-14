package agentmemory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClient records every call and answers from canned data.
type fakeClient struct {
	events      []*bedrockagentcore.CreateEventInput
	failSeqs    map[string]bool // client tokens that must fail
	retrieveIn  []*bedrockagentcore.RetrieveMemoryRecordsInput
	retrieveOut []types.MemoryRecordSummary
	retrieveErr error

	records     map[string]types.MemoryRecord // id -> record (for Get)
	listPages   [][]types.MemoryRecordSummary // consumed in order by ListMemoryRecords
	deleted     []string
	batchDelete [][]types.MemoryRecordDeleteInput

	sessions      []types.SessionSummary
	eventsBySess  map[string][]types.Event
	deletedEvents []string
}

func (f *fakeClient) CreateEvent(_ context.Context, in *bedrockagentcore.CreateEventInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.CreateEventOutput, error) {
	if f.failSeqs[aws.ToString(in.ClientToken)] {
		return nil, errors.New("boom")
	}
	f.events = append(f.events, in)
	return &bedrockagentcore.CreateEventOutput{}, nil
}

func (f *fakeClient) RetrieveMemoryRecords(_ context.Context, in *bedrockagentcore.RetrieveMemoryRecordsInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.RetrieveMemoryRecordsOutput, error) {
	f.retrieveIn = append(f.retrieveIn, in)
	if f.retrieveErr != nil {
		return nil, f.retrieveErr
	}
	return &bedrockagentcore.RetrieveMemoryRecordsOutput{MemoryRecordSummaries: f.retrieveOut}, nil
}

func (f *fakeClient) ListMemoryRecords(_ context.Context, _ *bedrockagentcore.ListMemoryRecordsInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.ListMemoryRecordsOutput, error) {
	if len(f.listPages) == 0 {
		return &bedrockagentcore.ListMemoryRecordsOutput{}, nil
	}
	page := f.listPages[0]
	f.listPages = f.listPages[1:]
	out := &bedrockagentcore.ListMemoryRecordsOutput{MemoryRecordSummaries: page}
	if len(f.listPages) > 0 {
		out.NextToken = aws.String("more")
	}
	return out, nil
}

func (f *fakeClient) GetMemoryRecord(_ context.Context, in *bedrockagentcore.GetMemoryRecordInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.GetMemoryRecordOutput, error) {
	rec, ok := f.records[aws.ToString(in.MemoryRecordId)]
	if !ok {
		return nil, &types.ResourceNotFoundException{Message: aws.String("no such record")}
	}
	return &bedrockagentcore.GetMemoryRecordOutput{MemoryRecord: &rec}, nil
}

func (f *fakeClient) DeleteMemoryRecord(_ context.Context, in *bedrockagentcore.DeleteMemoryRecordInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.DeleteMemoryRecordOutput, error) {
	f.deleted = append(f.deleted, aws.ToString(in.MemoryRecordId))
	return &bedrockagentcore.DeleteMemoryRecordOutput{}, nil
}

func (f *fakeClient) BatchDeleteMemoryRecords(_ context.Context, in *bedrockagentcore.BatchDeleteMemoryRecordsInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.BatchDeleteMemoryRecordsOutput, error) {
	f.batchDelete = append(f.batchDelete, in.Records)
	return &bedrockagentcore.BatchDeleteMemoryRecordsOutput{}, nil
}

func (f *fakeClient) ListSessions(_ context.Context, _ *bedrockagentcore.ListSessionsInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.ListSessionsOutput, error) {
	return &bedrockagentcore.ListSessionsOutput{SessionSummaries: f.sessions}, nil
}

func (f *fakeClient) ListEvents(_ context.Context, in *bedrockagentcore.ListEventsInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.ListEventsOutput, error) {
	return &bedrockagentcore.ListEventsOutput{Events: f.eventsBySess[aws.ToString(in.SessionId)]}, nil
}

func (f *fakeClient) DeleteEvent(_ context.Context, in *bedrockagentcore.DeleteEventInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.DeleteEventOutput, error) {
	f.deletedEvents = append(f.deletedEvents, aws.ToString(in.EventId))
	return &bedrockagentcore.DeleteEventOutput{}, nil
}

func newSvc(t *testing.T, fc *fakeClient) *Service {
	t.Helper()
	s := New(fc, Config{MemoryID: "mem-1", Mode: ModeOwner}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }
	return s
}

func summary(id, ns, text string, score float64) types.MemoryRecordSummary {
	return types.MemoryRecordSummary{
		MemoryRecordId: aws.String(id),
		Namespaces:     []string{ns},
		Content:        &types.MemoryContentMemberText{Value: text},
		Score:          aws.Float64(score),
		CreatedAt:      aws.Time(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)),
	}
}

func TestConfigFromEnvFailsClosed(t *testing.T) {
	t.Setenv("AGENTCORE_MEMORY_ID", "")
	t.Setenv("AGENTCORE_MEMORY_MODE", "all")
	assert.False(t, ConfigFromEnv().Enabled(), "no id means off, whatever the mode says")

	t.Setenv("AGENTCORE_MEMORY_ID", "mem-1")
	t.Setenv("AGENTCORE_MEMORY_MODE", "sometimes")
	assert.False(t, ConfigFromEnv().Enabled(), "an unknown mode is off")

	t.Setenv("AGENTCORE_MEMORY_MODE", "Owner")
	cfg := ConfigFromEnv()
	assert.True(t, cfg.Enabled())
	assert.True(t, cfg.Admits("owner"))
	assert.False(t, cfg.Admits("member"))
	assert.False(t, cfg.Admits(""))

	t.Setenv("AGENTCORE_MEMORY_MODE", "all")
	assert.True(t, ConfigFromEnv().Admits("member"))
}

func TestNilServiceIsInert(t *testing.T) {
	var s *Service
	assert.False(t, s.Admits("owner"))
	n, err := s.RecordExchanges(context.Background(), "u", "s", []Turn{{Seq: 1, Role: "user", Text: "hi"}})
	assert.Zero(t, n)
	assert.NoError(t, err)
	recs, err := s.Preload(context.Background(), "u", 10)
	assert.Nil(t, recs)
	assert.NoError(t, err)
	assert.NoError(t, s.RememberFact(context.Background(), "u", "s", "x"))
	ev, rc, err := s.PurgeActor(context.Background(), "u")
	assert.Zero(t, ev+rc)
	assert.NoError(t, err)
}

func TestPairExchanges(t *testing.T) {
	turns := []Turn{
		{Seq: 3, Role: "assistant", Text: "Sure."},
		{Seq: 2, Role: "user", Text: "Can you help?"},
		{Seq: 1, Role: "tool", Text: "{}"},
		{Seq: 4, Role: "user", Text: "Thanks"},
		{Seq: 5, Role: "user", Text: "   "},
		{Seq: 6, Role: "user", Text: "Also this"},
		{Seq: 7, Role: "assistant", Text: "Noted"},
		{Seq: 8, Role: "assistant", Text: "Anything else?"},
	}
	got := PairExchanges(turns)
	require.Len(t, got, 4)
	assert.Equal(t, []int{2, 3}, seqs(got[0]), "user then assistant pair, sorted by seq")
	assert.Equal(t, []int{4}, seqs(got[1]), "a user turn with no answer stands alone")
	assert.Equal(t, []int{6, 7}, seqs(got[2]))
	assert.Equal(t, []int{8}, seqs(got[3]), "a trailing assistant turn stands alone")
}

func seqs(turns []Turn) []int {
	out := make([]int, 0, len(turns))
	for _, t := range turns {
		out = append(out, t.Seq)
	}
	return out
}

func TestRecordExchangesWritesOneEventPerExchange(t *testing.T) {
	fc := &fakeClient{}
	s := newSvc(t, fc)
	var counted int64
	s.SetCounter(func(_ context.Context, userID string, events, retrievals int64) {
		assert.Equal(t, "user.1", userID)
		counted += events
		assert.Zero(t, retrievals)
	})

	n, err := s.RecordExchanges(context.Background(), "user.1", "sess-A", []Turn{
		{Seq: 1, Role: "user", Text: "My sister is Sarah."},
		{Seq: 2, Role: "assistant", Text: "Got it."},
		{Seq: 3, Role: "user", Text: "Bye"},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.EqualValues(t, 2, counted)
	require.Len(t, fc.events, 2)

	first := fc.events[0]
	assert.Equal(t, "mem-1", aws.ToString(first.MemoryId))
	assert.Equal(t, "user_1", aws.ToString(first.ActorId), "dots are not legal in an actor id")
	assert.Equal(t, "sess-A", aws.ToString(first.SessionId))
	assert.Equal(t, clientToken("sess-A", 1), aws.ToString(first.ClientToken))
	require.Len(t, first.Payload, 2)
	msg := first.Payload[0].(*types.PayloadTypeMemberConversational).Value
	assert.Equal(t, types.RoleUser, msg.Role)
	assert.Equal(t, "My sister is Sarah.", msg.Content.(*types.ContentMemberText).Value)
	assert.Equal(t, types.RoleAssistant, first.Payload[1].(*types.PayloadTypeMemberConversational).Value.Role)
	assert.Len(t, fc.events[1].Payload, 1)
}

func TestRecordExchangesContinuesPastAFailure(t *testing.T) {
	fc := &fakeClient{failSeqs: map[string]bool{clientToken("s", 1): true}}
	s := newSvc(t, fc)
	n, err := s.RecordExchanges(context.Background(), "u", "s", []Turn{
		{Seq: 1, Role: "user", Text: "one"},
		{Seq: 2, Role: "user", Text: "two"},
	})
	require.Error(t, err)
	assert.Equal(t, 1, n, "the second exchange still lands")
	require.Len(t, fc.events, 1)
	assert.Equal(t, clientToken("s", 2), aws.ToString(fc.events[0].ClientToken))
}

func TestRetrieveScopesToTheActorNamespace(t *testing.T) {
	fc := &fakeClient{retrieveOut: []types.MemoryRecordSummary{
		summary("r1", "/users/u1/facts/", "Sister: Sarah, lives in Austin", 0.91),
		{MemoryRecordId: aws.String("r2"), Content: &types.MemoryContentMemberText{Value: "   "}},
	}}
	s := newSvc(t, fc)
	var retrievals int64
	s.SetCounter(func(_ context.Context, _ string, _, r int64) { retrievals += r })

	recs, err := s.Preload(context.Background(), "u1", 10)
	require.NoError(t, err)
	require.Len(t, recs, 1, "an empty record is dropped")
	assert.Equal(t, "r1", recs[0].ID)
	assert.Equal(t, "/users/u1/facts/", recs[0].Namespace)
	assert.InDelta(t, 0.91, recs[0].Score, 1e-9)
	assert.EqualValues(t, 1, retrievals)

	in := fc.retrieveIn[0]
	assert.Nil(t, in.Namespace, "`namespace` is exact-match on the wire; the actor scope must go through namespacePath")
	assert.Equal(t, "/users/u1/", aws.ToString(in.NamespacePath))
	assert.Equal(t, PreloadQuery, aws.ToString(in.SearchCriteria.SearchQuery))
	assert.EqualValues(t, 10, aws.ToInt32(in.SearchCriteria.TopK))

	_, err = s.Retrieve(context.Background(), "u1", "  ", 5)
	assert.Error(t, err, "an empty query is refused before it costs a retrieval")
}

func TestRetrieveErrorIsReturned(t *testing.T) {
	fc := &fakeClient{retrieveErr: errors.New("throttled")}
	s := newSvc(t, fc)
	_, err := s.Preload(context.Background(), "u1", 10)
	assert.Error(t, err)
}

func TestForgetMatchingDeletesOnlyRecordsNamingTheEntity(t *testing.T) {
	fc := &fakeClient{retrieveOut: []types.MemoryRecordSummary{
		summary("r1", "/users/u1/facts/", "The user's sister Sarah lives in Austin", 0.9),
		summary("r2", "/users/u1/facts/", "The user likes hiking", 0.4),
		summary("r3", "/users/u1/preferences/", "Prefers to be called Sarah's brother", 0.3),
	}}
	s := newSvc(t, fc)
	n, err := s.ForgetMatching(context.Background(), "u1", "sarah")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	require.Len(t, fc.batchDelete, 1)
	ids := []string{aws.ToString(fc.batchDelete[0][0].MemoryRecordId), aws.ToString(fc.batchDelete[0][1].MemoryRecordId)}
	assert.ElementsMatch(t, []string{"r1", "r3"}, ids)

	n, err = s.ForgetMatching(context.Background(), "u1", "")
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestDeleteRecordRefusesAnotherActorsRecord(t *testing.T) {
	fc := &fakeClient{records: map[string]types.MemoryRecord{
		"mine":   {MemoryRecordId: aws.String("mine"), Namespaces: []string{"/users/u1/facts/"}},
		"theirs": {MemoryRecordId: aws.String("theirs"), Namespaces: []string{"/users/u2/facts/"}},
	}}
	s := newSvc(t, fc)

	ok, err := s.DeleteRecord(context.Background(), "u1", "theirs")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, fc.deleted)

	ok, err = s.DeleteRecord(context.Background(), "u1", "mine")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, []string{"mine"}, fc.deleted)

	ok, err = s.DeleteRecord(context.Background(), "u1", "missing")
	require.NoError(t, err)
	assert.False(t, ok, "not found is false, not an error")
}

func TestListRecordsPages(t *testing.T) {
	fc := &fakeClient{listPages: [][]types.MemoryRecordSummary{
		{summary("a", "/users/u1/facts/", "A", 0), summary("b", "/users/u1/facts/", "B", 0)},
		{summary("c", "/users/u1/preferences/", "C", 0)},
	}}
	s := newSvc(t, fc)
	recs, err := s.ListRecords(context.Background(), "u1", 10)
	require.NoError(t, err)
	assert.Len(t, recs, 3)
}

func TestPurgeActorDeletesEventsAndRecords(t *testing.T) {
	fc := &fakeClient{
		sessions: []types.SessionSummary{{SessionId: aws.String("s1")}, {SessionId: aws.String("s2")}},
		eventsBySess: map[string][]types.Event{
			"s1": {{EventId: aws.String("e1")}, {EventId: aws.String("e2")}},
			"s2": {{EventId: aws.String("e3")}},
		},
		listPages: [][]types.MemoryRecordSummary{
			{summary("r1", "/users/u1/facts/", "one", 0)},
		},
	}
	s := newSvc(t, fc)
	events, records, err := s.PurgeActor(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, 3, events)
	assert.Equal(t, 1, records)
	assert.ElementsMatch(t, []string{"e1", "e2", "e3"}, fc.deletedEvents)
	require.Len(t, fc.batchDelete, 1)
	assert.Equal(t, "r1", aws.ToString(fc.batchDelete[0][0].MemoryRecordId))
}

func TestDisplayTextUnwrapsPreferenceRecords(t *testing.T) {
	assert.Equal(t, "The user's sister Sarah lives in Austin.", DisplayText("  The user's sister Sarah lives in Austin. "))
	assert.Equal(t, "Prefers temperatures displayed in Celsius",
		DisplayText(`{"context":"The user explicitly stated that they always want temperatures in Celsius.","preference":"Prefers temperatures displayed in Celsius","categories":["units"]}`))
	assert.Equal(t, "Some context", DisplayText(`{"context":"Some context","preference":""}`))
	assert.Equal(t, `{"not":"a preference"}`, DisplayText(`{"not":"a preference"}`))
	assert.Equal(t, "{broken", DisplayText("{broken"))
}

func TestIdentifiersAreSanitised(t *testing.T) {
	assert.Equal(t, "amzn1_account_ABC", ActorID("amzn1.account.ABC"))
	assert.Equal(t, "/users/u-1/", Namespace("u-1"))
	assert.Equal(t, "explicit-20260914", SessionID("", time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)))
	assert.Equal(t, "unknown", ActorID("  "))
}
