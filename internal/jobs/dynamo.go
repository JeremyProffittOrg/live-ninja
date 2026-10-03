package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type DynamoAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
	UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}
type DynamoStore struct {
	client DynamoAPI
	table  string
}

func NewDynamoStore(ctx context.Context, table string) (*DynamoStore, error) {
	if table == "" {
		return nil, errors.New("jobs: TABLE_NAME is required")
	}
	cfg, e := awsconfig.LoadDefaultConfig(ctx)
	if e != nil {
		return nil, e
	}
	return NewDynamoStoreWithClient(dynamodb.NewFromConfig(cfg), table), nil
}
func NewDynamoStoreWithClient(client DynamoAPI, table string) *DynamoStore {
	return &DynamoStore{client: client, table: table}
}
func avs(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func key(uid, id string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": avs("USER#" + uid), "sk": avs("JOB#" + id)}
}
func decode(item map[string]types.AttributeValue) (*Record, error) {
	p, ok := item["payload"].(*types.AttributeValueMemberS)
	if !ok {
		return nil, ErrCorrupt
	}
	var r Record
	if e := json.Unmarshal([]byte(p.Value), &r); e != nil {
		return nil, ErrCorrupt
	}
	if r.Job.Version < 1 || !validID(r.Job.ID) || r.UserID == "" {
		return nil, ErrCorrupt
	}
	return &r, nil
}
func (s *DynamoStore) Get(ctx context.Context, uid, id string) (*Record, error) {
	o, e := s.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(s.table), Key: key(uid, id), ConsistentRead: aws.Bool(true)})
	if e != nil {
		return nil, e
	}
	if len(o.Item) == 0 {
		return nil, ErrNotFound
	}
	r, e := decode(o.Item)
	if e != nil {
		return nil, e
	}
	if r.UserID != uid || r.Job.ID != id {
		return nil, ErrCorrupt
	}
	return r, nil
}
func (s *DynamoStore) List(ctx context.Context, uid string, limit int, cursor string) ([]Record, string, error) {
	if cursor != "" && !validID(cursor) {
		return nil, "", fmt.Errorf("%w: invalid cursor", ErrValidation)
	}
	in := &dynamodb.QueryInput{TableName: aws.String(s.table), ConsistentRead: aws.Bool(true), KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :prefix)"), ExpressionAttributeValues: map[string]types.AttributeValue{":pk": avs("USER#" + uid), ":prefix": avs("JOB#")}, Limit: aws.Int32(int32(pageLimit(limit)))}
	if cursor != "" {
		in.ExclusiveStartKey = key(uid, cursor)
	}
	out, e := s.client.Query(ctx, in)
	if e != nil {
		return nil, "", e
	}
	records := make([]Record, 0, len(out.Items))
	for _, item := range out.Items {
		r, e := decode(item)
		if e != nil {
			return nil, "", e
		}
		if r.UserID != uid {
			return nil, "", errors.New("jobs: corrupt owner")
		}
		records = append(records, *r)
	}
	nextCursor := ""
	if v, ok := out.LastEvaluatedKey["sk"].(*types.AttributeValueMemberS); ok {
		nextCursor = strings.TrimPrefix(v.Value, "JOB#")
	}
	return records, nextCursor, nil
}
func validID(id string) bool {
	if len(id) != 36 || !strings.HasPrefix(id, "job_") {
		return false
	}
	for _, c := range id[4:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (s *DynamoStore) CompareAndSwap(ctx context.Context, r *Record, expected int64) error {
	return s.write(ctx, r, expected, true)
}
func (s *DynamoStore) write(ctx context.Context, r *Record, expected int64, active bool) error {
	checks, e := s.authorizationChecks(ctx, r.UserID, active)
	if e != nil {
		return e
	}
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if len(b) > 300000 {
		return fmt.Errorf("%w: record storage capacity reached", ErrLimit)
	}
	item := key(r.UserID, r.Job.ID)
	item["payload"] = avs(string(b))
	item["version"] = &types.AttributeValueMemberN{Value: fmt.Sprint(r.Job.Version)}
	if due := dueAt(r); !due.IsZero() {
		item["gsi2pk"] = avs("JOBS_DUE")
		item["gsi2sk"] = avs(indexTime(due) + "#" + r.UserID + "#" + r.Job.ID)
	}
	condition := "attribute_not_exists(pk)"
	var values map[string]types.AttributeValue
	if expected > 0 {
		condition = "version = :version"
		values = map[string]types.AttributeValue{":version": &types.AttributeValueMemberN{Value: fmt.Sprint(expected)}}
	}
	putIndex := len(checks)
	checks = append(checks, types.TransactWriteItem{Put: &types.Put{TableName: aws.String(s.table), Item: item, ConditionExpression: aws.String(condition), ExpressionAttributeValues: values}})
	if active {
		eventWrites, e := s.historyWrites(ctx, r, expected)
		if e != nil {
			return e
		}
		checks = append(checks, eventWrites...)
	}
	// Inactive suspension only changes an existing aggregate. Account purge
	// snapshots USER keys before deleting them; creating an event after that
	// snapshot would leave an orphan even when the job version is fenced.
	_, e = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: checks})
	if e != nil {
		var cancelled *types.TransactionCanceledException
		if errors.As(e, &cancelled) {
			for i, reason := range cancelled.CancellationReasons {
				if aws.ToString(reason.Code) == "ConditionalCheckFailed" {
					if i < putIndex && active {
						return ErrForbidden
					}
					return ErrConflict
				}
			}
		}
	}
	return e
}

func attributeString(item map[string]types.AttributeValue, key string) string {
	if v, ok := item[key].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

// Match existing owner OR allowlist authorization, and fence the chosen grant
// INSIDE the write transaction. A stale JWT or a revocation between read/write
// can never start work; identity changes invalidate the same transaction.
func (s *DynamoStore) authorizationChecks(ctx context.Context, uid string, active bool) ([]types.TransactWriteItem, error) {
	profileKey := map[string]types.AttributeValue{"pk": avs("USER#" + uid), "sk": avs("PROFILE")}
	out, e := s.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(s.table), Key: profileKey, ConsistentRead: aws.Bool(true)})
	if e != nil {
		return nil, e
	}
	status := attributeString(out.Item, "status")
	role := attributeString(out.Item, "role")
	base := &types.ConditionCheck{TableName: aws.String(s.table), Key: profileKey, ConditionExpression: aws.String("attribute_exists(pk) AND #status = :active"), ExpressionAttributeNames: map[string]string{"#status": "status"}, ExpressionAttributeValues: map[string]types.AttributeValue{":active": avs("active")}}
	if status != "active" {
		if active {
			return nil, ErrForbidden
		}
		base.ConditionExpression = aws.String("attribute_not_exists(pk) OR #status <> :active")
		return []types.TransactWriteItem{{ConditionCheck: base}}, nil
	}
	base.ExpressionAttributeNames["#role"] = "role"
	base.ExpressionAttributeValues[":role"] = avs(role)
	base.ConditionExpression = aws.String(aws.ToString(base.ConditionExpression) + " AND #role = :role")
	if role == "owner" {
		if !active {
			return nil, ErrConflict
		}
		return []types.TransactWriteItem{{ConditionCheck: base}}, nil
	}
	for _, field := range []string{"amazonUserId", "email"} {
		name := "#" + field
		base.ExpressionAttributeNames[name] = field
		if value, exists := out.Item[field]; exists {
			base.ExpressionAttributeValues[":"+field] = value
			base.ConditionExpression = aws.String(aws.ToString(base.ConditionExpression) + " AND " + name + " = :" + field)
		} else {
			base.ConditionExpression = aws.String(aws.ToString(base.ConditionExpression) + " AND attribute_not_exists(" + name + ")")
		}
	}
	checks := []types.TransactWriteItem{{ConditionCheck: base}}
	seen := map[string]bool{}
	for _, raw := range []string{attributeString(out.Item, "amazonUserId"), attributeString(out.Item, "email")} {
		grant := strings.TrimSpace(raw)
		if strings.Contains(grant, "@") {
			grant = strings.ToLower(grant)
		}
		if grant == "" || seen[grant] {
			continue
		}
		seen[grant] = true
		grantKey := map[string]types.AttributeValue{"pk": avs("CONFIG"), "sk": avs("ALLOW#" + grant)}
		allowed, e := s.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(s.table), Key: grantKey, ConsistentRead: aws.Bool(true)})
		if e != nil {
			return nil, e
		}
		if len(allowed.Item) > 0 {
			if !active {
				return nil, ErrConflict
			}
			return append(checks[:1], types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{TableName: aws.String(s.table), Key: grantKey, ConditionExpression: aws.String("attribute_exists(pk)")}}), nil
		}
		if !active {
			checks = append(checks, types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{TableName: aws.String(s.table), Key: grantKey, ConditionExpression: aws.String("attribute_not_exists(pk)")}})
		}
	}
	if active {
		return nil, ErrForbidden
	}
	return checks, nil
}
func indexTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func (s *DynamoStore) Due(ctx context.Context, now time.Time, limit int) ([]Ref, error) {
	limit = pageLimit(limit)
	refs := make([]Ref, 0, limit)
	in := &dynamodb.QueryInput{TableName: aws.String(s.table), IndexName: aws.String("GSI2"), KeyConditionExpression: aws.String("gsi2pk = :pk AND gsi2sk <= :now"), ExpressionAttributeValues: map[string]types.AttributeValue{":pk": avs("JOBS_DUE"), ":now": avs(indexTime(now) + "~")}, ProjectionExpression: aws.String("pk, sk"), Limit: aws.Int32(int32(limit))}
	for page := 0; page < 10; page++ {
		out, e := s.client.Query(ctx, in)
		if e != nil {
			return nil, e
		}
		for _, item := range out.Items {
			pk, pok := item["pk"].(*types.AttributeValueMemberS)
			sk, sok := item["sk"].(*types.AttributeValueMemberS)
			if !pok || !sok || !strings.HasPrefix(pk.Value, "USER#") || len(pk.Value) <= 5 || !strings.HasPrefix(sk.Value, "JOB#") || !validID(strings.TrimPrefix(sk.Value, "JOB#")) {
				continue
			}
			refs = append(refs, Ref{UserID: strings.TrimPrefix(pk.Value, "USER#"), ID: strings.TrimPrefix(sk.Value, "JOB#")})
			if len(refs) == limit {
				return refs, nil
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			return refs, nil
		}
		in.ExclusiveStartKey = out.LastEvaluatedKey
		in.Limit = aws.Int32(int32(limit - len(refs)))
	}
	if len(refs) > 0 {
		slog.Warn("jobs due-index recovery budget exhausted; processing healthy rows", "healthyRows", len(refs))
		return refs, nil
	}
	return refs, fmt.Errorf("%w: malformed due-index entries exceeded the ten-page recovery budget", ErrCorrupt)
}

// Quarantine removes only scheduling-index keys, retaining the original corrupt
// payload for recovery. The payload equality and existing-key conditions defeat
// concurrent repair and purge; an Update can never recreate a deleted account row.
func (s *DynamoStore) Quarantine(ctx context.Context, ref Ref) error {
	if ref.UserID == "" || !validID(ref.ID) {
		return ErrValidation
	}
	out, e := s.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(s.table), Key: key(ref.UserID, ref.ID), ConsistentRead: aws.Bool(true)})
	if e != nil {
		return e
	}
	if len(out.Item) == 0 {
		return nil
	}
	r, decodeErr := decode(out.Item)
	if decodeErr == nil && r.UserID == ref.UserID && r.Job.ID == ref.ID {
		return nil
	}
	condition := "attribute_exists(pk) AND attribute_not_exists(#payload)"
	values := map[string]types.AttributeValue{":reason": avs("Malformed job record; original payload retained for operator recovery."), ":at": avs(stamp(time.Now()))}
	if payload, exists := out.Item["payload"]; exists {
		condition = "attribute_exists(pk) AND #payload = :payload"
		values[":payload"] = payload
	}
	_, e = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(s.table), Key: key(ref.UserID, ref.ID), ConditionExpression: aws.String(condition), UpdateExpression: aws.String("SET jobsQuarantineReason = :reason, jobsQuarantinedAt = :at REMOVE gsi2pk, gsi2sk"), ExpressionAttributeNames: map[string]string{"#payload": "payload"}, ExpressionAttributeValues: values})
	var failed *types.ConditionalCheckFailedException
	if errors.As(e, &failed) {
		return ErrConflict
	}
	return e
}

// SuspendInactive removes revoked/deleted accounts from the due queue without
// racing account purge or reactivation. The transaction only replaces an
// existing version while PROFILE is absent/inactive or all member grants remain
// absent. It never resurrects a deleted row.
func (s *DynamoStore) SuspendInactive(ctx context.Context, ref Ref) error {
	r, e := s.Get(ctx, ref.UserID, ref.ID)
	if errors.Is(e, ErrNotFound) {
		return nil
	}
	if e != nil {
		return e
	}
	version := r.Job.Version
	now := time.Now().UTC()
	r.Job.Status = "paused"
	r.Job.NextRunAt = ""
	r.Generation++
	cancelPending(r, now, "Account inactive; job paused.")
	r.Job.Version++
	r.Job.UpdatedAt = stamp(now)
	return s.write(ctx, r, version, false)
}
