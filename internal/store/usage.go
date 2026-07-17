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

// Token-bucket parameters for the realtime-session mint gate
// (contracts/metering.md): capacity 3, 1 token refilled per 5 seconds.
const (
	mintBucketCapacity   = 3.0
	mintBucketRefillSecs = 5.0
)

// Log retention and active-user-marker retention.
const (
	logTTL          = 90 * 24 * time.Hour
	activeMarkerTTL = 48 * time.Hour
)

// Usage is the USER#<uid>/USAGE#<period> counter row. Period is either a
// month ("2006-01" → monthTokens/monthSeconds) or a day ("2006-01-02" →
// dayTokens/daySeconds/dayMints); the unused fields are zero.
type Usage struct {
	Period       string `dynamodbav:"-"`
	MonthTokens  int64  `dynamodbav:"monthTokens,omitempty"`
	MonthSeconds int64  `dynamodbav:"monthSeconds,omitempty"`
	DayTokens    int64  `dynamodbav:"dayTokens,omitempty"`
	DaySeconds   int64  `dynamodbav:"daySeconds,omitempty"`
	DayMints     int64  `dynamodbav:"dayMints,omitempty"`
	UpdatedAt    string `dynamodbav:"updatedAt,omitempty"`
}

// LogTurn is one USER#<uid>/LOG#<sessionId>#<seq %06d> transcript row
// (TTL 90 days). Seq 0 is the broker's session-start ledger marker.
type LogTurn struct {
	UserID    string `dynamodbav:"userId"`
	SessionID string `dynamodbav:"sessionId"`
	Seq       int    `dynamodbav:"seq"`
	Role      string `dynamodbav:"role"` // user | assistant | tool
	Text      string `dynamodbav:"text"`
	Surface   string `dynamodbav:"surface"`
	Engine    string `dynamodbav:"engine"`
	TS        string `dynamodbav:"ts"` // RFC3339
}

// Note is one USER#<uid>/NOTE#<noteId> row (remember_note/recall_note).
type Note struct {
	NoteID    string   `dynamodbav:"-"`
	Text      string   `dynamodbav:"text"`
	Tags      []string `dynamodbav:"tags,omitempty"`
	CreatedAt string   `dynamodbav:"createdAt"` // RFC3339
}

// MonthPeriod / DayPeriod format a time into the USAGE# period strings.
func MonthPeriod(t time.Time) string { return t.UTC().Format("2006-01") }
func DayPeriod(t time.Time) string   { return t.UTC().Format("2006-01-02") }

// GetUsage fetches USER#<uid>/USAGE#<period>. Returns (nil, nil) when no
// counter row exists yet (treat as zero usage).
func (s *Store) GetUsage(ctx context.Context, userID, period string) (*Usage, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       keyOf(userPK(userID), usageSK(period)),
	})
	if err != nil {
		return nil, fmt.Errorf("store: get usage: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var u Usage
	if err := attributevalue.UnmarshalMap(out.Item, &u); err != nil {
		return nil, fmt.Errorf("store: unmarshal usage: %w", err)
	}
	u.Period = period
	return &u, nil
}

// AddDayUsage atomically increments the day counters (UpdateItem ADD —
// never read-modify-write) on USER#<uid>/USAGE#<day> (day "2006-01-02").
// Creates the row on first touch. Pass zeros for counters you are not
// bumping (e.g. BumpDayMints uses mints=1 only).
func (s *Store) AddDayUsage(ctx context.Context, userID, day string, tokens, seconds, mints int64) error {
	return s.addUsage(ctx, userID, day, map[string]int64{
		"dayTokens":  tokens,
		"daySeconds": seconds,
		"dayMints":   mints,
	})
}

// AddMonthUsage atomically increments the month counters on
// USER#<uid>/USAGE#<month> (month "2006-01").
func (s *Store) AddMonthUsage(ctx context.Context, userID, month string, tokens, seconds int64) error {
	return s.addUsage(ctx, userID, month, map[string]int64{
		"monthTokens":  tokens,
		"monthSeconds": seconds,
	})
}

// BumpDayMints increments today's dayMints counter by one (called by the
// realtime broker on every successful ephemeral-token mint).
func (s *Store) BumpDayMints(ctx context.Context, userID string) error {
	return s.AddDayUsage(ctx, userID, DayPeriod(time.Now()), 0, 0, 1)
}

func (s *Store) addUsage(ctx context.Context, userID, period string, adds map[string]int64) error {
	names := map[string]string{"#ua": "updatedAt"}
	values := map[string]types.AttributeValue{
		":now": &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
	}
	var terms []string
	i := 0
	for attr, v := range adds {
		if v == 0 {
			continue
		}
		n := fmt.Sprintf("#a%d", i)
		p := fmt.Sprintf(":a%d", i)
		names[n] = attr
		values[p] = &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", v)}
		terms = append(terms, n+" "+p)
		i++
	}
	expr := "SET #ua = :now"
	if len(terms) > 0 {
		expr += " ADD " + strings.Join(terms, ", ")
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.table),
		Key:                       keyOf(userPK(userID), usageSK(period)),
		UpdateExpression:          aws.String(expr),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	if err != nil {
		return fmt.Errorf("store: add usage %s: %w", period, err)
	}
	return nil
}

// SetUsageTotals overwrites the counter row with recomputed absolute
// totals — the hourly usage-rollup's write path (it re-Queries the LOG#
// items and SETs the sums, per contracts/metering.md). A 7-char period
// ("2006-01") sets the month fields, a 10-char period ("2006-01-02") sets
// the day token/second fields (dayMints is broker-owned and untouched).
func (s *Store) SetUsageTotals(ctx context.Context, userID, period string, tokens, seconds int64) error {
	var tokAttr, secAttr string
	switch len(period) {
	case len("2006-01"):
		tokAttr, secAttr = "monthTokens", "monthSeconds"
	case len("2006-01-02"):
		tokAttr, secAttr = "dayTokens", "daySeconds"
	default:
		return fmt.Errorf("store: invalid usage period %q", period)
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(s.table),
		Key:              keyOf(userPK(userID), usageSK(period)),
		UpdateExpression: aws.String("SET #tok = :tok, #sec = :sec, #ua = :now"),
		ExpressionAttributeNames: map[string]string{
			"#tok": tokAttr,
			"#sec": secAttr,
			"#ua":  "updatedAt",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":tok": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", tokens)},
			":sec": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", seconds)},
			":now": &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
		},
	})
	if err != nil {
		return fmt.Errorf("store: set usage totals %s: %w", period, err)
	}
	return nil
}

// mintBucket is the USER#<uid>/BUCKET#mint token-bucket row.
type mintBucket struct {
	Tokens     float64 `dynamodbav:"tokens"`
	LastRefill int64   `dynamodbav:"lastRefill"` // unix seconds
}

// TakeMintToken implements the mint rate limiter (1 token / 5s, burst 3)
// from contracts/metering.md. Returns (true, nil) when a token was
// consumed and the mint may proceed, (false, nil) when the bucket is
// empty (caller responds 429 with Retry-After 5). Concurrency-safe: the
// refill computation is applied via a conditional UpdateItem CAS on the
// exact (tokens, lastRefill) pair that was read — a lost race retries,
// never double-spends.
func (s *Store) TakeMintToken(ctx context.Context, userID string) (bool, error) {
	key := keyOf(userPK(userID), skBucketMint)
	for attempt := 0; attempt < 4; attempt++ {
		now := time.Now().Unix()
		out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
			TableName:      aws.String(s.table),
			Key:            key,
			ConsistentRead: aws.Bool(true),
		})
		if err != nil {
			return false, fmt.Errorf("store: get mint bucket: %w", err)
		}

		if out.Item == nil {
			// First mint ever: create the bucket full-minus-one.
			_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
				TableName: aws.String(s.table),
				Item: map[string]types.AttributeValue{
					"pk":         &types.AttributeValueMemberS{Value: userPK(userID)},
					"sk":         &types.AttributeValueMemberS{Value: skBucketMint},
					"tokens":     &types.AttributeValueMemberN{Value: formatFloat(mintBucketCapacity - 1)},
					"lastRefill": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now)},
				},
				ConditionExpression: aws.String("attribute_not_exists(pk)"),
			})
			if err != nil {
				var cond *types.ConditionalCheckFailedException
				if errors.As(err, &cond) {
					continue // lost the creation race — re-read
				}
				return false, fmt.Errorf("store: create mint bucket: %w", err)
			}
			return true, nil
		}

		var b mintBucket
		if err := attributevalue.UnmarshalMap(out.Item, &b); err != nil {
			return false, fmt.Errorf("store: unmarshal mint bucket: %w", err)
		}

		avail := b.Tokens + float64(now-b.LastRefill)/mintBucketRefillSecs
		if avail > mintBucketCapacity {
			avail = mintBucketCapacity
		}
		if avail < 1 {
			return false, nil // 429 — no write, no queuing
		}

		// CAS: only spend if the row is exactly as read.
		_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:           aws.String(s.table),
			Key:                 key,
			UpdateExpression:    aws.String("SET #tok = :new, #lr = :now"),
			ConditionExpression: aws.String("#tok = :old AND #lr = :oldlr"),
			ExpressionAttributeNames: map[string]string{
				"#tok": "tokens",
				"#lr":  "lastRefill",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":new":   &types.AttributeValueMemberN{Value: formatFloat(avail - 1)},
				":now":   &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now)},
				":old":   &types.AttributeValueMemberN{Value: formatFloat(b.Tokens)},
				":oldlr": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", b.LastRefill)},
			},
		})
		if err != nil {
			var cond *types.ConditionalCheckFailedException
			if errors.As(err, &cond) {
				continue // concurrent spend — recompute and retry
			}
			return false, fmt.Errorf("store: spend mint token: %w", err)
		}
		return true, nil
	}
	// Persistent contention: fail closed as rate-limited (the caller's
	// 429 tells the client to retry shortly).
	return false, nil
}

func formatFloat(f float64) string {
	return fmt.Sprintf("%g", f)
}

// PutLogTurn writes one transcript/ledger row at
// USER#<uid>/LOG#<sessionId>#<seq %06d> with a 90-day TTL. Overwrites on
// same (session, seq) — retried deliveries of the same turn are
// idempotent by construction.
func (s *Store) PutLogTurn(ctx context.Context, t *LogTurn) error {
	if t.UserID == "" || t.SessionID == "" {
		return errors.New("store: userID and sessionID are required")
	}
	if t.TS == "" {
		t.TS = time.Now().UTC().Format(time.RFC3339)
	}
	it := struct {
		PK  string `dynamodbav:"pk"`
		SK  string `dynamodbav:"sk"`
		TTL int64  `dynamodbav:"ttl"`
		LogTurn
	}{
		PK:      userPK(t.UserID),
		SK:      fmt.Sprintf("LOG#%s#%06d", t.SessionID, t.Seq),
		TTL:     time.Now().Add(logTTL).Unix(),
		LogTurn: *t,
	}
	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return fmt.Errorf("store: marshal log turn: %w", err)
	}
	if _, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	}); err != nil {
		return fmt.Errorf("store: put log turn: %w", err)
	}
	return nil
}

// QuerySessionLog returns a session's transcript rows in seq order
// (Query on sk begins_with LOG#<sessionId>#).
func (s *Store) QuerySessionLog(ctx context.Context, userID, sessionID string) ([]LogTurn, error) {
	return s.queryLogTurns(ctx, userID, "LOG#"+sessionID+"#", "")
}

// QueryLogTurnsSince returns the user's transcript rows with ts >=
// sinceTS (RFC3339) across all sessions — the usage-rollup's read path.
// Single-partition Query with a ts FilterExpression; the LOG# range is
// TTL-bounded to 90 days for a single user, so the read stays small.
func (s *Store) QueryLogTurnsSince(ctx context.Context, userID, sinceTS string) ([]LogTurn, error) {
	return s.queryLogTurns(ctx, userID, "LOG#", sinceTS)
}

func (s *Store) queryLogTurns(ctx context.Context, userID, skPrefix, sinceTS string) ([]LogTurn, error) {
	in := &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: skPrefix},
		},
	}
	if sinceTS != "" {
		in.FilterExpression = aws.String("#ts >= :since")
		in.ExpressionAttributeNames = map[string]string{"#ts": "ts"}
		in.ExpressionAttributeValues[":since"] = &types.AttributeValueMemberS{Value: sinceTS}
	}
	raw, err := s.queryAllPages(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("store: query log turns: %w", err)
	}
	turns := make([]LogTurn, 0, len(raw))
	for _, r := range raw {
		var t LogTurn
		if err := attributevalue.UnmarshalMap(r, &t); err != nil {
			return nil, fmt.Errorf("store: unmarshal log turn: %w", err)
		}
		turns = append(turns, t)
	}
	return turns, nil
}

// MarkActiveUser writes the CONFIG/ACTIVEUSER#<uid>#<day> marker (48h
// TTL) — the transcript sink calls this so the hourly usage-rollup can
// find today's active users with a Query, never a Scan. Idempotent.
func (s *Store) MarkActiveUser(ctx context.Context, userID, day string) error {
	if userID == "" || day == "" {
		return errors.New("store: userID and day are required")
	}
	if _, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"pk":  &types.AttributeValueMemberS{Value: pkConfig},
			"sk":  &types.AttributeValueMemberS{Value: "ACTIVEUSER#" + userID + "#" + day},
			"ttl": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", time.Now().Add(activeMarkerTTL).Unix())},
		},
	}); err != nil {
		return fmt.Errorf("store: mark active user: %w", err)
	}
	return nil
}

// ListActiveUsers returns the userIds marked active for the given day
// ("2006-01-02"). Queries the CONFIG partition's ACTIVEUSER# range (kept
// tiny by the 48h TTL) and filters the day suffix in code — sk carries
// uid before day, so the day match can't be a key condition.
func (s *Store) ListActiveUsers(ctx context.Context, day string) ([]string, error) {
	raw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: pkConfig},
			":pfx": &types.AttributeValueMemberS{Value: "ACTIVEUSER#"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list active users: %w", err)
	}
	var users []string
	for _, r := range raw {
		skAttr, ok := r["sk"].(*types.AttributeValueMemberS)
		if !ok {
			continue
		}
		rest := strings.TrimPrefix(skAttr.Value, "ACTIVEUSER#")
		i := strings.LastIndex(rest, "#")
		if i < 0 || rest[i+1:] != day {
			continue
		}
		users = append(users, rest[:i])
	}
	return users, nil
}

// PutNote writes USER#<uid>/NOTE#<noteId> (remember_note tool). Fills
// CreatedAt if unset.
func (s *Store) PutNote(ctx context.Context, userID string, n *Note) error {
	if userID == "" || n.NoteID == "" {
		return errors.New("store: userID and noteID are required")
	}
	if n.CreatedAt == "" {
		n.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	it := struct {
		PK string `dynamodbav:"pk"`
		SK string `dynamodbav:"sk"`
		Note
	}{
		PK:   userPK(userID),
		SK:   noteSK(n.NoteID),
		Note: *n,
	}
	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return fmt.Errorf("store: marshal note: %w", err)
	}
	if _, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	}); err != nil {
		return fmt.Errorf("store: put note: %w", err)
	}
	return nil
}

// QueryNotes returns the user's notes, optionally filtered by a
// case-insensitive substring matched against text and tags (recall_note
// tool). Single-partition Query on sk begins_with NOTE#; the substring
// filter runs in code per the dictated design — never a Scan.
func (s *Store) QueryNotes(ctx context.Context, userID, substr string) ([]Note, error) {
	raw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: "NOTE#"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: query notes: %w", err)
	}
	needle := strings.ToLower(substr)
	notes := make([]Note, 0, len(raw))
	for _, r := range raw {
		var it struct {
			SK string `dynamodbav:"sk"`
			Note
		}
		if err := attributevalue.UnmarshalMap(r, &it); err != nil {
			return nil, fmt.Errorf("store: unmarshal note: %w", err)
		}
		it.Note.NoteID = strings.TrimPrefix(it.SK, "NOTE#")
		if needle != "" && !noteMatches(&it.Note, needle) {
			continue
		}
		notes = append(notes, it.Note)
	}
	return notes, nil
}

func noteMatches(n *Note, lowerNeedle string) bool {
	if strings.Contains(strings.ToLower(n.Text), lowerNeedle) {
		return true
	}
	for _, tag := range n.Tags {
		if strings.Contains(strings.ToLower(tag), lowerNeedle) {
			return true
		}
	}
	return false
}
