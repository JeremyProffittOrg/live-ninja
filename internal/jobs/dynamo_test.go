package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type fakeDynamo struct {
	get       *dynamodb.GetItemInput
	query     *dynamodb.QueryInput
	tx        *dynamodb.TransactWriteItemsInput
	txErr     error
	item      map[string]types.AttributeValue
	profile   map[string]types.AttributeValue
	grants    map[string]bool
	update    *dynamodb.UpdateItemInput
	updateErr error
	pages     []*dynamodb.QueryOutput
	queries   int
}

func (f *fakeDynamo) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.get = in
	sk := attributeString(in.Key, "sk")
	if sk == "PROFILE" {
		profile := f.profile
		if profile == nil {
			profile = map[string]types.AttributeValue{"status": avs("active"), "role": avs("owner")}
		}
		return &dynamodb.GetItemOutput{Item: profile}, nil
	}
	if strings.HasPrefix(sk, "ALLOW#") {
		var item map[string]types.AttributeValue
		if f.grants[sk] {
			item = in.Key
		}
		return &dynamodb.GetItemOutput{Item: item}, nil
	}
	return &dynamodb.GetItemOutput{Item: f.item}, nil
}
func (f *fakeDynamo) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.query = in
	f.queries++
	if len(f.pages) > 0 {
		out := f.pages[0]
		f.pages = f.pages[1:]
		return out, nil
	}
	return &dynamodb.QueryOutput{}, nil
}
func (f *fakeDynamo) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.update = in
	return &dynamodb.UpdateItemOutput{}, f.updateErr
}
func (f *fakeDynamo) TransactWriteItems(_ context.Context, in *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	f.tx = in
	return &dynamodb.TransactWriteItemsOutput{}, f.txErr
}
func TestDynamoOwnershipAndActiveProfileCAS(t *testing.T) {
	f := &fakeDynamo{}
	s := NewDynamoStoreWithClient(f, "table")
	ctx := context.Background()
	r := &Record{UserID: "alice", Job: Job{ID: "job_12345678901234567890123456789012", Status: "active", Version: 2, NextRunAt: "2026-10-03T12:00:00Z"}}
	if e := s.CompareAndSwap(ctx, r, 1); e != nil {
		t.Fatal(e)
	}
	if len(f.tx.TransactItems) != 2 {
		t.Fatal("not atomic")
	}
	check := f.tx.TransactItems[0].ConditionCheck
	put := f.tx.TransactItems[1].Put
	if check.Key["pk"].(*types.AttributeValueMemberS).Value != "USER#alice" || check.Key["sk"].(*types.AttributeValueMemberS).Value != "PROFILE" || !strings.Contains(aws.ToString(check.ConditionExpression), "#status = :active") {
		t.Fatal("missing reauthorization", check)
	}
	if put.Item["pk"].(*types.AttributeValueMemberS).Value != "USER#alice" || !strings.Contains(aws.ToString(put.ConditionExpression), "version") {
		t.Fatal("unsafe CAS", put)
	}
	if put.Item["gsi2sk"].(*types.AttributeValueMemberS).Value != "2026-10-03T12:00:00.000000000Z#alice#"+r.Job.ID {
		t.Fatal("wrong due key")
	}
	if _, e := s.Due(ctx, parse("2026-10-03T12:00:00Z"), 20); e != nil {
		t.Fatal(e)
	}
	if aws.ToString(f.query.IndexName) != "GSI2" || aws.ToInt32(f.query.Limit) != 20 {
		t.Fatal("not bounded index query")
	}
	b, _ := json.Marshal(r)
	f.item = map[string]types.AttributeValue{"payload": avs(string(b))}
	if _, e := s.Get(ctx, "alice", r.Job.ID); e != nil || !aws.ToBool(f.get.ConsistentRead) {
		t.Fatal("read not consistent", e)
	}
	if _, e := s.Get(ctx, "bob", r.Job.ID); e == nil {
		t.Fatal("accepted mismatched payload owner")
	}
	if _, _, e := s.List(ctx, "alice", 20, ""); e != nil || f.query.ExpressionAttributeValues[":pk"].(*types.AttributeValueMemberS).Value != "USER#alice" || !aws.ToBool(f.query.ConsistentRead) {
		t.Fatal("unsafe list", e)
	}
}
func TestDynamoConditionalFailureDistinguishesRevocation(t *testing.T) {
	f := &fakeDynamo{}
	s := NewDynamoStoreWithClient(f, "table")
	r := &Record{UserID: "alice", Job: Job{ID: "job_12345678901234567890123456789012", Version: 2}}
	f.txErr = &types.TransactionCanceledException{CancellationReasons: []types.CancellationReason{{Code: aws.String("ConditionalCheckFailed")}, {Code: aws.String("None")}}}
	if e := s.CompareAndSwap(context.Background(), r, 1); !errors.Is(e, ErrForbidden) {
		t.Fatal(e)
	}
	f.txErr = &types.TransactionCanceledException{CancellationReasons: []types.CancellationReason{{Code: aws.String("None")}, {Code: aws.String("ConditionalCheckFailed")}}}
	if e := s.CompareAndSwap(context.Background(), r, 1); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
}
func TestEncodedHistoryCompactionAndDueFairness(t *testing.T) {
	s, fs, now := fixture(t)
	ctx := context.Background()
	in := input("reminder")
	in.Instructions = strings.Repeat("<", 2000)
	j := create(t, s, in)
	for n := 0; n < 70; n++ {
		j = current(t, s, j.ID)
		if _, e := s.RunNow(ctx, "alice", j.ID, j.Version, strings.Repeat("x", 8)+time.Duration(n).String()); e != nil {
			t.Fatalf("run%d %v", n, e)
		}
	}
	r, e := fs.Get(ctx, "alice", j.ID)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(r)
	if len(b) > 250000 || len(r.Runs) >= 50 || len(r.Runs) < 1 {
		t.Fatalf("unbounded encoded history: %d bytes, %d runs", len(b), len(r.Runs))
	}
	// Fill a scheduled review job with unresolved reviews. It must back off,
	// preserving intent while freeing the due-index head for the next job.
	in = input("review")
	in.Schedule = Schedule{Kind: "once", At: stamp(now.Add(time.Minute))}
	job, e := s.Create(ctx, "alice", in, "create-pending-job")
	if e != nil {
		t.Fatal(e)
	}
	for n := 0; n < 50; n++ {
		job = current(t, s, job.ID)
		if _, e = s.RunNow(ctx, "alice", job.ID, job.Version, strings.Repeat("p", 8)+time.Duration(n).String()); e != nil {
			t.Fatal(e)
		}
	}
	*now = now.Add(time.Minute)
	if _, e = s.Tick(ctx, *now, 100); e != nil {
		t.Fatal(e)
	}
	job = current(t, s, job.ID)
	if job.Error == "" || job.NextRunAt == "" {
		t.Fatalf("capacity not retained %+v", job)
	}
	refs, e := fs.Due(ctx, *now, 1)
	if e != nil || len(refs) != 0 {
		t.Fatalf("full job poisoned index %+v %v", refs, e)
	}
}

func TestDynamoMemberGrantIsAtomicAndRevocationPausesSafely(t *testing.T) {
	f := &fakeDynamo{profile: map[string]types.AttributeValue{"status": avs("active"), "role": avs("member"), "amazonUserId": avs("amzn.account.1"), "email": avs("ALICE@Example.com")}, grants: map[string]bool{"ALLOW#alice@example.com": true}}
	s := NewDynamoStoreWithClient(f, "table")
	ctx := context.Background()
	r := &Record{UserID: "alice", Job: Job{ID: "job_12345678901234567890123456789012", Version: 2, Status: "active", NextRunAt: "2026-10-03T12:00:00Z"}}
	if e := s.CompareAndSwap(ctx, r, 1); e != nil {
		t.Fatal(e)
	}
	if len(f.tx.TransactItems) != 3 {
		t.Fatal("grant not in transaction")
	}
	profile := f.tx.TransactItems[0].ConditionCheck
	grant := f.tx.TransactItems[1].ConditionCheck
	for _, required := range []string{"#role = :role", "#amazonUserId = :amazonUserId", "#email = :email"} {
		if !strings.Contains(aws.ToString(profile.ConditionExpression), required) {
			t.Fatal("profile identity not fenced", profile)
		}
	}
	if attributeString(grant.Key, "sk") != "ALLOW#alice@example.com" || aws.ToString(grant.ConditionExpression) != "attribute_exists(pk)" {
		t.Fatal("normalized grant not fenced", grant)
	}
	// Remove the grant AFTER the strong reads: transaction cancellation is a
	// forbidden result and cannot commit the run receipt.
	f.txErr = &types.TransactionCanceledException{CancellationReasons: []types.CancellationReason{{Code: aws.String("None")}, {Code: aws.String("ConditionalCheckFailed")}, {Code: aws.String("None")}}}
	if e := s.CompareAndSwap(ctx, r, 1); !errors.Is(e, ErrForbidden) {
		t.Fatal("revocation raced through", e)
	}
	f.txErr = nil
	f.grants = map[string]bool{}
	if e := s.CompareAndSwap(ctx, r, 1); !errors.Is(e, ErrForbidden) {
		t.Fatal("removed member allowed", e)
	}
	b, _ := json.Marshal(r)
	f.item = map[string]types.AttributeValue{"payload": avs(string(b))}
	if e := s.SuspendInactive(ctx, Ref{UserID: "alice", ID: r.Job.ID}); e != nil {
		t.Fatal(e)
	}
	if len(f.tx.TransactItems) != 4 {
		t.Fatal("missing inverse grant fences")
	}
	for _, index := range []int{1, 2} {
		if aws.ToString(f.tx.TransactItems[index].ConditionCheck.ConditionExpression) != "attribute_not_exists(pk)" {
			t.Fatal("reactivation could race pause")
		}
	}
	put := f.tx.TransactItems[3].Put
	if _, exists := put.Item["gsi2pk"]; exists {
		t.Fatal("revoked job still due")
	}
	if aws.ToString(put.ConditionExpression) != "version = :version" {
		t.Fatal("purge recreation possible")
	}
	// Purged PROFILE is allowed ONLY for a conditional pause of an existing
	// job; ordinary writes remain forbidden and cannot recreate it.
	f.profile = map[string]types.AttributeValue{}
	if e := s.CompareAndSwap(ctx, r, 1); !errors.Is(e, ErrForbidden) {
		t.Fatal(e)
	}
	if e := s.SuspendInactive(ctx, Ref{UserID: "alice", ID: r.Job.ID}); e != nil {
		t.Fatal(e)
	}
	if aws.ToString(f.tx.TransactItems[1].Put.ConditionExpression) != "version = :version" {
		t.Fatal("missing purge fence")
	}
}

func TestDynamoDueSkipsMalformedHeadAndFollowsEmptyPages(t *testing.T) {
	id := "job_12345678901234567890123456789012"
	cursor := map[string]types.AttributeValue{"pk": avs("malformed"), "sk": avs("bad")}
	f := &fakeDynamo{pages: []*dynamodb.QueryOutput{
		{Items: []map[string]types.AttributeValue{cursor}, LastEvaluatedKey: cursor},
		{LastEvaluatedKey: cursor},
		{Items: []map[string]types.AttributeValue{key("alice", id)}},
	}}
	s := NewDynamoStoreWithClient(f, "table")
	refs, e := s.Due(context.Background(), time.Now(), 1)
	if e != nil || len(refs) != 1 || refs[0].ID != id || f.queries != 3 || f.query.ExclusiveStartKey == nil {
		t.Fatalf("healthy job starved %+v %v calls%d", refs, e, f.queries)
	}
}

func TestDynamoDueBudgetDoesNotDiscardHealthyRefs(t *testing.T) {
	id := "job_12345678901234567890123456789012"
	cursor := map[string]types.AttributeValue{"pk": avs("malformed"), "sk": avs("bad")}
	f := &fakeDynamo{}
	for n := 0; n < 10; n++ {
		page := &dynamodb.QueryOutput{LastEvaluatedKey: cursor}
		if n == 0 {
			page.Items = []map[string]types.AttributeValue{key("alice", id)}
		}
		f.pages = append(f.pages, page)
	}
	refs, e := NewDynamoStoreWithClient(f, "table").Due(context.Background(), time.Now(), 2)
	if e != nil || len(refs) != 1 {
		t.Fatalf("healthy refs discarded %+v %v", refs, e)
	}
}

func TestDynamoQuarantinePreservesBytesAndFencesRepairPurge(t *testing.T) {
	ctx := context.Background()
	ref := Ref{UserID: "alice", ID: "job_12345678901234567890123456789012"}
	f := &fakeDynamo{item: map[string]types.AttributeValue{"payload": avs("{broken original bytes")}}
	s := NewDynamoStoreWithClient(f, "table")
	if e := s.Quarantine(ctx, ref); e != nil {
		t.Fatal(e)
	}
	u := f.update
	if u == nil || aws.ToString(u.ConditionExpression) != "attribute_exists(pk) AND #payload = :payload" || attributeString(u.ExpressionAttributeValues, ":payload") != "{broken original bytes" || strings.Contains(aws.ToString(u.UpdateExpression), "payload") {
		t.Fatal("unsafe quarantine", u)
	}
	f.updateErr = &types.ConditionalCheckFailedException{}
	if e := s.Quarantine(ctx, ref); !errors.Is(e, ErrConflict) {
		t.Fatal("repair/purge fence bypassed", e)
	}
	f.update = nil
	f.item = nil
	if e := s.Quarantine(ctx, ref); e != nil || f.update != nil {
		t.Fatal("purged row recreated", e)
	}
	r := Record{UserID: ref.UserID, Job: Job{ID: ref.ID, Version: 1}}
	b, _ := json.Marshal(r)
	f.item = map[string]types.AttributeValue{"payload": avs(string(b))}
	if e := s.Quarantine(ctx, ref); e != nil || f.update != nil {
		t.Fatal("repaired record quarantined", e)
	}
}
