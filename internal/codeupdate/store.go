package codeupdate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// The CODEUPD# store — TWO rows per request, because two callers reach it with
// two different things in hand.
//
//	USER#<userId> / CODEUPD#<requestId>     the status record
//	CODEUPD#<requestId> / TOKEN             the run-token row
//
// The STATUS RECORD lives in the owner's partition, so a request is only ever
// readable by the account that asked for it, "my recent updates" is a
// single-partition Query on the CODEUPD# prefix (never a Scan), and the
// account-purge worker sweeps it with everything else in that partition.
//
// The TOKEN ROW exists because the coding agent on the node knows only the run
// token — it has no idea which account requested the work, and must not need
// one, since a user id in a prompt on a remote machine is an identifier leaked
// for no gain. So the token row is keyed by request id alone and carries just
// enough to answer "is this token good, and how many posts has it spent": the
// owning user id, the secret's hash, and the counter. It is NOT in a user
// partition and therefore not swept by account purge — its 24 h TTL is what
// retires it, which is also exactly when the token should stop working.
//
// Splitting them is what keeps the token row from becoming a second, unguarded
// copy of the request: it holds no prompt, no instructions and no run detail.

// ErrNotFound is returned when no row exists for a request id.
var ErrNotFound = errors.New("codeupdate: request not found")

// ErrPostLimit is returned when a run has already sent MaxProgressPosts
// progress reports.
var ErrPostLimit = errors.New("codeupdate: progress post limit reached")

// DDB is the DynamoDB surface this store needs. A *dynamodb.Client satisfies
// it; tests inject a fake.
type DDB interface {
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// Store reads and writes CODEUPD# rows.
type Store struct {
	ddb   DDB
	table string
	now   func() time.Time
}

// NewStore builds a Store. now defaults to time.Now.
func NewStore(ddb DDB, table string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{ddb: ddb, table: table, now: now}
}

func userPK(userID string) string { return "USER#" + userID }

// Put writes (or replaces) a record.
func (s *Store) Put(ctx context.Context, rec Record) error {
	now := s.now().UTC()
	if rec.CreatedAt == "" {
		rec.CreatedAt = now.Format(time.RFC3339)
	}
	rec.UpdatedAt = now.Format(time.RFC3339)

	item := map[string]ddbtypes.AttributeValue{
		"pk":        &ddbtypes.AttributeValueMemberS{Value: userPK(rec.UserID)},
		"sk":        &ddbtypes.AttributeValueMemberS{Value: SortKey(rec.RequestID)},
		"requestId": &ddbtypes.AttributeValueMemberS{Value: rec.RequestID},
		"userId":    &ddbtypes.AttributeValueMemberS{Value: rec.UserID},
		"status":    &ddbtypes.AttributeValueMemberS{Value: rec.Status},
		"repo":      &ddbtypes.AttributeValueMemberS{Value: rec.Repo},
		"node":      &ddbtypes.AttributeValueMemberS{Value: rec.Node},
		"cli":       &ddbtypes.AttributeValueMemberS{Value: rec.CLI},
		"deploy":    &ddbtypes.AttributeValueMemberBOOL{Value: rec.Deploy},
		"rewritten": &ddbtypes.AttributeValueMemberBOOL{Value: rec.Rewritten},
		"createdAt": &ddbtypes.AttributeValueMemberS{Value: rec.CreatedAt},
		"updatedAt": &ddbtypes.AttributeValueMemberS{Value: rec.UpdatedAt},
		"ttl":       &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now.Add(RecordTTL).Unix(), 10)},
	}
	for k, v := range map[string]string{
		"model":       rec.Model,
		"rewriteNote": rec.RewriteNote,
		"eventId":     rec.EventID,
		"runId":       rec.RunID,
		"error":       rec.Error,
	} {
		if v != "" {
			item[k] = &ddbtypes.AttributeValueMemberS{Value: v}
		}
	}

	if _, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      item,
	}); err != nil {
		return fmt.Errorf("codeupdate: put record: %w", err)
	}
	return nil
}

// Get reads one record.
func (s *Store) Get(ctx context.Context, userID, requestID string) (Record, error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &ddbtypes.AttributeValueMemberS{Value: SortKey(requestID)},
		},
	})
	if err != nil {
		return Record{}, fmt.Errorf("codeupdate: get record: %w", err)
	}
	if len(out.Item) == 0 {
		return Record{}, ErrNotFound
	}
	return recordFromItem(out.Item), nil
}

// Latest returns the caller's most recent record, newest first. It is a
// single-partition Query on the CODEUPD# prefix — never a Scan. Request ids are
// UUIDv7, so lexical order on the sort key IS time order, and the newest row is
// simply the last one.
func (s *Store) Latest(ctx context.Context, userID string) (Record, error) {
	out, err := s.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :prefix)"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":pk":     &ddbtypes.AttributeValueMemberS{Value: userPK(userID)},
			":prefix": &ddbtypes.AttributeValueMemberS{Value: "CODEUPD#"},
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(1),
	})
	if err != nil {
		return Record{}, fmt.Errorf("codeupdate: query latest: %w", err)
	}
	if len(out.Items) == 0 {
		return Record{}, ErrNotFound
	}
	return recordFromItem(out.Items[0]), nil
}

// SetStatus advances a record's status, optionally recording the launch result
// or a failure reason. Fields left empty are not written, so a later update
// cannot blank an earlier one.
type StatusUpdate struct {
	Status      string
	EventID     string
	RunID       string
	Error       string
	Rewritten   *bool
	RewriteNote string
}

// SetStatus applies u to the record.
func (s *Store) SetStatus(ctx context.Context, userID, requestID string, u StatusUpdate) error {
	set := []string{"#s = :s", "updatedAt = :u"}
	names := map[string]string{"#s": "status"}
	values := map[string]ddbtypes.AttributeValue{
		":s": &ddbtypes.AttributeValueMemberS{Value: u.Status},
		":u": &ddbtypes.AttributeValueMemberS{Value: s.now().UTC().Format(time.RFC3339)},
	}
	for expr, val := range map[string]string{
		"eventId":     u.EventID,
		"runId":       u.RunID,
		"rewriteNote": u.RewriteNote,
	} {
		if val == "" {
			continue
		}
		set = append(set, expr+" = :"+expr)
		values[":"+expr] = &ddbtypes.AttributeValueMemberS{Value: val}
	}
	if u.Error != "" {
		// `error` is a DynamoDB reserved word.
		set = append(set, "#e = :e")
		names["#e"] = "error"
		values[":e"] = &ddbtypes.AttributeValueMemberS{Value: u.Error}
	}
	if u.Rewritten != nil {
		set = append(set, "rewritten = :rw")
		values[":rw"] = &ddbtypes.AttributeValueMemberBOOL{Value: *u.Rewritten}
	}

	if _, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &ddbtypes.AttributeValueMemberS{Value: SortKey(requestID)},
		},
		UpdateExpression:          aws.String("SET " + joinComma(set)),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	}); err != nil {
		return fmt.Errorf("codeupdate: set status: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Token row
// ---------------------------------------------------------------------------

// tokenPK / tokenSK key the run-token row. It is deliberately its own
// partition: the node knows the request id and nothing else.
func tokenPK(requestID string) string { return "CODEUPD#" + requestID }

const tokenSK = "TOKEN"

// TokenRow is what the progress endpoint needs to authenticate a post and
// decide where the resulting email goes. Nothing more is stored here.
type TokenRow struct {
	RequestID string
	UserID    string
	Repo      string
	TokenHash string
	PostCount int
}

// ErrTokenExists means a run-token row already exists for this request id.
var ErrTokenExists = errors.New("codeupdate: run token already minted for this request")

// PutToken writes the run-token row. The plaintext secret is never passed in —
// only its hash — so this function cannot leak one even by accident.
//
// The write is CREATE-ONLY. A duplicate SQS delivery re-enters Dispatch and
// would otherwise re-mint: overwriting the hash silently revokes the token the
// still-running session is holding (every subsequent progress post 401s) and
// resets postCount to 0, handing a second full quota of emails to a run that had
// already spent one. Failing the second mint instead is both safer and truthful
// — the caller learns it is a redelivery.
func (s *Store) PutToken(ctx context.Context, row TokenRow) error {
	now := s.now().UTC()
	_, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
		Item: map[string]ddbtypes.AttributeValue{
			"pk":        &ddbtypes.AttributeValueMemberS{Value: tokenPK(row.RequestID)},
			"sk":        &ddbtypes.AttributeValueMemberS{Value: tokenSK},
			"requestId": &ddbtypes.AttributeValueMemberS{Value: row.RequestID},
			"userId":    &ddbtypes.AttributeValueMemberS{Value: row.UserID},
			"repo":      &ddbtypes.AttributeValueMemberS{Value: row.Repo},
			"tokenHash": &ddbtypes.AttributeValueMemberS{Value: row.TokenHash},
			"postCount": &ddbtypes.AttributeValueMemberN{Value: "0"},
			// expiresAt duplicates the ttl attribute as a value the CODE can read.
			// DynamoDB's TTL sweep is best-effort and routinely lags by hours, so a
			// row past its ttl stays readable — and a token that outlives its run
			// must not keep working just because a background deleter is behind.
			"expiresAt": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now.Add(RecordTTL).Unix(), 10)},
			"ttl":       &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now.Add(RecordTTL).Unix(), 10)},
		},
	})
	if err != nil {
		var cond *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrTokenExists
		}
		return fmt.Errorf("codeupdate: put token row: %w", err)
	}
	return nil
}

// GetToken reads the run-token row, treating an expired one as absent.
//
// The expiry is checked HERE, in code, rather than trusted to DynamoDB's TTL
// sweep — that sweep is best-effort and routinely runs hours behind, so a row
// past its TTL remains readable and would keep authenticating a token whose run
// ended yesterday. Returning ErrNotFound makes the caller's uniform 401 cover
// it, so an expired token is indistinguishable from an unknown one.
func (s *Store) GetToken(ctx context.Context, requestID string) (TokenRow, error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: tokenPK(requestID)},
			"sk": &ddbtypes.AttributeValueMemberS{Value: tokenSK},
		},
	})
	if err != nil {
		return TokenRow{}, fmt.Errorf("codeupdate: get token row: %w", err)
	}
	if len(out.Item) == 0 {
		return TokenRow{}, ErrNotFound
	}
	// A row written before expiresAt existed has 0 here; treat that as
	// "no recorded expiry" rather than "expired at the epoch".
	if exp := int64(itemInt(out.Item, "expiresAt")); exp > 0 && s.now().UTC().Unix() >= exp {
		return TokenRow{}, ErrNotFound
	}
	return TokenRow{
		RequestID: itemString(out.Item, "requestId"),
		UserID:    itemString(out.Item, "userId"),
		Repo:      itemString(out.Item, "repo"),
		TokenHash: itemString(out.Item, "tokenHash"),
		PostCount: itemInt(out.Item, "postCount"),
	}, nil
}

// ClaimProgressPost increments the token row's post counter, refusing past
// MaxProgressPosts. The bound is enforced by a CONDITIONAL update rather than a
// read-then-write, so two progress posts racing each other cannot both observe
// "7 so far" and both go through. attribute_exists(sk) additionally makes a
// TTL-expired row a refusal rather than a resurrection.
func (s *Store) ClaimProgressPost(ctx context.Context, requestID string) (int, error) {
	out, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: tokenPK(requestID)},
			"sk": &ddbtypes.AttributeValueMemberS{Value: tokenSK},
		},
		UpdateExpression:    aws.String("SET postCount = postCount + :one"),
		ConditionExpression: aws.String("attribute_exists(sk) AND postCount < :max"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":one": &ddbtypes.AttributeValueMemberN{Value: "1"},
			":max": &ddbtypes.AttributeValueMemberN{Value: strconv.Itoa(MaxProgressPosts)},
		},
		ReturnValues: ddbtypes.ReturnValueAllNew,
	})
	if err != nil {
		var cond *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return 0, ErrPostLimit
		}
		return 0, fmt.Errorf("codeupdate: claim progress post: %w", err)
	}
	return itemInt(out.Attributes, "postCount"), nil
}

func itemString(item map[string]ddbtypes.AttributeValue, k string) string {
	if v, ok := item[k].(*ddbtypes.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func itemInt(item map[string]ddbtypes.AttributeValue, k string) int {
	if v, ok := item[k].(*ddbtypes.AttributeValueMemberN); ok {
		if n, err := strconv.Atoi(v.Value); err == nil {
			return n
		}
	}
	return 0
}

func recordFromItem(item map[string]ddbtypes.AttributeValue) Record {
	str := func(k string) string {
		if v, ok := item[k].(*ddbtypes.AttributeValueMemberS); ok {
			return v.Value
		}
		return ""
	}
	boolean := func(k string) bool {
		if v, ok := item[k].(*ddbtypes.AttributeValueMemberBOOL); ok {
			return v.Value
		}
		return false
	}
	return Record{
		RequestID:   str("requestId"),
		UserID:      str("userId"),
		Status:      str("status"),
		Repo:        str("repo"),
		Node:        str("node"),
		CLI:         str("cli"),
		Model:       str("model"),
		Deploy:      boolean("deploy"),
		Rewritten:   boolean("rewritten"),
		RewriteNote: str("rewriteNote"),
		EventID:     str("eventId"),
		RunID:       str("runId"),
		Error:       str("error"),
		CreatedAt:   str("createdAt"),
		UpdatedAt:   str("updatedAt"),
	}
}

func joinComma(parts []string) string { return strings.Join(parts, ", ") }
