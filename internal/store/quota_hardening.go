package store

// This file is the quota-hardening store surface (M7): the "suspended"
// user status and the PROFILE transitions around it.
//
// Auto-suspension itself is performed inline by the realtime quota gate
// (internal/realtime/quota.go, hourly-burn anomaly) with the exact same
// conditional write as SuspendUser below — keep the two in sync. These
// store methods are the supported path for every non-broker caller
// (owner/admin remediation, account lifecycle): there must always be a
// programmatic way to reinstate a falsely-tripped account without hand
// editing DynamoDB items.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// UserStatusSuspended marks a user auto-suspended by the quota-hardening
// gate (or manually via SuspendUser). The authorizer denies every status
// that is not UserStatusActive, so a suspended user loses all API access
// within its 60s user-cache window — and immediately on the broker mint
// path, which re-reads the profile per request.
const UserStatusSuspended = "suspended"

// SuspendUser transitions USER#<uid>/PROFILE from active to suspended,
// recording suspendReason/suspendedAt and bumping tokensValidAfter so all
// outstanding access JWTs are invalidated (same kill-switch as "log out
// everywhere"). Conditional on the profile existing AND currently being
// active: returns ErrNotFound when the user is absent or not active
// (already suspended, disabled, deleting, ...), so the transition — and
// any caller-side alerting keyed on it — happens exactly once.
func (s *Store) SuspendUser(ctx context.Context, userID, reason string) error {
	if userID == "" {
		return errors.New("store: userID is required")
	}
	now := time.Now().UTC()
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.table),
		Key:                 keyOf(userPK(userID), skProfile),
		UpdateExpression:    aws.String("SET #st = :susp, suspendReason = :r, suspendedAt = :ts, tokensValidAfter = :now"),
		ConditionExpression: aws.String("attribute_exists(pk) AND #st = :active"),
		ExpressionAttributeNames: map[string]string{
			"#st": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":susp":   &types.AttributeValueMemberS{Value: UserStatusSuspended},
			":active": &types.AttributeValueMemberS{Value: UserStatusActive},
			":r":      &types.AttributeValueMemberS{Value: reason},
			":ts":     &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
			":now":    &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now.Unix())},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrNotFound
		}
		return fmt.Errorf("store: suspend user: %w", err)
	}
	return nil
}

// ReinstateUser transitions a suspended user back to active (owner
// remediation after a false-positive burn trip), recording reinstatedAt.
// The stale suspendReason/suspendedAt attributes are deliberately left in
// place as an audit trail of the last suspension. Conditional on the
// profile currently being suspended; returns ErrNotFound otherwise.
// Existing sessions stay dead (tokensValidAfter is NOT rolled back) — the
// user signs in again.
func (s *Store) ReinstateUser(ctx context.Context, userID string) error {
	if userID == "" {
		return errors.New("store: userID is required")
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.table),
		Key:                 keyOf(userPK(userID), skProfile),
		UpdateExpression:    aws.String("SET #st = :active, reinstatedAt = :ts"),
		ConditionExpression: aws.String("attribute_exists(pk) AND #st = :susp"),
		ExpressionAttributeNames: map[string]string{
			"#st": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":active": &types.AttributeValueMemberS{Value: UserStatusActive},
			":susp":   &types.AttributeValueMemberS{Value: UserStatusSuspended},
			":ts":     &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrNotFound
		}
		return fmt.Errorf("store: reinstate user: %w", err)
	}
	return nil
}
