package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Wake-word items (M6 FR-K01..06, plan.md M6 training pipeline task).
//
//	pk=USER#<uid>  sk=WAKEWORD#<wwId>
//
// One item per custom wake-word training request, tracking the async
// AWS Batch openWakeWord training job's lifecycle
// (pending → training → ready | failed). The item is inherently
// owner-scoped by living in the user's partition — every access is a
// GetItem or a single-partition Query, never a Scan, and there is no
// cross-user lookup path (the catalog's builtin half is static code,
// per internal/wakeword/catalog.go).
//
// The daily training quota (≤3/day/user, plan.md M6 / risk #7) is a
// USAGE-style atomic counter item at pk=USER#<uid> sk=WWTRAIN#<day>
// with a 48h TTL — see TakeWakewordTrainingSlot.

// Wake-word training statuses (single source of truth for the strings
// stored in DynamoDB and returned by GET /api/v1/wakeword/:id).
const (
	WakewordStatusPending  = "pending"
	WakewordStatusTraining = "training"
	WakewordStatusReady    = "ready"
	WakewordStatusFailed   = "failed"
)

// wwTrainSlotTTL bounds the daily-quota counter rows (48h covers the
// UTC day plus clock skew; DynamoDB TTL sweeps them).
const wwTrainSlotTTL = 48 * time.Hour

// Wakeword is the USER#<uid>/WAKEWORD#<wwId> item.
type Wakeword struct {
	ID               string   `dynamodbav:"wwId"`
	UserID           string   `dynamodbav:"userId"`
	Phrase           string   `dynamodbav:"phrase"`           // display form, e.g. "Hey Ninja"
	NormalizedPhrase string   `dynamodbav:"normalizedPhrase"` // lowercase collapsed, collision key
	Engine           string   `dynamodbav:"engine"`           // openwakeword (only server-trainable engine)
	Status           string   `dynamodbav:"status"`           // pending | training | ready | failed
	BatchJobID       string   `dynamodbav:"batchJobId,omitempty"`
	FailureReason    string   `dynamodbav:"failureReason,omitempty"`
	Platforms        []string `dynamodbav:"platforms,omitempty"` // filled at ready: ["web","android"]
	CreatedAt        string   `dynamodbav:"createdAt"`           // RFC3339
	ReadyAt          string   `dynamodbav:"readyAt,omitempty"`   // RFC3339
}

func wakewordSK(id string) string { return "WAKEWORD#" + id }
func wwTrainSK(day string) string { return "WWTRAIN#" + day }
func wakewordItemPKSK(w *Wakeword) (string, string) {
	return userPK(w.UserID), wakewordSK(w.ID)
}

// CreateWakeword conditionally creates the item; ErrAlreadyExists when
// an item with that wwId already exists for this user (the phrase-slug
// collision the service layer maps to HTTP 409).
func (s *Store) CreateWakeword(ctx context.Context, w *Wakeword) error {
	return s.putWakeword(ctx, w, true)
}

// ReplaceWakeword overwrites the item unconditionally — the
// retrain-after-failed path (same wwId, fresh pending record).
func (s *Store) ReplaceWakeword(ctx context.Context, w *Wakeword) error {
	return s.putWakeword(ctx, w, false)
}

func (s *Store) putWakeword(ctx context.Context, w *Wakeword, conditional bool) error {
	if w == nil || w.UserID == "" || w.ID == "" {
		return errors.New("store: wakeword userID and id are required")
	}
	if w.CreatedAt == "" {
		w.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	pk, sk := wakewordItemPKSK(w)
	it := struct {
		PK string `dynamodbav:"pk"`
		SK string `dynamodbav:"sk"`
		Wakeword
	}{PK: pk, SK: sk, Wakeword: *w}

	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return fmt.Errorf("store: marshal wakeword: %w", err)
	}
	in := &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	}
	if conditional {
		in.ConditionExpression = aws.String("attribute_not_exists(pk)")
	}
	if _, err := s.client.PutItem(ctx, in); err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("store: put wakeword: %w", err)
	}
	return nil
}

// GetWakeword fetches one wake-word item; (nil, nil) when absent.
func (s *Store) GetWakeword(ctx context.Context, userID, id string) (*Wakeword, error) {
	if userID == "" || id == "" {
		return nil, errors.New("store: userID and id are required")
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       keyOf(userPK(userID), wakewordSK(id)),
	})
	if err != nil {
		return nil, fmt.Errorf("store: get wakeword: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var w Wakeword
	if err := attributevalue.UnmarshalMap(out.Item, &w); err != nil {
		return nil, fmt.Errorf("store: unmarshal wakeword: %w", err)
	}
	return &w, nil
}

// ListWakewords returns every wake-word item for a user
// (single-partition Query on sk begins_with WAKEWORD#; a user has at
// most a handful, bounded by the ≤3/day training quota).
func (s *Store) ListWakewords(ctx context.Context, userID string) ([]Wakeword, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}
	raw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: "WAKEWORD#"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list wakewords: %w", err)
	}
	out := make([]Wakeword, 0, len(raw))
	for _, r := range raw {
		var w Wakeword
		if err := attributevalue.UnmarshalMap(r, &w); err != nil {
			return nil, fmt.Errorf("store: unmarshal wakeword: %w", err)
		}
		out = append(out, w)
	}
	return out, nil
}

// UpdateWakewordStatus transitions an item's lifecycle fields in place
// (status plus, when non-zero, failureReason / platforms / readyAt).
// Conditional on the item existing — ErrNotFound otherwise (a deleted
// wake word must not be resurrected by a late finalize).
func (s *Store) UpdateWakewordStatus(ctx context.Context, userID, id, status, failureReason string, platforms []string, readyAt string) error {
	if userID == "" || id == "" || status == "" {
		return errors.New("store: userID, id and status are required")
	}
	expr := "SET #st = :st"
	names := map[string]string{"#st": "status"}
	values := map[string]types.AttributeValue{
		":st": &types.AttributeValueMemberS{Value: status},
	}
	if failureReason != "" {
		expr += ", #fr = :fr"
		names["#fr"] = "failureReason"
		values[":fr"] = &types.AttributeValueMemberS{Value: failureReason}
	}
	if len(platforms) > 0 {
		expr += ", #pl = :pl"
		names["#pl"] = "platforms"
		list := make([]types.AttributeValue, len(platforms))
		for i, p := range platforms {
			list[i] = &types.AttributeValueMemberS{Value: p}
		}
		values[":pl"] = &types.AttributeValueMemberL{Value: list}
	}
	if readyAt != "" {
		expr += ", #ra = :ra"
		names["#ra"] = "readyAt"
		values[":ra"] = &types.AttributeValueMemberS{Value: readyAt}
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.table),
		Key:                       keyOf(userPK(userID), wakewordSK(id)),
		UpdateExpression:          aws.String(expr),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrNotFound
		}
		return fmt.Errorf("store: update wakeword status: %w", err)
	}
	return nil
}

// SetWakewordJobID records the submitted AWS Batch job id on the item
// (called right after SubmitJob succeeds).
func (s *Store) SetWakewordJobID(ctx context.Context, userID, id, jobID string) error {
	if userID == "" || id == "" || jobID == "" {
		return errors.New("store: userID, id and jobID are required")
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                aws.String(s.table),
		Key:                      keyOf(userPK(userID), wakewordSK(id)),
		UpdateExpression:         aws.String("SET #j = :j"),
		ConditionExpression:      aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames: map[string]string{"#j": "batchJobId"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":j": &types.AttributeValueMemberS{Value: jobID},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrNotFound
		}
		return fmt.Errorf("store: set wakeword job id: %w", err)
	}
	return nil
}

// DeleteWakeword removes the item (idempotent — deleting an absent item
// is a no-op).
func (s *Store) DeleteWakeword(ctx context.Context, userID, id string) error {
	if userID == "" || id == "" {
		return errors.New("store: userID and id are required")
	}
	if _, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       keyOf(userPK(userID), wakewordSK(id)),
	}); err != nil {
		return fmt.Errorf("store: delete wakeword: %w", err)
	}
	return nil
}

// TakeWakewordTrainingSlot atomically consumes one of the user's daily
// training slots (day formatted "2006-01-02" UTC, max = the ≤3/day cap).
// Returns (true, nil) when a slot was taken, (false, nil) when the cap
// is already reached. Implemented as a single conditional UpdateItem ADD
// on pk=USER#<uid> sk=WWTRAIN#<day> — no read-modify-write race, and the
// row self-expires via TTL.
func (s *Store) TakeWakewordTrainingSlot(ctx context.Context, userID, day string, max int) (bool, error) {
	if userID == "" || day == "" || max < 1 {
		return false, errors.New("store: userID, day and positive max are required")
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.table),
		Key:                 keyOf(userPK(userID), wwTrainSK(day)),
		UpdateExpression:    aws.String("SET #ttl = if_not_exists(#ttl, :ttl) ADD #c :one"),
		ConditionExpression: aws.String("attribute_not_exists(pk) OR #c < :max"),
		ExpressionAttributeNames: map[string]string{
			"#c":   "count",
			"#ttl": "ttl",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one": &types.AttributeValueMemberN{Value: "1"},
			":max": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", max)},
			":ttl": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", time.Now().Add(wwTrainSlotTTL).Unix())},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return false, nil
		}
		return false, fmt.Errorf("store: take wakeword training slot: %w", err)
	}
	return true, nil
}

// ReturnWakewordTrainingSlot gives back a slot consumed by
// TakeWakewordTrainingSlot when the submission it gated never happened
// (Batch SubmitJob failed, queue full). Best-effort: a missing counter
// row is a no-op.
func (s *Store) ReturnWakewordTrainingSlot(ctx context.Context, userID, day string) error {
	if userID == "" || day == "" {
		return errors.New("store: userID and day are required")
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                aws.String(s.table),
		Key:                      keyOf(userPK(userID), wwTrainSK(day)),
		UpdateExpression:         aws.String("ADD #c :neg"),
		ConditionExpression:      aws.String("attribute_exists(pk) AND #c > :zero"),
		ExpressionAttributeNames: map[string]string{"#c": "count"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":neg":  &types.AttributeValueMemberN{Value: "-1"},
			":zero": &types.AttributeValueMemberN{Value: "0"},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return nil // nothing to return — fine
		}
		return fmt.Errorf("store: return wakeword training slot: %w", err)
	}
	return nil
}
