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

// sessionItem is the raw USER#<uid>/SESS#<sessionId> row (Session +
// table/GSI keys). GSI1 answers "which session is <sessionId>?" without
// knowing the user; GSI2 is the per-user active-session feed ordered by
// lastUsedAt.
type sessionItem struct {
	PK     string `dynamodbav:"pk"`
	SK     string `dynamodbav:"sk"`
	Gsi1PK string `dynamodbav:"gsi1pk"`
	Gsi1SK string `dynamodbav:"gsi1sk"`
	Gsi2PK string `dynamodbav:"gsi2pk"`
	Gsi2SK string `dynamodbav:"gsi2sk"`
	Session
}

func sessionToItem(sess *Session) sessionItem {
	return sessionItem{
		PK:      userPK(sess.UserID),
		SK:      sessSK(sess.SessionID),
		Gsi1PK:  sessGSI1PK(sess.SessionID),
		Gsi1SK:  "SESS",
		Gsi2PK:  sessGSI2PK(sess.UserID),
		Gsi2SK:  time.Unix(sess.LastUsedAt, 0).UTC().Format(time.RFC3339),
		Session: *sess,
	}
}

// CreateSession writes a new session row. Conditional on the key not
// existing (sessionId is a fresh random id; a collision means a bug or an
// attack, not an upsert). Fills CreatedAt/LastUsedAt if unset.
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	if sess.SessionID == "" || sess.UserID == "" || sess.FamilyID == "" || sess.RefreshHash == "" {
		return errors.New("store: sessionID, userID, familyID and refreshHash are required")
	}
	if sess.CreatedAt == 0 {
		sess.CreatedAt = time.Now().Unix()
	}
	if sess.LastUsedAt == 0 {
		sess.LastUsedAt = sess.CreatedAt
	}
	av, err := attributevalue.MarshalMap(sessionToItem(sess))
	if err != nil {
		return fmt.Errorf("store: marshal session: %w", err)
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
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// GetSessionByID resolves a session by its id alone via GSI1
// (gsi1pk=SESS#<sessionId>). Returns (nil, nil) when absent. Note GSI
// reads are eventually consistent — RotateRefresh's conditional
// transaction (not this read) is what guarantees rotate-exactly-once.
func (s *Store) GetSessionByID(ctx context.Context, sessionID string) (*Session, error) {
	out, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		IndexName:              aws.String(indexGSI1),
		KeyConditionExpression: aws.String("#g1pk = :pk AND #g1sk = :sk"),
		ExpressionAttributeNames: map[string]string{
			"#g1pk": "gsi1pk",
			"#g1sk": "gsi1sk",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: sessGSI1PK(sessionID)},
			":sk": &types.AttributeValueMemberS{Value: "SESS"},
		},
		Limit: aws.Int32(1),
	})
	if err != nil {
		return nil, fmt.Errorf("store: query session by id: %w", err)
	}
	if len(out.Items) == 0 {
		return nil, nil
	}
	var it sessionItem
	if err := attributevalue.UnmarshalMap(out.Items[0], &it); err != nil {
		return nil, fmt.Errorf("store: unmarshal session: %w", err)
	}
	sess := it.Session
	return &sess, nil
}

// getSessionConsistent reads the session row from the base table with
// ConsistentRead — used to adjudicate a lost rotate race without GSI lag.
func (s *Store) getSessionConsistent(ctx context.Context, userID, sessionID string) (*Session, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            keyOf(userPK(userID), sessSK(sessionID)),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("store: get session: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var it sessionItem
	if err := attributevalue.UnmarshalMap(out.Item, &it); err != nil {
		return nil, fmt.Errorf("store: unmarshal session: %w", err)
	}
	sess := it.Session
	return &sess, nil
}

// RotateRefresh is the rotate-on-use refresh-token state machine:
//
//   - presentedHash == current refreshHash → rotate atomically
//     (TransactWriteItems Update conditioned on refreshHash still being
//     the presented value): refreshHash=newHash, prevHash=presentedHash,
//     expiresAt/ttl slide to slideTo, lastUsedAt (and the GSI2 feed key)
//     bump to now. Returns the updated session.
//   - presentedHash == prevHash → REUSE DETECTED: an already-rotated
//     token is being replayed (theft, or a client that lost the rotation
//     response and a thief used it first). The entire session family is
//     revoked and ErrRefreshReuse is returned — the caller fires the
//     security alert.
//   - anything else → ErrInvalidRefresh.
//
// A concurrent double-spend of the same token (two requests racing) is
// resolved by the transaction's condition: the loser re-reads with
// ConsistentRead and lands in the prevHash branch → family revoke. That
// is deliberate (strict reuse posture per plan.md M1).
func (s *Store) RotateRefresh(ctx context.Context, sess *Session, presentedHash, newHash string, slideTo int64) (*Session, error) {
	if sess == nil {
		return nil, ErrInvalidRefresh
	}
	if presentedHash == "" || newHash == "" {
		return nil, ErrInvalidRefresh
	}

	// Reuse visible on the copy we were handed — revoke before touching
	// anything else.
	if presentedHash != sess.RefreshHash {
		if sess.PrevHash != "" && presentedHash == sess.PrevHash {
			if err := s.RevokeFamily(ctx, sess.UserID, sess.FamilyID); err != nil {
				return nil, fmt.Errorf("store: revoke family after reuse: %w", err)
			}
			return nil, ErrRefreshReuse
		}
		return nil, ErrInvalidRefresh
	}

	now := time.Now()
	_, err := s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{
				Update: &types.Update{
					TableName: aws.String(s.table),
					Key:       keyOf(userPK(sess.UserID), sessSK(sess.SessionID)),
					UpdateExpression: aws.String(
						"SET #rh = :new, #ph = :presented, #exp = :slide, #ttl = :slide, #lu = :now, #g2sk = :nowts"),
					ConditionExpression: aws.String("attribute_exists(pk) AND #rh = :presented"),
					ExpressionAttributeNames: map[string]string{
						"#rh":   "refreshHash",
						"#ph":   "prevHash",
						"#exp":  "expiresAt",
						"#ttl":  "ttl",
						"#lu":   "lastUsedAt",
						"#g2sk": "gsi2sk",
					},
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":new":       &types.AttributeValueMemberS{Value: newHash},
						":presented": &types.AttributeValueMemberS{Value: presentedHash},
						":slide":     &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", slideTo)},
						":now":       &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now.Unix())},
						":nowts":     &types.AttributeValueMemberS{Value: now.UTC().Format(time.RFC3339)},
					},
				},
			},
		},
	})
	if err != nil {
		var canceled *types.TransactionCanceledException
		if errors.As(err, &canceled) && transactionConditionFailed(canceled) {
			// Lost a race (or the handed-in copy was stale). Adjudicate
			// against a strongly consistent read of the base table.
			fresh, rerr := s.getSessionConsistent(ctx, sess.UserID, sess.SessionID)
			if rerr != nil {
				return nil, rerr
			}
			if fresh != nil && fresh.PrevHash != "" && presentedHash == fresh.PrevHash {
				if verr := s.RevokeFamily(ctx, fresh.UserID, fresh.FamilyID); verr != nil {
					return nil, fmt.Errorf("store: revoke family after reuse: %w", verr)
				}
				return nil, ErrRefreshReuse
			}
			return nil, ErrInvalidRefresh
		}
		return nil, fmt.Errorf("store: rotate refresh: %w", err)
	}

	updated := *sess
	updated.PrevHash = presentedHash
	updated.RefreshHash = newHash
	updated.ExpiresAt = slideTo
	updated.TTL = slideTo
	updated.LastUsedAt = now.Unix()
	return &updated, nil
}

// transactionConditionFailed reports whether any cancellation reason of a
// canceled transaction was a ConditionalCheckFailed.
func transactionConditionFailed(c *types.TransactionCanceledException) bool {
	for _, r := range c.CancellationReasons {
		if r.Code != nil && *r.Code == "ConditionalCheckFailed" {
			return true
		}
	}
	return false
}

// RevokeSession deletes one session row (logout). Idempotent — deleting
// an absent session is a no-op.
func (s *Store) RevokeSession(ctx context.Context, userID, sessionID string) error {
	if _, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       keyOf(userPK(userID), sessSK(sessionID)),
	}); err != nil {
		return fmt.Errorf("store: revoke session: %w", err)
	}
	return nil
}

// listSessionItems queries every SESS# row in the user's partition
// (single-partition Query, never a Scan), optionally filtered by family.
func (s *Store) listSessionItems(ctx context.Context, userID, familyID string) ([]sessionItem, error) {
	in := &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: "SESS#"},
		},
	}
	if familyID != "" {
		in.FilterExpression = aws.String("#fid = :fid")
		in.ExpressionAttributeNames = map[string]string{"#fid": "familyId"}
		in.ExpressionAttributeValues[":fid"] = &types.AttributeValueMemberS{Value: familyID}
	}
	raw, err := s.queryAllPages(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	items := make([]sessionItem, 0, len(raw))
	for _, r := range raw {
		var it sessionItem
		if err := attributevalue.UnmarshalMap(r, &it); err != nil {
			return nil, fmt.Errorf("store: unmarshal session: %w", err)
		}
		items = append(items, it)
	}
	return items, nil
}

// deleteSessions deletes the given session rows one by one (session
// counts per user are tiny; no BatchWriteItem unprocessed-item handling
// needed).
func (s *Store) deleteSessions(ctx context.Context, items []sessionItem) error {
	for _, it := range items {
		if _, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(s.table),
			Key:       keyOf(it.PK, it.SK),
		}); err != nil {
			return fmt.Errorf("store: delete session %s: %w", it.SessionID, err)
		}
	}
	return nil
}

// RevokeFamily deletes every session in a refresh-token family (the blast
// radius of a detected reuse): Query the user's SESS# rows filtered by
// familyId, then delete each.
func (s *Store) RevokeFamily(ctx context.Context, userID, familyID string) error {
	items, err := s.listSessionItems(ctx, userID, familyID)
	if err != nil {
		return err
	}
	return s.deleteSessions(ctx, items)
}

// RevokeAllForUser deletes every session row for the user ("log out
// everywhere" — callers also bump tokensValidAfter so outstanding JWTs
// die within the authorizer cache window).
func (s *Store) RevokeAllForUser(ctx context.Context, userID string) error {
	items, err := s.listSessionItems(ctx, userID, "")
	if err != nil {
		return err
	}
	return s.deleteSessions(ctx, items)
}

// ListSessions returns the user's sessions most-recently-used first via
// the GSI2 feed (gsi2pk=USER#<uid>#SESS ordered by lastUsedAt).
func (s *Store) ListSessions(ctx context.Context, userID string) ([]Session, error) {
	raw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		IndexName:              aws.String(indexGSI2),
		KeyConditionExpression: aws.String("#g2pk = :pk"),
		ExpressionAttributeNames: map[string]string{
			"#g2pk": "gsi2pk",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: sessGSI2PK(userID)},
		},
		ScanIndexForward: aws.Bool(false),
	})
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	sessions := make([]Session, 0, len(raw))
	for _, r := range raw {
		var it sessionItem
		if err := attributevalue.UnmarshalMap(r, &it); err != nil {
			return nil, fmt.Errorf("store: unmarshal session: %w", err)
		}
		sessions = append(sessions, it.Session)
	}
	return sessions, nil
}
