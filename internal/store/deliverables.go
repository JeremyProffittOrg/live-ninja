package store

// Deliverables index items (M9 Deliverables Store, FR-DLV-01..06).
//
// Item shape (locked M9 decision):
//
//	pk     = USER#<uid>
//	sk     = DELIV#<createdAt RFC3339>#<deliverableId>   (chronological sk order)
//	gsi1pk = DELIV#<deliverableId>                        (id → item lookup)
//	gsi1sk = DELIV
//
// The object bytes live in the dedicated deliverables S3 bucket under
// deliverables/<uid>/<deliverableId>/<filename> (internal/deliv owns the
// key discipline; every consumer MUST prefix-check the key against the
// caller's user id before presigning or streaming). Listing is a
// single-partition Query with begins_with(sk, DELIV#) — never a Scan —
// and the id lookup is a GSI1 Query, per the repo's DynamoDB read-cost
// rules.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Deliverable lifecycle/kind enums (single source of truth for the strings
// persisted in DynamoDB).
const (
	DeliverableKindFile = "file" // direct content upload (deliverable_create)
	DeliverableKindZip  = "zip"  // bundle produced by the zipper Lambda

	DeliverableStatusPending = "pending" // zip requested, zipper not finished
	DeliverableStatusReady   = "ready"   // object present and downloadable
	DeliverableStatusFailed  = "failed"  // zipper failed; no usable object
)

// deliverableSKPrefix is the sk namespace for deliverable index items.
const deliverableSKPrefix = "DELIV#"

// deliverableNameSKPrefix is the sk namespace for per-user filename
// uniqueness claims (owner rule: the assistant may create documents but
// never overwrite one, so a filename is claimed atomically before any
// object write). "DELIVNAME#" deliberately does NOT begin with "DELIV#"
// (N != #), so claims never surface in ListDeliverables' begins_with
// Query.
const deliverableNameSKPrefix = "DELIVNAME#"

func deliverableSK(createdAt, id string) string {
	return deliverableSKPrefix + createdAt + "#" + id
}

func deliverableNameSK(name string) string { return deliverableNameSKPrefix + name }

func deliverableGSI1PK(id string) string { return "DELIV#" + id }

// Deliverable is one USER#<uid>/DELIV#<createdAt>#<id> index item.
type Deliverable struct {
	DeliverableID string   `dynamodbav:"deliverableId"`
	UserID        string   `dynamodbav:"userId"`
	Name          string   `dynamodbav:"name"`        // display filename, e.g. "report.md" or "bundle.zip"
	ContentType   string   `dynamodbav:"contentType"` // MIME type of the S3 object
	Kind          string   `dynamodbav:"kind"`        // file | zip
	Status        string   `dynamodbav:"status"`      // pending | ready | failed
	S3Key         string   `dynamodbav:"s3Key"`       // deliverables/<uid>/<id>/<filename>
	SizeBytes     int64    `dynamodbav:"sizeBytes"`
	CreatedAt     string   `dynamodbav:"createdAt"`         // RFC3339 UTC; also embedded in sk
	Sources       []string `dynamodbav:"sources,omitempty"` // zip only: bundled deliverable ids
}

// SK returns the item's sort key (needed by the zipper Lambda to address
// the exact item for its status write-back).
func (d *Deliverable) SK() string { return deliverableSK(d.CreatedAt, d.DeliverableID) }

// CreateDeliverable writes a new deliverable index item. The conditional
// put makes an id/createdAt collision (practically impossible with UUID
// ids) an ErrAlreadyExists instead of a silent overwrite.
func (s *Store) CreateDeliverable(ctx context.Context, d *Deliverable) error {
	switch {
	case d == nil:
		return errors.New("store: deliverable is required")
	case d.UserID == "" || d.DeliverableID == "" || d.CreatedAt == "":
		return errors.New("store: deliverable userID/deliverableID/createdAt are required")
	case d.S3Key == "":
		return errors.New("store: deliverable s3Key is required")
	}

	av, err := attributevalue.MarshalMap(d)
	if err != nil {
		return fmt.Errorf("store: marshal deliverable: %w", err)
	}
	av["pk"] = &types.AttributeValueMemberS{Value: userPK(d.UserID)}
	av["sk"] = &types.AttributeValueMemberS{Value: d.SK()}
	av["gsi1pk"] = &types.AttributeValueMemberS{Value: deliverableGSI1PK(d.DeliverableID)}
	av["gsi1sk"] = &types.AttributeValueMemberS{Value: "DELIV"}

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
		return fmt.Errorf("store: create deliverable: %w", err)
	}
	return nil
}

// ClaimDeliverableName atomically claims a display filename for one user
// via a conditional PutItem at USER#<uid> / DELIVNAME#<name>. It is the
// load-bearing no-overwrite guarantee of the deliverables corpus: two
// concurrent creates for the same name race on this single conditional
// write, and exactly one wins — never check-then-put. ErrAlreadyExists
// when the name is already claimed.
func (s *Store) ClaimDeliverableName(ctx context.Context, userID, name, deliverableID string) error {
	if userID == "" || name == "" || deliverableID == "" {
		return errors.New("store: userID, name, and deliverableID are required")
	}
	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"pk":            &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk":            &types.AttributeValueMemberS{Value: deliverableNameSK(name)},
			"userId":        &types.AttributeValueMemberS{Value: userID},
			"name":          &types.AttributeValueMemberS{Value: name},
			"deliverableId": &types.AttributeValueMemberS{Value: deliverableID},
		},
		ConditionExpression: aws.String("attribute_not_exists(pk) AND attribute_not_exists(sk)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("store: claim deliverable name: %w", err)
	}
	return nil
}

// ReleaseDeliverableName frees a claimed filename (rollback of a failed
// create, or deletion of the deliverable that owned it). Deleting an
// absent claim is a no-op, so legacy deliverables that predate name
// claims release harmlessly.
func (s *Store) ReleaseDeliverableName(ctx context.Context, userID, name string) error {
	if userID == "" || name == "" {
		return errors.New("store: userID and name are required")
	}
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: deliverableNameSK(name)},
		},
	})
	if err != nil {
		return fmt.Errorf("store: release deliverable name: %w", err)
	}
	return nil
}

// GetDeliverable resolves a deliverable by id via GSI1 and enforces
// ownership: an item that exists but belongs to a different user returns
// (nil, nil) exactly like an absent one, so callers can never distinguish
// (and therefore never enumerate) other users' deliverable ids.
func (s *Store) GetDeliverable(ctx context.Context, userID, deliverableID string) (*Deliverable, error) {
	if userID == "" || deliverableID == "" {
		return nil, errors.New("store: userID and deliverableID are required")
	}

	out, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		IndexName:              aws.String(indexGSI1),
		KeyConditionExpression: aws.String("gsi1pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: deliverableGSI1PK(deliverableID)},
		},
		Limit: aws.Int32(1),
	})
	if err != nil {
		return nil, fmt.Errorf("store: get deliverable: %w", err)
	}
	if len(out.Items) == 0 {
		return nil, nil
	}

	var d Deliverable
	if err := attributevalue.UnmarshalMap(out.Items[0], &d); err != nil {
		return nil, fmt.Errorf("store: unmarshal deliverable: %w", err)
	}
	if d.UserID != userID {
		return nil, nil // owned by someone else — indistinguishable from absent
	}
	return &d, nil
}

// ListDeliverables pages the caller's deliverables newest-first: a
// single-partition Query on pk=USER#<uid> with begins_with(sk, DELIV#),
// descending sk (createdAt-prefixed → reverse-chronological). cursor is
// the opaque nextCursor from a previous page ("" for the first page);
// the returned nextCursor is "" when no further pages exist.
func (s *Store) ListDeliverables(ctx context.Context, userID string, limit int32, cursor string) ([]Deliverable, string, error) {
	if userID == "" {
		return nil, "", errors.New("store: userID is required")
	}
	if limit < 1 {
		limit = 25
	}

	in := &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: deliverableSKPrefix},
		},
		ScanIndexForward: aws.Bool(false), // newest first
		Limit:            aws.Int32(limit),
	}
	if cursor != "" {
		sk, err := decodeDeliverableCursor(cursor)
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
		return nil, "", fmt.Errorf("store: list deliverables: %w", err)
	}

	items := make([]Deliverable, 0, len(out.Items))
	for _, raw := range out.Items {
		var d Deliverable
		if err := attributevalue.UnmarshalMap(raw, &d); err != nil {
			return nil, "", fmt.Errorf("store: unmarshal deliverable: %w", err)
		}
		items = append(items, d)
	}

	next := ""
	if lek := out.LastEvaluatedKey; len(lek) > 0 {
		if skAV, ok := lek["sk"].(*types.AttributeValueMemberS); ok {
			next = encodeDeliverableCursor(skAV.Value)
		}
	}
	return items, next, nil
}

// UpdateDeliverableStatus is the zipper Lambda's write-back: flip a
// pending zip item to ready/failed and record the final object size.
// The attribute_exists condition means a status write against a deleted
// item surfaces ErrNotFound instead of resurrecting a ghost row.
func (s *Store) UpdateDeliverableStatus(ctx context.Context, userID, sk, status string, sizeBytes int64) error {
	if userID == "" || sk == "" {
		return errors.New("store: userID and sk are required")
	}
	if !strings.HasPrefix(sk, deliverableSKPrefix) {
		return errors.New("store: sk is not a deliverable sort key")
	}

	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: sk},
		},
		ConditionExpression: aws.String("attribute_exists(pk)"),
		UpdateExpression:    aws.String("SET #st = :st, sizeBytes = :sz"),
		ExpressionAttributeNames: map[string]string{
			"#st": "status", // reserved word in DynamoDB expressions
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":st": &types.AttributeValueMemberS{Value: status},
			":sz": &types.AttributeValueMemberN{Value: strconv.FormatInt(sizeBytes, 10)},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrNotFound
		}
		return fmt.Errorf("store: update deliverable status: %w", err)
	}
	return nil
}

// DeleteDeliverable removes the index item. Callers resolve ownership
// via GetDeliverable first (that is also where they learn the sk), so no
// condition is needed here — deleting an already-absent key is a no-op.
func (s *Store) DeleteDeliverable(ctx context.Context, userID, sk string) error {
	if userID == "" || sk == "" {
		return errors.New("store: userID and sk are required")
	}
	if !strings.HasPrefix(sk, deliverableSKPrefix) {
		return errors.New("store: sk is not a deliverable sort key")
	}

	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil {
		return fmt.Errorf("store: delete deliverable: %w", err)
	}
	return nil
}

// ---- opaque list cursor ----
//
// The cursor is just the last-seen sk, base64url-wrapped so clients treat
// it as opaque. The pk is re-derived from the authenticated caller, so a
// tampered cursor can never walk another user's partition — at worst it
// mis-positions within the caller's own DELIV# range (and anything not
// DELIV#-prefixed is rejected outright).

func encodeDeliverableCursor(sk string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(sk))
}

func decodeDeliverableCursor(cursor string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", fmt.Errorf("store: invalid cursor: %w", err)
	}
	sk := string(b)
	if !strings.HasPrefix(sk, deliverableSKPrefix) {
		return "", errors.New("store: invalid cursor")
	}
	return sk, nil
}
