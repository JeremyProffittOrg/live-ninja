package store

// M10 memory-layer item shapes (locked decisions — see plan.md M10):
//
//	ENTITY: pk=USER#<uid> sk=ENT#<type>#<entityId>
//	        type ∈ {person place info project task plan}
//	        fields: name, attrs(map), relations[{type,targetId}],
//	        updatedAt, embedded(bool)
//	EMB:    pk=USER#<uid> sk=EMB#<entityId>
//	        vector (base64-encoded little-endian float32s), dim, model,
//	        updatedAt (+ additive entityType field so search/forget can
//	        key straight back to the ENT item without probing all six
//	        type prefixes — documented extension of the locked shape)
//	GUIDE:  pk=USER#<uid> sk=GUIDE#<guideId>
//	        title, text, enabled, priority(int), version, updatedAt
//
// Vector-store decision (VERIFIED 2026-07-17): the aws-sdk-go-v2
// `s3vectors` package IS published (v1.9.x), so the SDK itself is not
// the blocker — but S3 Vectors needs a vector bucket + index + IAM that
// are outside this deploy's locked template.yaml delta (no S3 Vectors
// resources are provisioned), so per the locked fallback the embedding
// store is DynamoDB-native: EMB# items in the user's partition,
// brute-force cosine ranking over a single-partition Query capped at
// MaxEmbeddingsPerUser. Query/GetItem only — never a Scan.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Entity type enum (single source of truth — tool arg validation and the
// web/Android type filters must use these exact strings).
const (
	EntityTypePerson  = "person"
	EntityTypePlace   = "place"
	EntityTypeInfo    = "info"
	EntityTypeProject = "project"
	EntityTypeTask    = "task"
	EntityTypePlan    = "plan"
)

// EntityTypes lists every valid entity type, in the order GetEntityByID
// probes them.
var EntityTypes = []string{
	EntityTypePerson, EntityTypePlace, EntityTypeInfo,
	EntityTypeProject, EntityTypeTask, EntityTypePlan,
}

// ErrInvalidEntityType is returned when a caller passes a type outside
// the EntityTypes enum.
var ErrInvalidEntityType = errors.New("store: invalid entity type")

// MaxEmbeddingsPerUser bounds the single-partition brute-force cosine
// design: ListEmbeddings never returns more than this many vectors, so
// memory search cost is capped regardless of how many entities a user
// accumulates (locked decision: ≤2000 embeddings/user).
const MaxEmbeddingsPerUser = 2000

// Relation is one edge in an entity's relation list
// (e.g. {Type:"works_at", TargetID:"<place entityId>"}).
type Relation struct {
	Type     string `dynamodbav:"type" json:"type"`
	TargetID string `dynamodbav:"targetId" json:"targetId"`
}

// Entity is the USER#<uid>/ENT#<type>#<entityId> item.
type Entity struct {
	UserID    string         `dynamodbav:"-" json:"-"`
	Type      string         `dynamodbav:"entityType" json:"type"`
	EntityID  string         `dynamodbav:"entityId" json:"entityId"`
	Name      string         `dynamodbav:"name" json:"name"`
	Attrs     map[string]any `dynamodbav:"attrs" json:"attrs"`
	Relations []Relation     `dynamodbav:"relations" json:"relations"`
	UpdatedAt string         `dynamodbav:"updatedAt" json:"updatedAt"` // RFC3339
	Embedded  bool           `dynamodbav:"embedded" json:"embedded"`
}

// Embedding is the USER#<uid>/EMB#<entityId> item. Vector round-trips
// through a base64(little-endian float32) string attribute on the wire;
// the struct exposes the decoded []float32.
type Embedding struct {
	UserID     string    `dynamodbav:"-"`
	EntityID   string    `dynamodbav:"entityId"`
	EntityType string    `dynamodbav:"entityType"` // additive: keys back to ENT#<type>#<id>
	Vector     []float32 `dynamodbav:"-"`
	Dim        int       `dynamodbav:"dim"`
	Model      string    `dynamodbav:"model"`
	UpdatedAt  string    `dynamodbav:"updatedAt"` // RFC3339
}

// Guide is the USER#<uid>/GUIDE#<guideId> item — a standing instruction
// the broker injects (priority ascending) into every session's persona
// instructions when enabled.
type Guide struct {
	UserID    string `dynamodbav:"-" json:"-"`
	GuideID   string `dynamodbav:"guideId" json:"guideId"`
	Title     string `dynamodbav:"title" json:"title"`
	Text      string `dynamodbav:"text" json:"text"`
	Enabled   bool   `dynamodbav:"enabled" json:"enabled"`
	Priority  int    `dynamodbav:"priority" json:"priority"`
	Version   int    `dynamodbav:"version" json:"version"`
	UpdatedAt string `dynamodbav:"updatedAt" json:"updatedAt"` // RFC3339
}

// SeedGuideID is the fixed id of the default guide seeded on first list
// (plan.md M10 / FR-MEM-09).
const SeedGuideID = "default-ai-emerging"

// seedGuide returns the default "AI is an emerging technology" guide.
func seedGuide() *Guide {
	return &Guide{
		GuideID: SeedGuideID,
		Title:   "AI is an emerging technology",
		Text: "AI is an emerging technology and the state of the art moves quickly. " +
			"When discussing AI capabilities, tools, models, or best practices, prefer " +
			"recent sources (roughly the last 30 days), state the date of any claim or " +
			"source you cite, and acknowledge uncertainty where things may have changed.",
		Enabled:  true,
		Priority: 100,
		Version:  1,
	}
}

// ---- key builders ----

func entSK(entityType, entityID string) string { return "ENT#" + entityType + "#" + entityID }
func embSK(entityID string) string             { return "EMB#" + entityID }
func guideSK(guideID string) string            { return "GUIDE#" + guideID }

// ValidEntityType reports whether t is one of the six entity types.
func ValidEntityType(t string) bool {
	for _, v := range EntityTypes {
		if v == t {
			return true
		}
	}
	return false
}

// ---- vector wire encoding ----

// EncodeVector packs a float32 vector into the base64(little-endian
// IEEE-754) string stored on the EMB item.
func EncodeVector(v []float32) string {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// DecodeVector reverses EncodeVector.
func DecodeVector(s string) ([]float32, error) {
	buf, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("store: decode vector base64: %w", err)
	}
	if len(buf)%4 != 0 {
		return nil, fmt.Errorf("store: vector byte length %d not a multiple of 4", len(buf))
	}
	out := make([]float32, len(buf)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return out, nil
}

// ---- ENTITY CRUD ----

// PutEntity upserts the full ENT item (last-writer-wins; the memory
// tools re-read before mutating). Sets UpdatedAt server-side.
func (s *Store) PutEntity(ctx context.Context, e *Entity) error {
	switch {
	case e == nil:
		return errors.New("store: entity is required")
	case e.UserID == "":
		return errors.New("store: entity userID is required")
	case e.EntityID == "":
		return errors.New("store: entityID is required")
	case e.Name == "":
		return errors.New("store: entity name is required")
	case !ValidEntityType(e.Type):
		return fmt.Errorf("%w: %q", ErrInvalidEntityType, e.Type)
	}
	e.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	av, err := attributevalue.MarshalMap(e)
	if err != nil {
		return fmt.Errorf("store: marshal entity: %w", err)
	}
	av["pk"] = &types.AttributeValueMemberS{Value: userPK(e.UserID)}
	av["sk"] = &types.AttributeValueMemberS{Value: entSK(e.Type, e.EntityID)}

	if _, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	}); err != nil {
		return fmt.Errorf("store: put entity: %w", err)
	}
	return nil
}

// GetEntity fetches one entity by its full typed key. Returns (nil, nil)
// when absent.
func (s *Store) GetEntity(ctx context.Context, userID, entityType, entityID string) (*Entity, error) {
	if userID == "" || entityID == "" {
		return nil, errors.New("store: userID and entityID are required")
	}
	if !ValidEntityType(entityType) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidEntityType, entityType)
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: entSK(entityType, entityID)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get entity: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var e Entity
	if err := attributevalue.UnmarshalMap(out.Item, &e); err != nil {
		return nil, fmt.Errorf("store: unmarshal entity: %w", err)
	}
	e.UserID = userID
	return &e, nil
}

// GetEntityByID resolves an entity when only its id is known (tool args
// `entity_get`/`forget` carry a bare entityId): at most six GetItem key
// lookups, one per type prefix — bounded and Scan-free.
func (s *Store) GetEntityByID(ctx context.Context, userID, entityID string) (*Entity, error) {
	if userID == "" || entityID == "" {
		return nil, errors.New("store: userID and entityID are required")
	}
	for _, t := range EntityTypes {
		e, err := s.GetEntity(ctx, userID, t, entityID)
		if err != nil {
			return nil, err
		}
		if e != nil {
			return e, nil
		}
	}
	return nil, nil
}

// ListEntities returns the user's entities, optionally filtered to one
// type (empty entityType = all types). Single-partition prefix Query,
// paginated to exhaustion — a user's entity partition is bounded by the
// same ≤2000 design cap as embeddings.
func (s *Store) ListEntities(ctx context.Context, userID, entityType string) ([]Entity, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}
	prefix := "ENT#"
	if entityType != "" {
		if !ValidEntityType(entityType) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidEntityType, entityType)
		}
		prefix = entSK(entityType, "")
	}

	items, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: prefix},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list entities: %w", err)
	}

	out := make([]Entity, 0, len(items))
	for _, raw := range items {
		var e Entity
		if err := attributevalue.UnmarshalMap(raw, &e); err != nil {
			return nil, fmt.Errorf("store: unmarshal entity: %w", err)
		}
		e.UserID = userID
		out = append(out, e)
	}
	return out, nil
}

// FindEntityByName returns the user's entity of the given type whose
// name matches exactly (first match in sk order), or nil. Used by
// memory_write upsert semantics — a type-prefix Query, bounded.
func (s *Store) FindEntityByName(ctx context.Context, userID, entityType, name string) (*Entity, error) {
	if name == "" {
		return nil, errors.New("store: name is required")
	}
	list, err := s.ListEntities(ctx, userID, entityType)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, nil
}

// MarkEntityEmbedded flips embedded=true on an existing ENT item
// (called after the EMB write lands). ErrNotFound if the entity is gone
// (e.g. forgotten mid-flight) — the condition prevents resurrecting a
// deleted entity as a key-only ghost item.
func (s *Store) MarkEntityEmbedded(ctx context.Context, userID, entityType, entityID string) error {
	if !ValidEntityType(entityType) {
		return fmt.Errorf("%w: %q", ErrInvalidEntityType, entityType)
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: entSK(entityType, entityID)},
		},
		ConditionExpression: aws.String("attribute_exists(pk)"),
		UpdateExpression:    aws.String("SET #emb = :true"),
		ExpressionAttributeNames: map[string]string{
			"#emb": "embedded",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":true": &types.AttributeValueMemberBOOL{Value: true},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrNotFound
		}
		return fmt.Errorf("store: mark entity embedded: %w", err)
	}
	return nil
}

// DeleteEntity removes the ENT item. Returns ErrNotFound when nothing
// was there (ALL_OLD tells us), so forget can report accurately.
func (s *Store) DeleteEntity(ctx context.Context, userID, entityType, entityID string) error {
	if !ValidEntityType(entityType) {
		return fmt.Errorf("%w: %q", ErrInvalidEntityType, entityType)
	}
	out, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: entSK(entityType, entityID)},
		},
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return fmt.Errorf("store: delete entity: %w", err)
	}
	if out.Attributes == nil {
		return ErrNotFound
	}
	return nil
}

// ---- EMB CRUD ----

// embItem is the wire shape of the EMB item (vector as base64 string).
type embItem struct {
	PK         string `dynamodbav:"pk"`
	SK         string `dynamodbav:"sk"`
	EntityID   string `dynamodbav:"entityId"`
	EntityType string `dynamodbav:"entityType"`
	Vector     string `dynamodbav:"vector"`
	Dim        int    `dynamodbav:"dim"`
	Model      string `dynamodbav:"model"`
	UpdatedAt  string `dynamodbav:"updatedAt"`
}

// PutEmbedding upserts the EMB item for an entity.
func (s *Store) PutEmbedding(ctx context.Context, e *Embedding) error {
	switch {
	case e == nil:
		return errors.New("store: embedding is required")
	case e.UserID == "" || e.EntityID == "":
		return errors.New("store: embedding userID and entityID are required")
	case len(e.Vector) == 0:
		return errors.New("store: embedding vector is required")
	case !ValidEntityType(e.EntityType):
		return fmt.Errorf("%w: %q", ErrInvalidEntityType, e.EntityType)
	}
	e.Dim = len(e.Vector)
	e.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	av, err := attributevalue.MarshalMap(embItem{
		PK:         userPK(e.UserID),
		SK:         embSK(e.EntityID),
		EntityID:   e.EntityID,
		EntityType: e.EntityType,
		Vector:     EncodeVector(e.Vector),
		Dim:        e.Dim,
		Model:      e.Model,
		UpdatedAt:  e.UpdatedAt,
	})
	if err != nil {
		return fmt.Errorf("store: marshal embedding: %w", err)
	}
	if _, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	}); err != nil {
		return fmt.Errorf("store: put embedding: %w", err)
	}
	return nil
}

// ListEmbeddings loads the user's EMB partition — the brute-force cosine
// corpus — as a paginated single-partition prefix Query, hard-capped at
// MaxEmbeddingsPerUser items so a runaway partition cannot blow up
// read cost or memory.
func (s *Store) ListEmbeddings(ctx context.Context, userID string) ([]Embedding, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}

	in := &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: "EMB#"},
		},
	}

	out := make([]Embedding, 0, 64)
	for len(out) < MaxEmbeddingsPerUser {
		remaining := int32(MaxEmbeddingsPerUser - len(out))
		in.Limit = aws.Int32(remaining)
		page, err := s.client.Query(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("store: list embeddings: %w", err)
		}
		for _, raw := range page.Items {
			var it embItem
			if err := attributevalue.UnmarshalMap(raw, &it); err != nil {
				return nil, fmt.Errorf("store: unmarshal embedding: %w", err)
			}
			vec, err := DecodeVector(it.Vector)
			if err != nil {
				return nil, fmt.Errorf("store: embedding %s: %w", it.EntityID, err)
			}
			out = append(out, Embedding{
				UserID:     userID,
				EntityID:   it.EntityID,
				EntityType: it.EntityType,
				Vector:     vec,
				Dim:        it.Dim,
				Model:      it.Model,
				UpdatedAt:  it.UpdatedAt,
			})
			if len(out) >= MaxEmbeddingsPerUser {
				break
			}
		}
		if page.LastEvaluatedKey == nil || len(page.LastEvaluatedKey) == 0 {
			break
		}
		in.ExclusiveStartKey = page.LastEvaluatedKey
	}
	return out, nil
}

// DeleteEmbedding removes the EMB item. Missing item is NOT an error —
// forget must succeed for entities that were never embedded (or whose
// embed failed and left embedded=false).
func (s *Store) DeleteEmbedding(ctx context.Context, userID, entityID string) error {
	if userID == "" || entityID == "" {
		return errors.New("store: userID and entityID are required")
	}
	if _, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: embSK(entityID)},
		},
	}); err != nil {
		return fmt.Errorf("store: delete embedding: %w", err)
	}
	return nil
}

// ---- GUIDE CRUD ----

// GetGuide fetches one guide; (nil, nil) when absent.
func (s *Store) GetGuide(ctx context.Context, userID, guideID string) (*Guide, error) {
	if userID == "" || guideID == "" {
		return nil, errors.New("store: userID and guideID are required")
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: guideSK(guideID)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get guide: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var g Guide
	if err := attributevalue.UnmarshalMap(out.Item, &g); err != nil {
		return nil, fmt.Errorf("store: unmarshal guide: %w", err)
	}
	g.UserID = userID
	return &g, nil
}

// UpsertGuide writes the guide's editable fields and bumps version by 1
// atomically (UpdateItem SET+ADD), creating the item on first write with
// version 1. Returns the stored guide (with its new version).
func (s *Store) UpsertGuide(ctx context.Context, g *Guide) (*Guide, error) {
	switch {
	case g == nil:
		return nil, errors.New("store: guide is required")
	case g.UserID == "" || g.GuideID == "":
		return nil, errors.New("store: guide userID and guideID are required")
	case g.Title == "":
		return nil, errors.New("store: guide title is required")
	case g.Text == "":
		return nil, errors.New("store: guide text is required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(g.UserID)},
			"sk": &types.AttributeValueMemberS{Value: guideSK(g.GuideID)},
		},
		UpdateExpression: aws.String(
			"SET #gid = :gid, #title = :title, #text = :text, #en = :en, #pri = :pri, #upd = :upd ADD #ver :one"),
		ExpressionAttributeNames: map[string]string{
			"#gid":   "guideId",
			"#title": "title",
			"#text":  "text",
			"#en":    "enabled",
			"#pri":   "priority",
			"#upd":   "updatedAt",
			"#ver":   "version",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":gid":   &types.AttributeValueMemberS{Value: g.GuideID},
			":title": &types.AttributeValueMemberS{Value: g.Title},
			":text":  &types.AttributeValueMemberS{Value: g.Text},
			":en":    &types.AttributeValueMemberBOOL{Value: g.Enabled},
			":pri":   &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", g.Priority)},
			":upd":   &types.AttributeValueMemberS{Value: now},
			":one":   &types.AttributeValueMemberN{Value: "1"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: upsert guide: %w", err)
	}
	return s.GetGuide(ctx, g.UserID, g.GuideID)
}

// DeleteGuide removes the guide; ErrNotFound when it didn't exist.
func (s *Store) DeleteGuide(ctx context.Context, userID, guideID string) error {
	if userID == "" || guideID == "" {
		return errors.New("store: userID and guideID are required")
	}
	out, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: guideSK(guideID)},
		},
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return fmt.Errorf("store: delete guide: %w", err)
	}
	if out.Attributes == nil {
		return ErrNotFound
	}
	return nil
}

// ListGuides returns the user's guides sorted by priority ascending
// (ties by guideId for stability). On a user's very first list — no
// GUIDE# items at all — it seeds the default "AI is an emerging
// technology" guide (conditional put, so a racing seed is a no-op) and
// returns it. Deleting the seed guide later sticks only until the next
// empty list; disabling it (enabled=false) is the persistent opt-out,
// which the Guide Manager UI exposes.
func (s *Store) ListGuides(ctx context.Context, userID string) ([]Guide, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}
	guides, err := s.listGuidesRaw(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(guides) == 0 {
		if err := s.seedDefaultGuide(ctx, userID); err != nil {
			return nil, err
		}
		guides, err = s.listGuidesRaw(ctx, userID)
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(guides, func(i, j int) bool {
		if guides[i].Priority != guides[j].Priority {
			return guides[i].Priority < guides[j].Priority
		}
		return guides[i].GuideID < guides[j].GuideID
	})
	return guides, nil
}

// ListEnabledGuides is the broker's session-mint read: enabled guides
// only, priority ascending (FR-MEM-07 always-inject).
func (s *Store) ListEnabledGuides(ctx context.Context, userID string) ([]Guide, error) {
	all, err := s.ListGuides(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, g := range all {
		if g.Enabled {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *Store) listGuidesRaw(ctx context.Context, userID string) ([]Guide, error) {
	items, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: "GUIDE#"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list guides: %w", err)
	}
	out := make([]Guide, 0, len(items))
	for _, raw := range items {
		var g Guide
		if err := attributevalue.UnmarshalMap(raw, &g); err != nil {
			return nil, fmt.Errorf("store: unmarshal guide: %w", err)
		}
		g.UserID = userID
		out = append(out, g)
	}
	return out, nil
}

// seedDefaultGuide conditionally creates the default guide (a lost race
// with another surface's first list is an idempotent no-op).
func (s *Store) seedDefaultGuide(ctx context.Context, userID string) error {
	g := seedGuide()
	err := s.ConditionalPut(ctx, userPK(userID), guideSK(g.GuideID), map[string]any{
		"guideId":   g.GuideID,
		"title":     g.Title,
		"text":      g.Text,
		"enabled":   g.Enabled,
		"priority":  g.Priority,
		"version":   g.Version,
		"updatedAt": time.Now().UTC().Format(time.RFC3339),
	}, 0)
	if err != nil && !errors.Is(err, ErrAlreadyExists) {
		return fmt.Errorf("store: seed default guide: %w", err)
	}
	return nil
}
