package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// UserStatusDeleting marks an account whose right-to-delete purge has been
// requested (DELETE /api/v1/account) and is running asynchronously in
// cmd/account-purge. Deliberately declared here (the M7 privacy/account
// file) rather than in types.go, which other M7 workstreams also touch.
const UserStatusDeleting = "deleting"

// SetUserStatus updates the user's status attribute (active | disabled |
// suspended | deleting). Conditional on the profile row existing —
// returns ErrNotFound otherwise.
func (s *Store) SetUserStatus(ctx context.Context, userID, status string) error {
	if userID == "" || status == "" {
		return errors.New("store: userID and status are required")
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.table),
		Key:                 keyOf(userPK(userID), skProfile),
		UpdateExpression:    aws.String("SET #st = :s"),
		ConditionExpression: aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames: map[string]string{
			"#st": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":s": &types.AttributeValueMemberS{Value: status},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrNotFound
		}
		return fmt.Errorf("store: set user status: %w", err)
	}
	return nil
}

// QueryUserPartition returns every item in the USER#<uid> partition as
// generic maps, following pagination to exhaustion. A single-partition
// Query (never a Scan) — this is the data source for both the account
// export (GET /api/v1/account/export) and the purge Lambda's delete pass.
func (s *Store) QueryUserPartition(ctx context.Context, userID string) ([]map[string]any, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}
	raw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: userPK(userID)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: query user partition: %w", err)
	}
	items := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		var m map[string]any
		if err := attributevalue.UnmarshalMap(r, &m); err != nil {
			return nil, fmt.Errorf("store: unmarshal user partition item: %w", err)
		}
		items = append(items, m)
	}
	return items, nil
}
