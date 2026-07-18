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

const (
	oauthStateTTL  = 10 * time.Minute
	pairTTL        = 15 * time.Minute
	pairConfirmTTL = 10 * time.Minute
)

// PutOAuthState stores the OAUTH#<state>/STATE row holding the PKCE
// verifier for one authorization round-trip. Conditional on the state not
// existing (states are single-use random values — a collision is a bug or
// an attack). Fills CreatedAt/TTL (now+10min) if unset.
func (s *Store) PutOAuthState(ctx context.Context, st *OAuthState) error {
	if st.State == "" || st.CodeVerifier == "" {
		return errors.New("store: state and codeVerifier are required")
	}
	now := time.Now()
	if st.CreatedAt == 0 {
		st.CreatedAt = now.Unix()
	}
	if st.TTL == 0 {
		st.TTL = now.Add(oauthStateTTL).Unix()
	}
	it := struct {
		PK string `dynamodbav:"pk"`
		SK string `dynamodbav:"sk"`
		OAuthState
	}{
		PK:         oauthPK(st.State),
		SK:         skState,
		OAuthState: *st,
	}
	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return fmt.Errorf("store: marshal oauth state: %w", err)
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
		return fmt.Errorf("store: put oauth state: %w", err)
	}
	return nil
}

// GetOAuthState consumes an OAuth state — a one-shot read implemented as
// DeleteItem with ReturnValues ALL_OLD, so the state can never be
// replayed: the first caller gets the verifier, every later caller gets
// (nil, nil). Also returns (nil, nil) for rows past their TTL that
// DynamoDB has not reaped yet.
func (s *Store) GetOAuthState(ctx context.Context, state string) (*OAuthState, error) {
	out, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:    aws.String(s.table),
		Key:          keyOf(oauthPK(state), skState),
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return nil, fmt.Errorf("store: consume oauth state: %w", err)
	}
	if len(out.Attributes) == 0 {
		return nil, nil
	}
	var st OAuthState
	if err := attributevalue.UnmarshalMap(out.Attributes, &st); err != nil {
		return nil, fmt.Errorf("store: unmarshal oauth state: %w", err)
	}
	if st.TTL > 0 && st.TTL < time.Now().Unix() {
		return nil, nil // expired; already deleted above either way
	}
	st.State = state
	return &st, nil
}

// CreatePair registers a PAIR#<nonce> row (device pairing bootstrap).
// Conditional on the nonce not existing; status defaults to pending and
// TTL to now+15min. UserCode is mandatory — the anti-phishing confirm leg
// (internal/auth BindPairing) refuses to bind without a matching code, so
// a PAIR row without one would be permanently unbindable anyway. Returns
// ErrAlreadyExists on nonce collision.
func (s *Store) CreatePair(ctx context.Context, p *Pair) error {
	if p.Nonce == "" || p.CodeChallenge == "" {
		return errors.New("store: nonce and codeChallenge are required")
	}
	if p.UserCode == "" {
		return errors.New("store: userCode is required")
	}
	now := time.Now()
	if p.Status == "" {
		p.Status = PairStatusPending
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = now.Unix()
	}
	if p.TTL == 0 {
		p.TTL = now.Add(pairTTL).Unix()
	}
	it := struct {
		PK string `dynamodbav:"pk"`
		SK string `dynamodbav:"sk"`
		Pair
	}{
		PK:   pairPK(p.Nonce),
		SK:   skPair,
		Pair: *p,
	}
	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return fmt.Errorf("store: marshal pair: %w", err)
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
		return fmt.Errorf("store: create pair: %w", err)
	}
	return nil
}

// GetPair fetches a PAIR#<nonce> row. Returns (nil, nil) when absent or
// past its TTL (unreaped rows are treated as gone).
func (s *Store) GetPair(ctx context.Context, nonce string) (*Pair, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            keyOf(pairPK(nonce), skPair),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("store: get pair: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var p Pair
	if err := attributevalue.UnmarshalMap(out.Item, &p); err != nil {
		return nil, fmt.Errorf("store: unmarshal pair: %w", err)
	}
	if p.TTL > 0 && p.TTL < time.Now().Unix() {
		return nil, nil
	}
	p.Nonce = nonce
	return &p, nil
}

// UpdatePair advances the pairing state machine (pending → bound →
// claimed), conditionally on the row existing, currently holding
// expectStatus, and not being past its TTL — so two racing
// binders/claimers cannot both win a transition. deviceID/userID are set
// when non-empty (the bind leg attaches both; the claim leg passes
// empty). Returns ErrInvalidPairState when the conditional check loses
// (callers map it to their "already claimed" error).
func (s *Store) UpdatePair(ctx context.Context, nonce, expectStatus, newStatus, deviceID, userID string) error {
	if nonce == "" || expectStatus == "" || newStatus == "" {
		return errors.New("store: nonce, expectStatus and newStatus are required")
	}
	update := "SET #st = :new"
	names := map[string]string{
		"#st":  "status",
		"#ttl": "ttl",
	}
	values := map[string]types.AttributeValue{
		":new":    &types.AttributeValueMemberS{Value: newStatus},
		":expect": &types.AttributeValueMemberS{Value: expectStatus},
		":now":    &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", time.Now().Unix())},
	}
	if deviceID != "" {
		update += ", #did = :did"
		names["#did"] = "deviceId"
		values[":did"] = &types.AttributeValueMemberS{Value: deviceID}
	}
	if userID != "" {
		update += ", #uid = :uid"
		names["#uid"] = "userId"
		values[":uid"] = &types.AttributeValueMemberS{Value: userID}
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.table),
		Key:                       keyOf(pairPK(nonce), skPair),
		UpdateExpression:          aws.String(update),
		ConditionExpression:       aws.String("attribute_exists(pk) AND #st = :expect AND #ttl > :now"),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrInvalidPairState
		}
		return fmt.Errorf("store: update pair: %w", err)
	}
	return nil
}

// IncrementPairAttempts atomically bumps codeAttempts on a still-pending,
// unexpired PAIR row (one wrong user-code entry). Conditional on status =
// pending so attempts can never accrue on a bound/claimed/failed pairing;
// returns ErrInvalidPairState when that condition loses. The caller reads
// the row back to learn the new count (and flips pending → failed via
// UpdatePair once the max is reached).
func (s *Store) IncrementPairAttempts(ctx context.Context, nonce string) error {
	if nonce == "" {
		return errors.New("store: nonce is required")
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.table),
		Key:                 keyOf(pairPK(nonce), skPair),
		UpdateExpression:    aws.String("ADD #att :one"),
		ConditionExpression: aws.String("attribute_exists(pk) AND #st = :pending AND #ttl > :now"),
		ExpressionAttributeNames: map[string]string{
			"#att": "codeAttempts",
			"#st":  "status",
			"#ttl": "ttl",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one":     &types.AttributeValueMemberN{Value: "1"},
			":pending": &types.AttributeValueMemberS{Value: PairStatusPending},
			":now":     &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", time.Now().Unix())},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrInvalidPairState
		}
		return fmt.Errorf("store: increment pair attempts: %w", err)
	}
	return nil
}

// PutPairConfirm stores the PAIRCONFIRM#<token> row linking a completed
// LWA sign-in to the pending user-code confirm form. Conditional on the
// token not existing (tokens are single-use random values); fills
// CreatedAt/TTL (now+10min) if unset.
func (s *Store) PutPairConfirm(ctx context.Context, pc *PairConfirm) error {
	if pc.Token == "" || pc.Nonce == "" || pc.AmazonUserID == "" {
		return errors.New("store: token, nonce and amazonUserId are required")
	}
	now := time.Now()
	if pc.CreatedAt == 0 {
		pc.CreatedAt = now.Unix()
	}
	if pc.TTL == 0 {
		pc.TTL = now.Add(pairConfirmTTL).Unix()
	}
	it := struct {
		PK string `dynamodbav:"pk"`
		SK string `dynamodbav:"sk"`
		PairConfirm
	}{
		PK:          pairConfirmPK(pc.Token),
		SK:          skConfirm,
		PairConfirm: *pc,
	}
	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return fmt.Errorf("store: marshal pair confirm: %w", err)
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
		return fmt.Errorf("store: put pair confirm: %w", err)
	}
	return nil
}

// GetPairConfirm fetches a PAIRCONFIRM#<token> row. NOT consumed on read —
// the confirm form allows several wrong-code retries against the same
// token; the caller deletes it (DeletePairConfirm) on success or terminal
// failure. Returns (nil, nil) when absent or past its TTL.
func (s *Store) GetPairConfirm(ctx context.Context, token string) (*PairConfirm, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            keyOf(pairConfirmPK(token), skConfirm),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("store: get pair confirm: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var pc PairConfirm
	if err := attributevalue.UnmarshalMap(out.Item, &pc); err != nil {
		return nil, fmt.Errorf("store: unmarshal pair confirm: %w", err)
	}
	if pc.TTL > 0 && pc.TTL < time.Now().Unix() {
		return nil, nil
	}
	pc.Token = token
	return &pc, nil
}

// DeletePairConfirm removes a PAIRCONFIRM#<token> row (successful bind or
// terminal pairing failure). Idempotent — deleting an absent row is fine.
func (s *Store) DeletePairConfirm(ctx context.Context, token string) error {
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       keyOf(pairConfirmPK(token), skConfirm),
	})
	if err != nil {
		return fmt.Errorf("store: delete pair confirm: %w", err)
	}
	return nil
}
