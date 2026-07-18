package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Consent is one USER#<uid>/CONSENT#<ts> item — the M7 privacy consent
// ledger (PRD NFR-03 / Crosscut §4: "First-run consent (timestamp +
// version) recorded at CONSENT#<ts>"). One append-only row per consent
// event; consent rows carry no TTL (they are the proof of consent and
// live until the account is purged by cmd/account-purge).
type Consent struct {
	// TS is the server-side RFC3339Nano timestamp — it is also the sort
	// key discriminator, so consent events never overwrite each other.
	TS string `dynamodbav:"ts"`
	// Surface the consent was granted on (web|android|device) — taken
	// from the verified auth context, never from the client body.
	Surface string `dynamodbav:"surface"`
	// Version is the disclosure/policy version string the client showed
	// (e.g. "2026-07-privacy-v1").
	Version string `dynamodbav:"version"`
	// ClientTS is the client-reported grant timestamp (informational;
	// the authoritative time is TS).
	ClientTS string `dynamodbav:"clientTs,omitempty"`
}

// consentSK builds the sort key: CONSENT#<ts>#<random suffix>. The random
// suffix disambiguates two events landing in the same clock tick (Windows
// and coarse-clock hosts can return identical time.Now() nanoseconds for
// back-to-back calls) while keeping the keys time-ordered.
func consentSK(ts string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: consent sk entropy: %w", err)
	}
	return "CONSENT#" + ts + "#" + hex.EncodeToString(b[:]), nil
}

// RecordConsent appends a consent event for the user. surface and version
// are required; clientTS is optional client-reported context. Returns the
// stored record (with the server timestamp that keyed it).
func (s *Store) RecordConsent(ctx context.Context, userID, surface, version, clientTS string) (*Consent, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}
	if strings.TrimSpace(surface) == "" || strings.TrimSpace(version) == "" {
		return nil, errors.New("store: consent surface and version are required")
	}

	c := Consent{
		TS:       time.Now().UTC().Format(time.RFC3339Nano),
		Surface:  surface,
		Version:  version,
		ClientTS: strings.TrimSpace(clientTS),
	}
	sk, err := consentSK(c.TS)
	if err != nil {
		return nil, err
	}
	it := struct {
		PK string `dynamodbav:"pk"`
		SK string `dynamodbav:"sk"`
		Consent
	}{
		PK:      userPK(userID),
		SK:      sk,
		Consent: c,
	}
	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return nil, fmt.Errorf("store: marshal consent: %w", err)
	}
	// Conditional put: the nano timestamp makes collisions practically
	// impossible, but if one ever happened silently overwriting a consent
	// record would be worse than failing loudly.
	if _, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                av,
		ConditionExpression: aws.String("attribute_not_exists(pk) AND attribute_not_exists(sk)"),
	}); err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return nil, ErrAlreadyExists
		}
		return nil, fmt.Errorf("store: record consent: %w", err)
	}
	return &c, nil
}

// ListConsents returns the user's consent events oldest-first (Query on
// the user's own partition, sk begins_with CONSENT# — never a Scan).
func (s *Store) ListConsents(ctx context.Context, userID string) ([]Consent, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}
	items, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: "CONSENT#"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list consents: %w", err)
	}
	out := make([]Consent, 0, len(items))
	for _, raw := range items {
		var c Consent
		if err := attributevalue.UnmarshalMap(raw, &c); err != nil {
			return nil, fmt.Errorf("store: unmarshal consent: %w", err)
		}
		out = append(out, c)
	}
	return out, nil
}
