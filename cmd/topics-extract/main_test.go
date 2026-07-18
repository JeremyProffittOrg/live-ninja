package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// fakeBroker records extract-topics invocations and plays back a canned
// broker response.
type fakeBroker struct {
	t        *testing.T
	response brokerResponse
	requests []brokerRequest
	err      error
}

func (f *fakeBroker) Invoke(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	var req brokerRequest
	require.NoError(f.t, json.Unmarshal(params.Payload, &req))
	f.requests = append(f.requests, req)
	payload, err := json.Marshal(f.response)
	require.NoError(f.t, err)
	assert.Equal(f.t, "live-ninja-realtime-broker", aws.ToString(params.FunctionName))
	return &lambda.InvokeOutput{Payload: payload}, nil
}

func newTestHandler(t *testing.T, resp brokerResponse) (*handler, *store.Store, *fakeBroker) {
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja")
	broker := &fakeBroker{t: t, response: resp}
	h := &handler{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:    st,
		lambda:   broker,
		brokerFn: defaultBrokerFn,
	}
	return h, st, broker
}

func seedTurns(t *testing.T, st *store.Store, uid, sessionID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, st.ConditionalPut(ctx, "USER#"+uid, "LOG#"+sessionID+"#000000",
		map[string]any{"role": "system", "text": "session-start"}, 0))
	require.NoError(t, st.ConditionalPut(ctx, "USER#"+uid, "LOG#"+sessionID+"#000001",
		map[string]any{"role": "user", "text": "What should I cook tonight?", "engine": "openai-realtime", "surface": "web"}, 0))
	require.NoError(t, st.ConditionalPut(ctx, "USER#"+uid, "LOG#"+sessionID+"#000002",
		map[string]any{"role": "assistant", "text": "How about pasta?", "engine": "openai-realtime", "surface": "web"}, 0))
}

func TestHandleTagsConversation(t *testing.T) {
	ctx := context.Background()
	h, st, broker := newTestHandler(t, brokerResponse{
		TopicIDs:  []string{"cook"},
		NewTopics: []string{"Meal Planning"},
	})
	uid, sid, ts := "u1", "sessA", "2026-07-17T12:00:00Z"

	require.NoError(t, st.CreateTopic(ctx, uid, &store.Topic{TopicID: "cook", Name: "Cooking"}))
	require.NoError(t, st.CreateTopic(ctx, uid, &store.Topic{TopicID: "old", Name: "Archived", Archived: true}))
	seedTurns(t, st, uid, sid)

	require.NoError(t, h.Handle(ctx, Event{
		UserID: uid, SessionID: sid, TS: ts, DeviceID: "devA", Surface: "web",
	}))

	// Broker saw the right mode, transcript and only the ACTIVE taxonomy.
	require.Len(t, broker.requests, 1)
	req := broker.requests[0]
	assert.Equal(t, "extract-topics", req.Mode)
	assert.Equal(t, uid, req.UserID)
	assert.Equal(t, "web", req.Surface)
	var p struct {
		Transcript     string `json:"transcript"`
		ExistingTopics []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"existingTopics"`
	}
	require.NoError(t, json.Unmarshal(req.Payload, &p))
	assert.Contains(t, p.Transcript, "user: What should I cook tonight?")
	assert.NotContains(t, p.Transcript, "session-start", "system marker must not reach the model")
	require.Len(t, p.ExistingTopics, 1, "archived topics excluded")
	assert.Equal(t, "cook", p.ExistingTopics[0].ID)

	// Canonical CONV record.
	conv, err := st.GetConversation(ctx, uid, ts+"#"+sid)
	require.NoError(t, err)
	require.NotNil(t, conv)
	assert.Equal(t, "devA", conv.DeviceID)
	assert.Equal(t, "openai-realtime", conv.Engine)
	assert.Equal(t, "What should I cook tonight?", conv.Title)
	assert.Equal(t, 2, conv.TurnCount)
	require.Len(t, conv.TopicIDs, 2)
	assert.Equal(t, "cook", conv.TopicIDs[0])
	newID := conv.TopicIDs[1]

	// The proposed topic exists with a deterministic palette color and a
	// bumped counter; the existing topic counted too.
	created, err := st.GetTopic(ctx, uid, newID)
	require.NoError(t, err)
	require.NotNil(t, created)
	assert.Equal(t, "Meal Planning", created.Name)
	assert.Equal(t, topicColor("Meal Planning"), created.Color)
	assert.Equal(t, 1, created.ConvCount)
	cook, err := st.GetTopic(ctx, uid, "cook")
	require.NoError(t, err)
	assert.Equal(t, 1, cook.ConvCount)

	// Filterable immediately (FR-TOP-04).
	byTopic, _, err := st.ListConversations(ctx, uid, store.ListConversationsOpts{TopicID: "cook"})
	require.NoError(t, err)
	require.Len(t, byTopic, 1)
	assert.Equal(t, sid, byTopic[0].SessionID)
}

func TestHandleIsIdempotentOnRetry(t *testing.T) {
	ctx := context.Background()
	h, st, broker := newTestHandler(t, brokerResponse{TopicIDs: []string{"cook"}})
	uid, sid, ts := "u1", "sessA", "2026-07-17T12:00:00Z"

	require.NoError(t, st.CreateTopic(ctx, uid, &store.Topic{TopicID: "cook", Name: "Cooking"}))
	seedTurns(t, st, uid, sid)

	ev := Event{UserID: uid, SessionID: sid, TS: ts, Surface: "web"}
	require.NoError(t, h.Handle(ctx, ev))
	require.NoError(t, h.Handle(ctx, ev)) // async-retry redelivery

	assert.Len(t, broker.requests, 1, "second delivery must not re-invoke the model")
	topic, err := st.GetTopic(ctx, uid, "cook")
	require.NoError(t, err)
	assert.Equal(t, 1, topic.ConvCount, "no double count on retry")

	convs, _, err := st.ListConversations(ctx, uid, store.ListConversationsOpts{})
	require.NoError(t, err)
	assert.Len(t, convs, 1)
}

func TestHandleSecondFinalFlushConvergesOnOneConversation(t *testing.T) {
	// The duplicate-CONV bug: one session's client sent {final:true} twice
	// a few seconds apart (End button, then pagehide), producing two invokes
	// with DIFFERENT timestamps. The session claim must map the second onto
	// the first's canonical timestamp → exactly one CONV row, one broker
	// call, no double topic counting.
	ctx := context.Background()
	h, st, broker := newTestHandler(t, brokerResponse{TopicIDs: []string{"cook"}})
	uid, sid := "u1", "sessA"
	tsFirst, tsSecond := "2026-07-17T12:00:00Z", "2026-07-17T12:00:03Z"

	require.NoError(t, st.CreateTopic(ctx, uid, &store.Topic{TopicID: "cook", Name: "Cooking"}))
	seedTurns(t, st, uid, sid)

	require.NoError(t, h.Handle(ctx, Event{UserID: uid, SessionID: sid, TS: tsFirst, Surface: "web"}))
	require.NoError(t, h.Handle(ctx, Event{UserID: uid, SessionID: sid, TS: tsSecond, Surface: "web"}))

	assert.Len(t, broker.requests, 1, "second final flush must not re-invoke the model")
	convs, _, err := st.ListConversations(ctx, uid, store.ListConversationsOpts{})
	require.NoError(t, err)
	require.Len(t, convs, 1, "one session must never mint two CONV rows")
	assert.Equal(t, tsFirst, convs[0].TS, "the first final flush's timestamp is canonical")
	topic, err := st.GetTopic(ctx, uid, "cook")
	require.NoError(t, err)
	assert.Equal(t, 1, topic.ConvCount)
}

func TestHandleConcurrentClaimDefersToRetry(t *testing.T) {
	// A different event claimed the session moments ago and its CONV isn't
	// recorded yet → this attempt must NOT run a parallel extraction (it
	// would double-create proposed topics); it errors so the async-invoke
	// retry re-checks after the first attempt lands.
	ctx := context.Background()
	h, st, broker := newTestHandler(t, brokerResponse{TopicIDs: []string{"cook"}})
	uid, sid := "u1", "sessA"
	seedTurns(t, st, uid, sid)

	claim, err := st.ClaimConversationSession(ctx, uid, sid, "2026-07-17T12:00:00Z")
	require.NoError(t, err)
	require.False(t, claim.Existing)

	err = h.Handle(ctx, Event{UserID: uid, SessionID: sid, TS: "2026-07-17T12:00:03Z", Surface: "web"})
	require.Error(t, err, "young foreign claim without a CONV row means in-flight")
	assert.Empty(t, broker.requests, "no parallel extraction")
}

func TestHandleSkipsEmptySessions(t *testing.T) {
	ctx := context.Background()
	h, st, broker := newTestHandler(t, brokerResponse{})
	uid, sid := "u1", "sessEmpty"

	// Only the broker's system marker exists — nothing was said.
	require.NoError(t, st.ConditionalPut(ctx, "USER#"+uid, "LOG#"+sid+"#000000",
		map[string]any{"role": "system", "text": "session-start"}, 0))

	require.NoError(t, h.Handle(ctx, Event{UserID: uid, SessionID: sid, TS: "2026-07-17T12:00:00Z"}))
	assert.Empty(t, broker.requests)
	convs, _, err := st.ListConversations(ctx, uid, store.ListConversationsOpts{})
	require.NoError(t, err)
	assert.Empty(t, convs)
}

func TestHandleBrokerErrorClassification(t *testing.T) {
	ctx := context.Background()
	uid, sid, ts := "u1", "sessA", "2026-07-17T12:00:00Z"

	// 5xx-class broker error → returned (async retry).
	h, st, _ := newTestHandler(t, brokerResponse{Error: "extract_failed", Code: 502, Message: "down"})
	seedTurns(t, st, uid, sid)
	require.Error(t, h.Handle(ctx, Event{UserID: uid, SessionID: sid, TS: ts}))

	// 4xx broker error → permanent, swallowed (no pointless retries).
	h, st, _ = newTestHandler(t, brokerResponse{Error: "invalid_request", Code: 400, Message: "bad"})
	seedTurns(t, st, uid, sid)
	require.NoError(t, h.Handle(ctx, Event{UserID: uid, SessionID: sid, TS: ts}))
	convs, _, err := st.ListConversations(ctx, uid, store.ListConversationsOpts{})
	require.NoError(t, err)
	assert.Empty(t, convs, "dropped extraction records nothing")
}

func TestHandleValidation(t *testing.T) {
	h, _, _ := newTestHandler(t, brokerResponse{})
	require.Error(t, h.Handle(context.Background(), Event{UserID: "", SessionID: "s"}))
	require.Error(t, h.Handle(context.Background(), Event{UserID: "u", SessionID: ""}))
}

func TestHelperFunctions(t *testing.T) {
	turns := []store.Turn{
		{Role: "assistant", Text: "Hi", Surface: "android", Engine: ""},
		{Role: "user", Text: "Plan my day", Engine: "nova-sonic"},
	}
	assert.Equal(t, "android", resolveSurface(Event{Surface: "bogus"}, turns))
	assert.Equal(t, "device", resolveSurface(Event{Surface: "device"}, turns))
	assert.Equal(t, "web", resolveSurface(Event{}, nil))
	assert.Equal(t, "nova-sonic", firstEngine(turns))
	assert.Equal(t, "Plan my day", conversationTitle(turns))
	assert.Equal(t, "", conversationTitle(turns[:1]))
	assert.Equal(t, []string{"a", "b"}, dedupe([]string{"a", "b", "a", ""}))

	// Transcript truncation keeps head and tail.
	long := make([]store.Turn, 0, 400)
	for i := 0; i < 400; i++ {
		long = append(long, store.Turn{Role: "user", Text: string(make([]byte, 0)) + "m" + string(rune('a'+i%26)) + " some words to pad the line out considerably for length"})
	}
	s := buildTranscript(long)
	assert.LessOrEqual(t, len(s), transcriptHeadChars+transcriptTailChars+64)
	assert.Contains(t, s, "[... transcript truncated ...]")

	// Deterministic palette colors.
	assert.Equal(t, topicColor("Cooking"), topicColor("cooking"))
	assert.Contains(t, topicPalette, topicColor("anything"))
}
