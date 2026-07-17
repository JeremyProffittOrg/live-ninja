package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// userItem is the raw USER#<uid>/PROFILE row (User + table/GSI keys).
type userItem struct {
	PK     string `dynamodbav:"pk"`
	SK     string `dynamodbav:"sk"`
	Gsi1PK string `dynamodbav:"gsi1pk"`
	Gsi1SK string `dynamodbav:"gsi1sk"`
	User
}

// GetUserByLWA looks a user up by Amazon (LWA) user id via GSI1
// (gsi1pk=LWA#<amazonUserId>, gsi1sk=PROFILE). Returns (nil, nil) when no
// such user exists.
func (s *Store) GetUserByLWA(ctx context.Context, amazonUserID string) (*User, error) {
	out, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		IndexName:              aws.String(indexGSI1),
		KeyConditionExpression: aws.String("#g1pk = :pk AND #g1sk = :sk"),
		ExpressionAttributeNames: map[string]string{
			"#g1pk": "gsi1pk",
			"#g1sk": "gsi1sk",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: lwaGSI1PK(amazonUserID)},
			":sk": &types.AttributeValueMemberS{Value: skProfile},
		},
		Limit: aws.Int32(1),
	})
	if err != nil {
		return nil, fmt.Errorf("store: query user by lwa: %w", err)
	}
	if len(out.Items) == 0 {
		return nil, nil
	}
	var it userItem
	if err := attributevalue.UnmarshalMap(out.Items[0], &it); err != nil {
		return nil, fmt.Errorf("store: unmarshal user: %w", err)
	}
	u := it.User
	return &u, nil
}

// CreateUser writes a new USER#<uid>/PROFILE item. Conditional on the key
// not existing — returns ErrAlreadyExists if the user is already present
// (callers that want upsert semantics should GetUserByLWA first).
func (s *Store) CreateUser(ctx context.Context, u *User) error {
	if u.UserID == "" || u.AmazonUserID == "" {
		return errors.New("store: userID and amazonUserID are required")
	}
	if u.CreatedAt == 0 {
		u.CreatedAt = time.Now().Unix()
	}
	it := userItem{
		PK:     userPK(u.UserID),
		SK:     skProfile,
		Gsi1PK: lwaGSI1PK(u.AmazonUserID),
		Gsi1SK: skProfile,
		User:   *u,
	}
	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return fmt.Errorf("store: marshal user: %w", err)
	}
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                av,
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("store: create user: %w", err)
	}
	return nil
}

// GetUser fetches USER#<uid>/PROFILE by primary key. Returns (nil, nil)
// when absent.
func (s *Store) GetUser(ctx context.Context, userID string) (*User, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       keyOf(userPK(userID), skProfile),
	})
	if err != nil {
		return nil, fmt.Errorf("store: get user: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var it userItem
	if err := attributevalue.UnmarshalMap(out.Item, &it); err != nil {
		return nil, fmt.Errorf("store: unmarshal user: %w", err)
	}
	u := it.User
	return &u, nil
}

// SetTokensValidAfter bumps the user's tokensValidAfter watermark (unix
// seconds) — every access JWT with iat < t is rejected by the authorizer,
// implementing "log out everywhere". Returns ErrNotFound if the user row
// does not exist.
func (s *Store) SetTokensValidAfter(ctx context.Context, userID string, t int64) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.table),
		Key:                 keyOf(userPK(userID), skProfile),
		UpdateExpression:    aws.String("SET #tva = :t"),
		ConditionExpression: aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames: map[string]string{
			"#tva": "tokensValidAfter",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":t": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", t)},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrNotFound
		}
		return fmt.Errorf("store: set tokensValidAfter: %w", err)
	}
	return nil
}

// GetOwner fetches the CONFIG/OWNER singleton. Returns (nil, nil) while
// the deployment is still unbound (no one has signed in yet).
func (s *Store) GetOwner(ctx context.Context) (*OwnerBinding, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       keyOf(pkConfig, skOwner),
	})
	if err != nil {
		return nil, fmt.Errorf("store: get owner: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var ob OwnerBinding
	if err := attributevalue.UnmarshalMap(out.Item, &ob); err != nil {
		return nil, fmt.Errorf("store: unmarshal owner: %w", err)
	}
	return &ob, nil
}

// BindOwner claims the CONFIG/OWNER singleton for the given identity.
// First successful sign-in wins via attribute_not_exists(pk); a lost race
// (or any later attempt) returns ErrAlreadyBound.
func (s *Store) BindOwner(ctx context.Context, amazonUserID, userID string) error {
	ob := struct {
		PK string `dynamodbav:"pk"`
		SK string `dynamodbav:"sk"`
		OwnerBinding
	}{
		PK: pkConfig,
		SK: skOwner,
		OwnerBinding: OwnerBinding{
			AmazonUserID: amazonUserID,
			UserID:       userID,
			BoundAt:      time.Now().UTC().Format(time.RFC3339),
		},
	}
	av, err := attributevalue.MarshalMap(ob)
	if err != nil {
		return fmt.Errorf("store: marshal owner binding: %w", err)
	}
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                av,
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrAlreadyBound
		}
		return fmt.Errorf("store: bind owner: %w", err)
	}
	return nil
}

// normalizeAllowKey lowercases email-form keys (emails are matched
// case-insensitively); Amazon user ids (amzn1.account.…) pass through
// unchanged since they are case-sensitive opaque ids.
func normalizeAllowKey(key string) string {
	if strings.Contains(key, "@") {
		return strings.ToLower(strings.TrimSpace(key))
	}
	return strings.TrimSpace(key)
}

// IsAllowed reports whether either the Amazon user id or the (lowercased)
// email appears on the CONFIG allowlist. Two GetItem key lookups — access
// = owner OR allowlisted; everyone else is rejected upstream with 403.
func (s *Store) IsAllowed(ctx context.Context, amazonUserID, email string) (bool, error) {
	for _, k := range []string{amazonUserID, email} {
		if k == "" {
			continue
		}
		out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(s.table),
			Key:       keyOf(pkConfig, allowSK(normalizeAllowKey(k))),
		})
		if err != nil {
			return false, fmt.Errorf("store: get allow entry: %w", err)
		}
		if out.Item != nil {
			return true, nil
		}
	}
	return false, nil
}

// AddAllow puts a CONFIG/ALLOW#<key> entry (key is an Amazon user id or
// an email, normalized). Idempotent — re-adding overwrites addedBy/addedAt.
func (s *Store) AddAllow(ctx context.Context, key, addedBy string) error {
	key = normalizeAllowKey(key)
	if key == "" {
		return errors.New("store: allow key is required")
	}
	it := struct {
		PK string `dynamodbav:"pk"`
		SK string `dynamodbav:"sk"`
		AllowEntry
	}{
		PK: pkConfig,
		SK: allowSK(key),
		AllowEntry: AllowEntry{
			AddedBy: addedBy,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
		},
	}
	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return fmt.Errorf("store: marshal allow entry: %w", err)
	}
	if _, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	}); err != nil {
		return fmt.Errorf("store: add allow entry: %w", err)
	}
	return nil
}

// RemoveAllow deletes a CONFIG/ALLOW#<key> entry. Idempotent no-op when
// the entry is already absent.
func (s *Store) RemoveAllow(ctx context.Context, key string) error {
	if _, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       keyOf(pkConfig, allowSK(normalizeAllowKey(key))),
	}); err != nil {
		return fmt.Errorf("store: remove allow entry: %w", err)
	}
	return nil
}

// ListAllow returns every allowlist entry (Query on the CONFIG partition,
// sk begins_with ALLOW# — a tiny bounded set, never a Scan).
func (s *Store) ListAllow(ctx context.Context) ([]AllowEntry, error) {
	items, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: pkConfig},
			":pfx": &types.AttributeValueMemberS{Value: "ALLOW#"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list allow entries: %w", err)
	}
	entries := make([]AllowEntry, 0, len(items))
	for _, raw := range items {
		var e struct {
			SK string `dynamodbav:"sk"`
			AllowEntry
		}
		if err := attributevalue.UnmarshalMap(raw, &e); err != nil {
			return nil, fmt.Errorf("store: unmarshal allow entry: %w", err)
		}
		e.AllowEntry.Key = strings.TrimPrefix(e.SK, "ALLOW#")
		entries = append(entries, e.AllowEntry)
	}
	return entries, nil
}

// keyOf builds a pk/sk primary-key map.
func keyOf(pk, sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
		"sk": &types.AttributeValueMemberS{Value: sk},
	}
}
