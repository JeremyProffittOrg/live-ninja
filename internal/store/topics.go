package store

// Conversation Topics & Filterable History (M11, FR-TOP-01..07).
//
// Locked item shapes (workflow CTX decision — deliberately NO new GSIs,
// the whole access pattern is single-partition Query + key lookups):
//
//	TOPIC: pk=USER#<uid> sk=TOPIC#<topicId>
//	       name, color, archived(bool), mergedInto?, createdAt, convCount(int)
//	CONV:  pk=USER#<uid> sk=CONV#<ts RFC3339>#<sessionId>      (canonical record)
//	       sessionId, ts, deviceId, engine, surface, title, topicIds[], turnCount
//	TREF:  pk=USER#<uid> sk=TREF#<topicId>#<ts RFC3339>#<sessionId>
//	       one per (conversation, topic) assignment — the by-topic index.
//	       Carries a denormalized deviceId (device FilterExpression without
//	       fetching the CONV first) and convSK (the CONV item's sort key).
//
// Filter mapping (FR-TOP-04, all Query, never Scan):
//   - by topic:          Query sk BETWEEN TREF#<topicId>#<from> .. <to>
//   - by date (no topic): Query sk BETWEEN CONV#<from> .. CONV#<to>
//   - by device:          FilterExpression deviceId = :d on either shape
//
// Rename/merge never re-tags: tags reference the stable topicId (FR-TOP-02);
// merge repoints TREF rows + CONV topicIds to the destination id and marks
// the source topic mergedInto=<dst>, archived=true.
//
// Delete (DeleteTopic) removes the TOPIC row and every TREF row under it
// (paginated Query + chunked BatchWriteItem — see batchDeleteKeys in
// store.go) but never touches CONV rows: a deleted topic's id can linger in
// a conversation's topicIds array, filtered out on read rather than
// scrubbed at delete time (see DeleteTopic's doc comment for why that's the
// simpler-and-still-correct choice). A topic name that reappears in future
// conversations gets a brand new topicId — extraction never resurrects a
// deleted one.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ---- key builders ----

const (
	topicSKPrefix = "TOPIC#"
	convSKPrefix  = "CONV#"
	trefSKPrefix  = "TREF#"
	logSKPrefix   = "LOG#"
	// convSessSKPrefix keys the per-session claim marker written by
	// ClaimConversationSession — it pins ONE canonical CONV timestamp per
	// sessionId so every extraction attempt (retries, a second final:true
	// flush seconds later) addresses the same CONV#/TREF# items. Note the
	// prefix does NOT begin with "CONV#" ('S' vs '#' after "CONV"), so
	// markers can never leak into the CONV# list/range queries.
	convSessSKPrefix = "CONVSESS#"
)

func topicSK(topicID string) string { return topicSKPrefix + topicID }

func convSK(ts, sessionID string) string { return convSKPrefix + ts + "#" + sessionID }

func trefSK(topicID, ts, sessionID string) string {
	return trefSKPrefix + topicID + "#" + ts + "#" + sessionID
}

func trefTopicPrefix(topicID string) string { return trefSKPrefix + topicID + "#" }

// skRangeHi is appended to an inclusive upper bound so every sk that
// starts with that bound (the "#<sessionId>" tail) still matches.
const skRangeHi = "￿"

// ---- item types ----

// Topic is one USER#<uid>/TOPIC#<topicId> taxonomy row (FR-TOP-02).
type Topic struct {
	TopicID    string `dynamodbav:"topicId"`
	Name       string `dynamodbav:"name"`
	Color      string `dynamodbav:"color"`
	Archived   bool   `dynamodbav:"archived"`
	MergedInto string `dynamodbav:"mergedInto,omitempty"`
	CreatedAt  string `dynamodbav:"createdAt"` // RFC3339 UTC
	ConvCount  int    `dynamodbav:"convCount"`
}

// Conversation is the canonical USER#<uid>/CONV#<ts>#<sessionId> record
// written once by the post-session extractor (FR-TOP-03).
//
// Cost fields are the client's list-price estimate for the session
// (accumulated from OpenAI Realtime usage events, shipped on the final
// transcript flush — see web/static/js/transcriptsink.mjs). Zero means
// "not reported" (pre-cost conversations, or an engine that surfaces no
// usage), never "free" — the UI renders it as absent.
type Conversation struct {
	SessionID       string   `dynamodbav:"sessionId"`
	TS              string   `dynamodbav:"ts"` // RFC3339 UTC; also embedded in sk
	DeviceID        string   `dynamodbav:"deviceId,omitempty"`
	Engine          string   `dynamodbav:"engine,omitempty"`
	Surface         string   `dynamodbav:"surface,omitempty"`
	Title           string   `dynamodbav:"title,omitempty"`
	TopicIDs        []string `dynamodbav:"topicIds"`
	TurnCount       int      `dynamodbav:"turnCount"`
	CostUSD         float64  `dynamodbav:"costUsd,omitempty"`
	CostTextTokens  int      `dynamodbav:"costTextTokens,omitempty"`
	CostAudioTokens int      `dynamodbav:"costAudioTokens,omitempty"`
}

// ConvID is the stable public identifier for one conversation
// ("<ts>#<sessionId>" — exactly the sk minus its CONV# prefix), used by
// GET /v1/conversations/{id}.
func (c *Conversation) ConvID() string { return c.TS + "#" + c.SessionID }

// SK returns the conversation's full sort key.
func (c *Conversation) SK() string { return convSK(c.TS, c.SessionID) }

// TopicRef is one USER#<uid>/TREF#<topicId>#<ts>#<sessionId> assignment
// row — the by-topic conversation index (one per topic per conversation).
type TopicRef struct {
	TopicID   string `dynamodbav:"topicId"`
	SessionID string `dynamodbav:"sessionId"`
	TS        string `dynamodbav:"ts"`
	DeviceID  string `dynamodbav:"deviceId,omitempty"`
	ConvSK    string `dynamodbav:"convSK"`
}

// Turn is one LOG#<sessionId>#<seq> transcript row as read back by the
// topic extractor (written by the transcript sink, internal/webapp).
// role=tool rows are the tool router's audit lines (internal/tools
// writeAudit): Text carries "tool=<name> outcome=<o> callId=<id>
// args=<json>[ error=<code>]" and Output (when present) a capped JSON
// snippet of the tool's successful output.
type Turn struct {
	SK      string `dynamodbav:"sk"`
	Role    string `dynamodbav:"role"`
	Text    string `dynamodbav:"text"`
	Engine  string `dynamodbav:"engine,omitempty"`
	Surface string `dynamodbav:"surface,omitempty"`
	TS      string `dynamodbav:"ts,omitempty"`
	Output  string `dynamodbav:"output,omitempty"`
}

// ---- topics CRUD ----

// CreateTopic writes a new topic row; conditional so an id collision is
// ErrAlreadyExists, never a silent overwrite. Fills CreatedAt if unset.
func (s *Store) CreateTopic(ctx context.Context, userID string, t *Topic) error {
	switch {
	case t == nil:
		return errors.New("store: topic is required")
	case userID == "" || t.TopicID == "" || strings.TrimSpace(t.Name) == "":
		return errors.New("store: userID, topicID and name are required")
	case strings.Contains(t.TopicID, "#"):
		return errors.New("store: topicID must not contain '#'")
	}
	if t.CreatedAt == "" {
		t.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	av, err := attributevalue.MarshalMap(t)
	if err != nil {
		return fmt.Errorf("store: marshal topic: %w", err)
	}
	av["pk"] = &types.AttributeValueMemberS{Value: userPK(userID)}
	av["sk"] = &types.AttributeValueMemberS{Value: topicSK(t.TopicID)}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                av,
		ConditionExpression: aws.String("attribute_not_exists(pk) AND attribute_not_exists(sk)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("store: create topic: %w", err)
	}
	return nil
}

// GetTopic fetches one topic row. Returns (nil, nil) when absent.
func (s *Store) GetTopic(ctx context.Context, userID, topicID string) (*Topic, error) {
	if userID == "" || topicID == "" {
		return nil, errors.New("store: userID and topicID are required")
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: topicSK(topicID)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get topic: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var t Topic
	if err := attributevalue.UnmarshalMap(out.Item, &t); err != nil {
		return nil, fmt.Errorf("store: unmarshal topic: %w", err)
	}
	return &t, nil
}

// ListTopics returns the user's whole taxonomy (single-partition Query on
// begins_with TOPIC#; taxonomies are user-curated and small). Callers
// filter archived/merged rows as their view requires.
func (s *Store) ListTopics(ctx context.Context, userID string) ([]Topic, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}
	raw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: topicSKPrefix},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list topics: %w", err)
	}
	topics := make([]Topic, 0, len(raw))
	for _, r := range raw {
		var t Topic
		if err := attributevalue.UnmarshalMap(r, &t); err != nil {
			return nil, fmt.Errorf("store: unmarshal topic: %w", err)
		}
		topics = append(topics, t)
	}
	return topics, nil
}

// TopicUpdate carries the PATCHable topic fields (nil pointer = leave
// unchanged). Merging is MergeTopics, not an update field, because it also
// rewrites TREF/CONV rows.
type TopicUpdate struct {
	Name     *string
	Color    *string
	Archived *bool
}

// UpdateTopic applies a partial update to an existing topic (rename /
// recolor / archive — FR-TOP-02's stable-id mutations: no TREF or CONV row
// is touched). ErrNotFound when the topic is absent.
func (s *Store) UpdateTopic(ctx context.Context, userID, topicID string, upd TopicUpdate) error {
	if userID == "" || topicID == "" {
		return errors.New("store: userID and topicID are required")
	}

	sets := make([]string, 0, 3)
	names := map[string]string{}
	values := map[string]types.AttributeValue{}
	if upd.Name != nil {
		if strings.TrimSpace(*upd.Name) == "" {
			return errors.New("store: topic name must not be empty")
		}
		sets = append(sets, "#nm = :nm")
		names["#nm"] = "name"
		values[":nm"] = &types.AttributeValueMemberS{Value: *upd.Name}
	}
	if upd.Color != nil {
		sets = append(sets, "#cl = :cl")
		names["#cl"] = "color"
		values[":cl"] = &types.AttributeValueMemberS{Value: *upd.Color}
	}
	if upd.Archived != nil {
		sets = append(sets, "#ar = :ar")
		names["#ar"] = "archived"
		values[":ar"] = &types.AttributeValueMemberBOOL{Value: *upd.Archived}
	}
	if len(sets) == 0 {
		return errors.New("store: no topic fields to update")
	}

	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: topicSK(topicID)},
		},
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		UpdateExpression:          aws.String("SET " + strings.Join(sets, ", ")),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrNotFound
		}
		return fmt.Errorf("store: update topic: %w", err)
	}
	return nil
}

// IncrementTopicConvCount bumps a topic's conversation counter by delta
// (ADD — atomic). Conditional on the topic existing so a counter bump can
// never resurrect a deleted/never-created topic row.
func (s *Store) IncrementTopicConvCount(ctx context.Context, userID, topicID string, delta int) error {
	if userID == "" || topicID == "" {
		return errors.New("store: userID and topicID are required")
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: topicSK(topicID)},
		},
		ConditionExpression: aws.String("attribute_exists(pk)"),
		UpdateExpression:    aws.String("ADD convCount :d"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":d": &types.AttributeValueMemberN{Value: strconv.Itoa(delta)},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrNotFound
		}
		return fmt.Errorf("store: increment topic convCount: %w", err)
	}
	return nil
}

// DeleteTopic removes a topic and every TREF ref that points at it: a
// paginated Query for the TREF#<topicID># range (queryAllPages already
// follows LastEvaluatedKey to exhaustion) followed by a chunked
// BatchWriteItem delete (batchDeleteKeys, 25 keys per request with
// UnprocessedItems retried) — the ref count is unbounded per topic, unlike
// the small per-user counts the sequential deleteSessions loop handles
// elsewhere in this package. Refs are deleted before the TOPIC# row itself
// so a crash mid-delete never leaves a "deleted" topic whose refs still
// resolve conversations (delete is safe to re-run: an already-gone key is
// a BatchWriteItem/DeleteItem no-op).
//
// Conversations themselves are deliberately left untouched — their
// topicIds arrays may still list the deleted id afterward. Rewriting every
// tagged CONV row (like MergeTopics's repointConversationTopic) would cost
// one UpdateItem per conversation the topic ever tagged, for a taxonomy
// action that's supposed to be cheap and instant; the simpler and equally
// correct alternative is "filtered on read", not "lazily cleaned": callers
// resolve topicIds against the live taxonomy and simply skip any id with
// no matching topic (see web/static/js/history.mjs's canonicalTopic/
// topicBadge, which already drop a topic id no longer in the loaded
// taxonomy rather than rendering a bare id).
//
// ErrNotFound when the topic is absent.
func (s *Store) DeleteTopic(ctx context.Context, userID, topicID string) error {
	if userID == "" || topicID == "" {
		return errors.New("store: userID and topicID are required")
	}

	refsRaw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: trefTopicPrefix(topicID)},
		},
		ProjectionExpression: aws.String("pk, sk"),
	})
	if err != nil {
		return fmt.Errorf("store: query topic refs: %w", err)
	}

	keys := make([]map[string]types.AttributeValue, 0, len(refsRaw))
	for _, r := range refsRaw {
		pk, okPK := r["pk"].(*types.AttributeValueMemberS)
		sk, okSK := r["sk"].(*types.AttributeValueMemberS)
		if !okPK || !okSK {
			return errors.New("store: topic ref row missing pk/sk")
		}
		keys = append(keys, map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: pk.Value},
			"sk": &types.AttributeValueMemberS{Value: sk.Value},
		})
	}
	if err := s.batchDeleteKeys(ctx, keys); err != nil {
		return fmt.Errorf("store: delete topic refs: %w", err)
	}

	_, err = s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: topicSK(topicID)},
		},
		ConditionExpression: aws.String("attribute_exists(pk)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrNotFound
		}
		return fmt.Errorf("store: delete topic: %w", err)
	}
	return nil
}

// ---- conversation + tag writes (the post-session extractor's path) ----

// ConversationClaim is ClaimConversationSession's result: the canonical
// timestamp every extraction attempt for a session must key its CONV/TREF
// writes with, plus enough metadata to detect a concurrent attempt.
type ConversationClaim struct {
	TS        string    // canonical conversation timestamp (RFC3339 UTC)
	ClaimedAt time.Time // when the winning attempt wrote the marker
	Existing  bool      // true when an earlier attempt claimed this session
}

// ClaimConversationSession pins the canonical conversation timestamp for
// one session via a conditional put of USER#<uid>/CONVSESS#<sessionId>.
// The first caller wins and its ts becomes canonical; every later caller —
// an async-retry redelivery, a crash-resume, or a SECOND final:true flush
// carrying a different ts (the duplicate-CONV bug: End button then pagehide
// a few seconds later) — gets the stored ts back so all attempts converge
// on the same CONV#<ts>#<sessionId> row instead of minting siblings.
// Key-addressed GetItem/PutItem only, never a Scan.
func (s *Store) ClaimConversationSession(ctx context.Context, userID, sessionID, ts string) (*ConversationClaim, error) {
	if userID == "" || sessionID == "" || ts == "" {
		return nil, errors.New("store: userID, sessionID and ts are required")
	}
	now := time.Now().UTC()
	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"pk":        &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk":        &types.AttributeValueMemberS{Value: convSessSKPrefix + sessionID},
			"sessionId": &types.AttributeValueMemberS{Value: sessionID},
			"ts":        &types.AttributeValueMemberS{Value: ts},
			"claimedAt": &types.AttributeValueMemberS{Value: now.Format(time.RFC3339Nano)},
		},
		ConditionExpression: aws.String("attribute_not_exists(pk) AND attribute_not_exists(sk)"),
	})
	if err == nil {
		return &ConversationClaim{TS: ts, ClaimedAt: now}, nil
	}
	var condErr *types.ConditionalCheckFailedException
	if !errors.As(err, &condErr) {
		return nil, fmt.Errorf("store: claim conversation session: %w", err)
	}

	// Marker already present — read the canonical claim back.
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: convSessSKPrefix + sessionID},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("store: read conversation session claim: %w", err)
	}
	if out.Item == nil {
		// Vanishingly unlikely (marker deleted between put and read) —
		// proceed with the caller's own ts; the conditional CONV/TREF puts
		// still guarantee no duplicates for identical timestamps.
		return &ConversationClaim{TS: ts, ClaimedAt: now}, nil
	}
	claim := &ConversationClaim{TS: ts, Existing: true}
	if v, ok := out.Item["ts"].(*types.AttributeValueMemberS); ok && v.Value != "" {
		claim.TS = v.Value
	}
	if v, ok := out.Item["claimedAt"].(*types.AttributeValueMemberS); ok {
		if t, perr := time.Parse(time.RFC3339Nano, v.Value); perr == nil {
			claim.ClaimedAt = t
		}
	}
	return claim, nil
}

// CreateConversation writes the canonical CONV record. Conditional put —
// the extractor uses ErrAlreadyExists as its "this session was already
// processed" idempotency signal (async Lambda retries re-deliver events).
func (s *Store) CreateConversation(ctx context.Context, userID string, c *Conversation) error {
	switch {
	case c == nil:
		return errors.New("store: conversation is required")
	case userID == "" || c.SessionID == "" || c.TS == "":
		return errors.New("store: userID, sessionID and ts are required")
	}
	if c.TopicIDs == nil {
		c.TopicIDs = []string{}
	}

	av, err := attributevalue.MarshalMap(c)
	if err != nil {
		return fmt.Errorf("store: marshal conversation: %w", err)
	}
	av["pk"] = &types.AttributeValueMemberS{Value: userPK(userID)}
	av["sk"] = &types.AttributeValueMemberS{Value: c.SK()}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                av,
		ConditionExpression: aws.String("attribute_not_exists(pk) AND attribute_not_exists(sk)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("store: create conversation: %w", err)
	}
	return nil
}

// GetConversation fetches one conversation by its public id
// ("<ts>#<sessionId>", see Conversation.ConvID). Returns (nil, nil) when
// absent — and because pk is always the caller's own partition, an id
// belonging to another user is indistinguishable from absent.
func (s *Store) GetConversation(ctx context.Context, userID, convID string) (*Conversation, error) {
	if userID == "" || convID == "" {
		return nil, errors.New("store: userID and convID are required")
	}
	return s.getConversationBySK(ctx, userID, convSKPrefix+convID)
}

func (s *Store) getConversationBySK(ctx context.Context, userID, sk string) (*Conversation, error) {
	if !strings.HasPrefix(sk, convSKPrefix) {
		return nil, errors.New("store: not a conversation sort key")
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get conversation: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var c Conversation
	if err := attributevalue.UnmarshalMap(out.Item, &c); err != nil {
		return nil, fmt.Errorf("store: unmarshal conversation: %w", err)
	}
	return &c, nil
}

// PutTopicRef writes one TREF assignment row. Conditional — the extractor
// only bumps the topic's convCount when the ref is newly created, so a
// retried event never double-counts.
func (s *Store) PutTopicRef(ctx context.Context, userID string, ref *TopicRef) error {
	switch {
	case ref == nil:
		return errors.New("store: topic ref is required")
	case userID == "" || ref.TopicID == "" || ref.SessionID == "" || ref.TS == "":
		return errors.New("store: userID, topicID, sessionID and ts are required")
	}
	if ref.ConvSK == "" {
		ref.ConvSK = convSK(ref.TS, ref.SessionID)
	}

	av, err := attributevalue.MarshalMap(ref)
	if err != nil {
		return fmt.Errorf("store: marshal topic ref: %w", err)
	}
	av["pk"] = &types.AttributeValueMemberS{Value: userPK(userID)}
	av["sk"] = &types.AttributeValueMemberS{Value: trefSK(ref.TopicID, ref.TS, ref.SessionID)}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                av,
		ConditionExpression: aws.String("attribute_not_exists(pk) AND attribute_not_exists(sk)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("store: put topic ref: %w", err)
	}
	return nil
}

// ListSessionTurns reads back one session's transcript LOG# rows in seq
// order (single-partition Query on begins_with LOG#<sessionId>#). The
// seq-0 "session-start" system marker written by the broker's RecordMint
// is included — callers skip role=system rows.
func (s *Store) ListSessionTurns(ctx context.Context, userID, sessionID string) ([]Turn, error) {
	if userID == "" || sessionID == "" {
		return nil, errors.New("store: userID and sessionID are required")
	}
	raw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: logSKPrefix + sessionID + "#"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list session turns: %w", err)
	}
	turns := make([]Turn, 0, len(raw))
	for _, r := range raw {
		var t Turn
		if err := attributevalue.UnmarshalMap(r, &t); err != nil {
			return nil, fmt.Errorf("store: unmarshal turn: %w", err)
		}
		turns = append(turns, t)
	}
	return turns, nil
}

// ---- history filtering (FR-TOP-04) ----

// ListConversationsOpts are the /v1/conversations filter facets. Zero
// values mean "no filter on that facet". From/To are inclusive RFC3339
// bounds on the conversation timestamp. TurnsOver keeps only
// conversations with MORE than that many turns (the UI's "long
// conversations" facet, strictly greater-than).
type ListConversationsOpts struct {
	TopicID   string
	DeviceID  string
	From      string
	To        string
	TurnsOver int
	Limit     int32
	Cursor    string
}

// ListConversations pages the caller's history newest-first, honoring any
// combination of topic/device/date filters with Query only (never Scan):
//
//   - topic set   → Query the TREF#<topicId># range (date bounds fold into
//     the sk BETWEEN; device is a FilterExpression on the denormalized
//     deviceId), then resolve each ref to its canonical CONV record via
//     GetItem (bounded by the page size).
//   - topic unset → Query the CONV# range directly (date bounds in the sk
//     BETWEEN, device as a FilterExpression).
//
// cursor is the opaque nextCursor from a previous page ("" first page);
// returned nextCursor is "" when exhausted. The cursor must come from a
// query with the same TopicID facet (it encodes a sk in that namespace) —
// changing filters restarts pagination, which is what UIs do anyway.
func (s *Store) ListConversations(ctx context.Context, userID string, opts ListConversationsOpts) ([]Conversation, string, error) {
	if userID == "" {
		return nil, "", errors.New("store: userID is required")
	}
	if opts.Limit < 1 {
		opts.Limit = 25
	}

	prefix := convSKPrefix
	if opts.TopicID != "" {
		prefix = trefTopicPrefix(opts.TopicID)
	}
	lo := prefix + opts.From
	hi := prefix + opts.To + skRangeHi // To=="" → whole prefix range

	in := &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND sk BETWEEN :lo AND :hi"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			":lo": &types.AttributeValueMemberS{Value: lo},
			":hi": &types.AttributeValueMemberS{Value: hi},
		},
		ScanIndexForward: aws.Bool(false), // newest first
		Limit:            aws.Int32(opts.Limit),
	}
	filters := make([]string, 0, 2)
	if opts.DeviceID != "" {
		filters = append(filters, "deviceId = :dev")
		in.ExpressionAttributeValues[":dev"] = &types.AttributeValueMemberS{Value: opts.DeviceID}
	}
	// turnCount lives only on CONV rows, so the server-side filter applies
	// to the no-topic path; the TREF path filters after resolving each ref
	// to its CONV record below.
	if opts.TurnsOver > 0 && opts.TopicID == "" {
		filters = append(filters, "turnCount > :mt")
		in.ExpressionAttributeValues[":mt"] = &types.AttributeValueMemberN{Value: strconv.Itoa(opts.TurnsOver)}
	}
	if len(filters) > 0 {
		in.FilterExpression = aws.String(strings.Join(filters, " AND "))
	}
	if opts.Cursor != "" {
		sk, err := decodeConversationCursor(opts.Cursor, prefix)
		if err != nil {
			return nil, "", err
		}
		in.ExclusiveStartKey = map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: sk},
		}
	}

	out, err := s.client.Query(ctx, in)
	if err != nil {
		return nil, "", fmt.Errorf("store: list conversations: %w", err)
	}

	convs := make([]Conversation, 0, len(out.Items))
	for _, r := range out.Items {
		if opts.TopicID != "" {
			var ref TopicRef
			if err := attributevalue.UnmarshalMap(r, &ref); err != nil {
				return nil, "", fmt.Errorf("store: unmarshal topic ref: %w", err)
			}
			conv, err := s.getConversationBySK(ctx, userID, ref.ConvSK)
			if err != nil {
				return nil, "", err
			}
			if conv == nil {
				continue // CONV deleted after the ref was written — skip
			}
			if opts.TurnsOver > 0 && conv.TurnCount <= opts.TurnsOver {
				continue // TREF rows carry no turnCount — filter post-resolve
			}
			convs = append(convs, *conv)
			continue
		}
		var c Conversation
		if err := attributevalue.UnmarshalMap(r, &c); err != nil {
			return nil, "", fmt.Errorf("store: unmarshal conversation: %w", err)
		}
		convs = append(convs, c)
	}

	next := ""
	if lek := out.LastEvaluatedKey; len(lek) > 0 {
		if skAV, ok := lek["sk"].(*types.AttributeValueMemberS); ok {
			next = encodeConversationCursor(skAV.Value)
		}
	}
	return convs, next, nil
}

// ConversationCostSummary is SumConversationCosts's aggregate: the summed
// client-estimated cost across a CONV# time range, plus how many
// conversations the range held and how many of them actually carried a
// cost figure (older rows predate cost persistence).
type ConversationCostSummary struct {
	TotalUSD      float64
	Conversations int
	Costed        int
}

// SumConversationCosts totals the persisted per-session cost estimates
// over the caller's CONV#<from>..<to> range (inclusive RFC3339 bounds,
// ""=open). Single-partition Query with a projection — never a Scan; the
// range is a per-user month of conversations, comfortably bounded.
func (s *Store) SumConversationCosts(ctx context.Context, userID, from, to string) (*ConversationCostSummary, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}
	raw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND sk BETWEEN :lo AND :hi"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			":lo": &types.AttributeValueMemberS{Value: convSKPrefix + from},
			":hi": &types.AttributeValueMemberS{Value: convSKPrefix + to + skRangeHi},
		},
		ProjectionExpression: aws.String("costUsd"),
	})
	if err != nil {
		return nil, fmt.Errorf("store: sum conversation costs: %w", err)
	}
	sum := &ConversationCostSummary{Conversations: len(raw)}
	for _, r := range raw {
		n, ok := r["costUsd"].(*types.AttributeValueMemberN)
		if !ok {
			continue
		}
		usd, perr := strconv.ParseFloat(n.Value, 64)
		if perr != nil || usd <= 0 {
			continue
		}
		sum.TotalUSD += usd
		sum.Costed++
	}
	return sum, nil
}

// ---- merge (FR-TOP-02: stable tags, mergedInto alias) ----

// MergeTopics folds srcID into dstID: every TREF under the source topic is
// rewritten under the destination (delete+put, idempotent per row), each
// affected CONV's topicIds list is repointed src→dst (deduped), the
// destination's convCount grows by the number of newly-created refs, and
// the source topic is marked mergedInto=dst + archived with convCount 0.
// Conversations themselves (their sk, their identity) are never touched —
// tags stay stable. Writes are sequential per row (per-user taxonomies and
// per-topic ref counts are bounded; no BatchWriteItem unprocessed-item
// machinery needed), and a mid-merge crash is safe to re-run: already-moved
// rows are conditional-put no-ops.
func (s *Store) MergeTopics(ctx context.Context, userID, srcID, dstID string) error {
	if userID == "" || srcID == "" || dstID == "" {
		return errors.New("store: userID, srcID and dstID are required")
	}
	if srcID == dstID {
		return errors.New("store: cannot merge a topic into itself")
	}
	src, err := s.GetTopic(ctx, userID, srcID)
	if err != nil {
		return err
	}
	dst, err := s.GetTopic(ctx, userID, dstID)
	if err != nil {
		return err
	}
	if src == nil || dst == nil {
		return ErrNotFound
	}

	refsRaw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: trefTopicPrefix(srcID)},
		},
	})
	if err != nil {
		return fmt.Errorf("store: query source topic refs: %w", err)
	}

	moved := 0
	for _, raw := range refsRaw {
		var ref TopicRef
		if err := attributevalue.UnmarshalMap(raw, &ref); err != nil {
			return fmt.Errorf("store: unmarshal topic ref: %w", err)
		}

		newRef := ref
		newRef.TopicID = dstID
		switch err := s.PutTopicRef(ctx, userID, &newRef); {
		case err == nil:
			moved++
		case errors.Is(err, ErrAlreadyExists):
			// Conversation already carried the destination topic too (or a
			// previous merge attempt got this far) — no double count.
		default:
			return err
		}

		if err := s.repointConversationTopic(ctx, userID, ref.ConvSK, srcID, dstID); err != nil {
			return err
		}

		if _, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(s.table),
			Key: map[string]types.AttributeValue{
				"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
				"sk": &types.AttributeValueMemberS{Value: trefSK(srcID, ref.TS, ref.SessionID)},
			},
		}); err != nil {
			return fmt.Errorf("store: delete source topic ref: %w", err)
		}
	}

	if moved > 0 {
		if err := s.IncrementTopicConvCount(ctx, userID, dstID, moved); err != nil {
			return err
		}
	}

	// Source becomes an archived alias: mergedInto carries the forwarding
	// pointer (FR-TOP-02), convCount zeroes out because its refs moved.
	_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: topicSK(srcID)},
		},
		ConditionExpression: aws.String("attribute_exists(pk)"),
		UpdateExpression:    aws.String("SET mergedInto = :dst, archived = :t, convCount = :z"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":dst": &types.AttributeValueMemberS{Value: dstID},
			":t":   &types.AttributeValueMemberBOOL{Value: true},
			":z":   &types.AttributeValueMemberN{Value: "0"},
		},
	})
	if err != nil {
		return fmt.Errorf("store: mark topic merged: %w", err)
	}
	return nil
}

// repointConversationTopic rewrites one CONV item's topicIds list,
// replacing srcID with dstID (deduplicated, original order preserved).
func (s *Store) repointConversationTopic(ctx context.Context, userID, convSortKey, srcID, dstID string) error {
	conv, err := s.getConversationBySK(ctx, userID, convSortKey)
	if err != nil {
		return err
	}
	if conv == nil {
		return nil // canonical record gone; nothing to repoint
	}

	rewritten := make([]string, 0, len(conv.TopicIDs))
	seen := make(map[string]bool, len(conv.TopicIDs))
	for _, id := range conv.TopicIDs {
		if id == srcID {
			id = dstID
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		rewritten = append(rewritten, id)
	}

	ids, err := attributevalue.Marshal(rewritten)
	if err != nil {
		return fmt.Errorf("store: marshal topicIds: %w", err)
	}
	_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: convSortKey},
		},
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		UpdateExpression:          aws.String("SET topicIds = :ids"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":ids": ids},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil // deleted between read and write — same as absent
		}
		return fmt.Errorf("store: repoint conversation topic: %w", err)
	}
	return nil
}

// ---- opaque list cursor (same discipline as deliverables.go: the cursor
// is just the last-seen sk, base64url-wrapped; pk always re-derives from
// the authenticated caller so a tampered cursor can never leave the
// caller's own partition, and the prefix check rejects cross-namespace
// cursors outright) ----

func encodeConversationCursor(sk string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(sk))
}

func decodeConversationCursor(cursor, wantPrefix string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", fmt.Errorf("store: invalid cursor: %w", err)
	}
	sk := string(b)
	if !strings.HasPrefix(sk, wantPrefix) {
		return "", errors.New("store: invalid cursor")
	}
	return sk, nil
}
