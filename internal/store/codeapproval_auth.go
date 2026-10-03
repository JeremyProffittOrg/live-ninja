package store

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// GetUserConsistent is for human approval boundaries that must not use an
// eventually consistent profile snapshot. Mutation services must additionally
// fence the active-owner condition in the same transaction as the write.
func (s *Store) GetUserConsistent(ctx context.Context, userID string) (*User, error) {
	out, e := s.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(s.table), Key: keyOf(userPK(userID), skProfile), ConsistentRead: aws.Bool(true)})
	if e != nil {
		return nil, fmt.Errorf("store: read current user: %w", e)
	}
	if len(out.Item) == 0 {
		return nil, nil
	}
	var item userItem
	if e = attributevalue.UnmarshalMap(out.Item, &item); e != nil {
		return nil, fmt.Errorf("store: decode current user: %w", e)
	}
	return &item.User, nil
}
