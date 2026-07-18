package main

// Unit tests for the account-purge worker: full-footprint erasure over an
// in-memory table/S3/IoT/SQS, batch chunking + UnprocessedItems retry,
// cross-user isolation, and idempotent re-runs (including the
// exactly-once confirmation email).

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// ---- fakes ----

// purgeDDB adds BatchWriteItem on top of the shared FakeDynamo (which the
// store fake surface doesn't need). Optionally reports the first request
// of the first batch as unprocessed to exercise the retry loop.
type purgeDDB struct {
	*testutil.FakeDynamo
	table              string
	batchCalls         int
	unprocessedOnFirst bool
}

func (p *purgeDDB) BatchWriteItem(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
	p.batchCalls++
	reqs := params.RequestItems[p.table]
	if len(reqs) > batchWriteMax {
		return nil, &ddbtypes.ProvisionedThroughputExceededException{Message: aws.String("batch > 25")}
	}
	var unprocessed []ddbtypes.WriteRequest
	for i, r := range reqs {
		if r.DeleteRequest == nil {
			continue
		}
		if p.unprocessedOnFirst && p.batchCalls == 1 && i == 0 {
			unprocessed = append(unprocessed, r)
			continue
		}
		if _, err := p.FakeDynamo.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(p.table),
			Key:       r.DeleteRequest.Key,
		}); err != nil {
			return nil, err
		}
	}
	out := &dynamodb.BatchWriteItemOutput{}
	if len(unprocessed) > 0 {
		out.UnprocessedItems = map[string][]ddbtypes.WriteRequest{p.table: unprocessed}
	}
	return out, nil
}

// purgeS3 is a multi-bucket in-memory object store with 2-key list pages
// (so prefix purges exercise pagination).
type purgeS3 struct {
	objects map[string]map[string]bool // bucket -> key set
}

func (f *purgeS3) put(bucket, key string) {
	if f.objects == nil {
		f.objects = map[string]map[string]bool{}
	}
	if f.objects[bucket] == nil {
		f.objects[bucket] = map[string]bool{}
	}
	f.objects[bucket][key] = true
}

func (f *purgeS3) keys(bucket, prefix string) []string {
	var out []string
	for k := range f.objects[bucket] {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

func (f *purgeS3) ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	matched := f.keys(aws.ToString(params.Bucket), aws.ToString(params.Prefix))
	// Stable order for paging.
	for i := 0; i < len(matched); i++ {
		for j := i + 1; j < len(matched); j++ {
			if matched[j] < matched[i] {
				matched[i], matched[j] = matched[j], matched[i]
			}
		}
	}
	start := 0
	if tok := aws.ToString(params.ContinuationToken); tok != "" {
		start, _ = strconv.Atoi(tok)
	}
	end := min(start+2, len(matched))
	out := &s3.ListObjectsV2Output{}
	for _, k := range matched[start:end] {
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
	}
	if end < len(matched) {
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(strconv.Itoa(end))
	} else {
		out.IsTruncated = aws.Bool(false)
	}
	return out, nil
}

func (f *purgeS3) DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	bucket := aws.ToString(params.Bucket)
	for _, o := range params.Delete.Objects {
		delete(f.objects[bucket], aws.ToString(o.Key))
	}
	return &s3.DeleteObjectsOutput{}, nil
}

// purgeIoT records the teardown call sequence; every op after the first
// full run answers ResourceNotFound (idempotent re-run behavior).
type purgeIoT struct {
	ops       []string
	notFound  bool
	policyArn string
}

func (f *purgeIoT) record(op string) error {
	f.ops = append(f.ops, op)
	if f.notFound {
		return &iottypes.ResourceNotFoundException{Message: aws.String("gone")}
	}
	return nil
}

func (f *purgeIoT) ListAttachedPolicies(ctx context.Context, params *iot.ListAttachedPoliciesInput, optFns ...func(*iot.Options)) (*iot.ListAttachedPoliciesOutput, error) {
	if err := f.record("ListAttachedPolicies"); err != nil {
		return nil, err
	}
	return &iot.ListAttachedPoliciesOutput{Policies: []iottypes.Policy{
		{PolicyName: aws.String("live-ninja-device"), PolicyArn: aws.String(f.policyArn)},
	}}, nil
}

func (f *purgeIoT) DetachPolicy(ctx context.Context, params *iot.DetachPolicyInput, optFns ...func(*iot.Options)) (*iot.DetachPolicyOutput, error) {
	return &iot.DetachPolicyOutput{}, f.record("DetachPolicy")
}

func (f *purgeIoT) DetachThingPrincipal(ctx context.Context, params *iot.DetachThingPrincipalInput, optFns ...func(*iot.Options)) (*iot.DetachThingPrincipalOutput, error) {
	return &iot.DetachThingPrincipalOutput{}, f.record("DetachThingPrincipal")
}

func (f *purgeIoT) UpdateCertificate(ctx context.Context, params *iot.UpdateCertificateInput, optFns ...func(*iot.Options)) (*iot.UpdateCertificateOutput, error) {
	return &iot.UpdateCertificateOutput{}, f.record("UpdateCertificate")
}

func (f *purgeIoT) DeleteCertificate(ctx context.Context, params *iot.DeleteCertificateInput, optFns ...func(*iot.Options)) (*iot.DeleteCertificateOutput, error) {
	return &iot.DeleteCertificateOutput{}, f.record("DeleteCertificate")
}

func (f *purgeIoT) DeleteThing(ctx context.Context, params *iot.DeleteThingInput, optFns ...func(*iot.Options)) (*iot.DeleteThingOutput, error) {
	return &iot.DeleteThingOutput{}, f.record("DeleteThing")
}

type purgeSQS struct{ messages []string }

func (f *purgeSQS) SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.messages = append(f.messages, aws.ToString(params.MessageBody))
	return &sqs.SendMessageOutput{}, nil
}

// ---- fixture ----

const testTable = "live-ninja"

func newTestPurger(t *testing.T) (*Purger, *purgeDDB, *purgeS3, *purgeIoT, *purgeSQS, *store.Store) {
	t.Helper()
	fake := testutil.NewFakeDynamo()
	ddb := &purgeDDB{FakeDynamo: fake, table: testTable}
	st := store.NewWithClient(fake, testTable)
	fs3 := &purgeS3{}
	fiot := &purgeIoT{policyArn: "arn:aws:iot:us-east-1:1:policy/live-ninja-device"}
	fsqs := &purgeSQS{}
	p := &Purger{
		Log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		DDB:                ddb,
		S3:                 fs3,
		IoT:                fiot,
		SQS:                fsqs,
		Store:              st,
		Table:              testTable,
		DeliverablesBucket: "deliv-bucket",
		UserBucket:         "user-bucket",
		WakewordsBucket:    "ww-bucket",
		EmailQueueURL:      "https://sqs.example/queue",
		Sleep:              func(time.Duration) {},
	}
	return p, ddb, fs3, fiot, fsqs, st
}

func seedUserFootprint(t *testing.T, st *store.Store, fs3 *purgeS3) {
	t.Helper()
	ctx := context.Background()

	if err := st.CreateUser(ctx, &store.User{
		UserID: "u1", AmazonUserID: "amzn1.account.u1",
		Email: "U1@Example.com", Role: store.RoleMember, Status: store.UserStatusDeleting,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDevice(ctx, &store.Device{
		DeviceID: "d1", UserID: "u1", FamilyID: "fam-d1", Name: "Tab5",
		ThingName: "ln-thing-d1", CertArn: "arn:aws:iot:us-east-1:1:cert/abc", CertID: "abc",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutDeviceTelemetry(ctx, "d1", map[string]any{"rssi": -40}); err != nil {
		t.Fatal(err)
	}
	// 30 transcript rows -> more than one BatchWriteItem chunk.
	for i := 0; i < 30; i++ {
		sk := "LOG#sess-1#" + strconv.Itoa(100000+i)
		if err := st.ConditionalPut(ctx, "USER#u1", sk, map[string]any{"text": "t"}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ConditionalPut(ctx, "USER#u1", "WAKEWORD#ww1", map[string]any{"phrase": "hey ninja"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, &store.Session{
		SessionID: "sess-1", UserID: "u1", FamilyID: "fam-w", Surface: "web", RefreshHash: "h",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAllow(ctx, "u1@example.com", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAllow(ctx, "amzn1.account.u1", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := st.ConditionalPut(ctx, "CONFIG", "ACTIVEUSER#u1#2026-07-17", map[string]any{"userId": "u1"}, 0); err != nil {
		t.Fatal(err)
	}

	// Bystander data that must survive.
	if err := st.CreateUser(ctx, &store.User{
		UserID: "u2", AmazonUserID: "amzn1.account.u2",
		Email: "u2@example.com", Role: store.RoleOwner, Status: store.UserStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAllow(ctx, "u2@example.com", "owner"); err != nil {
		t.Fatal(err)
	}

	fs3.put("deliv-bucket", "deliverables/u1/id1/report.md")
	fs3.put("deliv-bucket", "deliverables/u1/id2/data.csv")
	fs3.put("deliv-bucket", "deliverables/u1/id3/big.zip")
	fs3.put("deliv-bucket", "deliverables/u2/idz/keep.txt")
	fs3.put("user-bucket", "users/u1/avatar.png")
	fs3.put("ww-bucket", "wakewords/ww1/web/model.onnx")
	fs3.put("ww-bucket", "wakewords/ww1/esp32/model.onnx")
	fs3.put("ww-bucket", "wakewords/other/web/model.onnx")
}

// ---- tests ----

func TestPurgeRunErasesFootprint(t *testing.T) {
	p, ddb, fs3, fiot, fsqs, st := newTestPurger(t)
	seedUserFootprint(t, st, fs3)
	ctx := context.Background()

	err := p.Run(ctx, Event{UserID: "u1", RequestedAt: "2026-07-17T12:00:00Z"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// USER# partition fully gone.
	items, err := st.QueryUserPartition(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("USER#u1 items remaining = %d (%v)", len(items), items)
	}
	// Batch chunking actually happened (34 user rows + device rows + marker).
	if ddb.batchCalls < 2 {
		t.Errorf("batchCalls = %d, want >= 2 (chunking)", ddb.batchCalls)
	}

	// DEVICE# partition gone.
	if d, _ := st.GetDevice(ctx, "d1"); d != nil {
		t.Errorf("device META survived")
	}
	if tele := ddb.FakeDynamo.RawItem("DEVICE#d1", "TELEM"); tele != nil {
		t.Errorf("device TELEM survived")
	}

	// CONFIG identifiers gone (email matched case-insensitively), bystander kept.
	if allowed, _ := st.IsAllowed(ctx, "amzn1.account.u1", "u1@example.com"); allowed {
		t.Errorf("allowlist identifiers survived")
	}
	if allowed, _ := st.IsAllowed(ctx, "", "u2@example.com"); !allowed {
		t.Errorf("bystander allowlist entry deleted")
	}
	if m := ddb.FakeDynamo.RawItem("CONFIG", "ACTIVEUSER#u1#2026-07-17"); m != nil {
		t.Errorf("activeuser marker survived")
	}

	// Bystander user intact.
	if u2, _ := st.GetUser(ctx, "u2"); u2 == nil {
		t.Errorf("bystander user deleted")
	}

	// S3: caller's prefixes empty, others intact.
	if left := fs3.keys("deliv-bucket", "deliverables/u1/"); len(left) != 0 {
		t.Errorf("deliverables survived: %v", left)
	}
	if left := fs3.keys("deliv-bucket", "deliverables/u2/"); len(left) != 1 {
		t.Errorf("bystander deliverables touched: %v", left)
	}
	if left := fs3.keys("user-bucket", "users/u1/"); len(left) != 0 {
		t.Errorf("user bucket objects survived: %v", left)
	}
	if left := fs3.keys("ww-bucket", "wakewords/ww1/"); len(left) != 0 {
		t.Errorf("wakeword objects survived: %v", left)
	}
	if left := fs3.keys("ww-bucket", "wakewords/other/"); len(left) != 1 {
		t.Errorf("unrelated wakeword objects touched: %v", left)
	}

	// IoT teardown ran the full sequence.
	want := []string{"ListAttachedPolicies", "DetachPolicy", "DetachThingPrincipal",
		"UpdateCertificate", "DeleteCertificate", "DeleteThing"}
	if len(fiot.ops) != len(want) {
		t.Fatalf("iot ops = %v, want %v", fiot.ops, want)
	}
	for i := range want {
		if fiot.ops[i] != want[i] {
			t.Errorf("iot op[%d] = %s, want %s", i, fiot.ops[i], want[i])
		}
	}

	// Confirmation email enqueued once, to the profile email.
	if len(fsqs.messages) != 1 {
		t.Fatalf("emails = %d, want 1", len(fsqs.messages))
	}
	var msg struct {
		Template, To, Subject, Text string
	}
	if err := json.Unmarshal([]byte(fsqs.messages[0]), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Template != "account-deleted" || msg.To != "U1@Example.com" {
		t.Errorf("email = %+v", msg)
	}
}

func TestPurgeRunIsIdempotent(t *testing.T) {
	p, _, fs3, fiot, fsqs, st := newTestPurger(t)
	seedUserFootprint(t, st, fs3)
	ctx := context.Background()

	ev := Event{UserID: "u1", Email: "u1@example.com", AmazonUserID: "amzn1.account.u1"}
	if err := p.Run(ctx, ev); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Everything already gone; IoT now answers ResourceNotFound.
	fiot.notFound = true
	if err := p.Run(ctx, ev); err != nil {
		t.Fatalf("second run must be a clean no-op: %v", err)
	}
	// Exactly one confirmation across both runs (IDEMP marker).
	if len(fsqs.messages) != 1 {
		t.Errorf("emails = %d, want 1 across retries", len(fsqs.messages))
	}
	items, _ := st.QueryUserPartition(ctx, "u1")
	if len(items) != 0 {
		t.Errorf("second run left items: %v", items)
	}
}

func TestPurgeRetriesUnprocessedBatchItems(t *testing.T) {
	p, ddb, fs3, _, _, st := newTestPurger(t)
	seedUserFootprint(t, st, fs3)
	ddb.unprocessedOnFirst = true

	if err := p.Run(context.Background(), Event{UserID: "u1", Email: "u1@example.com"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	items, _ := st.QueryUserPartition(context.Background(), "u1")
	if len(items) != 0 {
		t.Errorf("unprocessed item never retried: %v", items)
	}
}

func TestPurgeRequiresUserID(t *testing.T) {
	p, _, _, _, _, _ := newTestPurger(t)
	if err := p.Run(context.Background(), Event{}); err == nil {
		t.Fatal("expected error for missing userId")
	}
}

func TestPurgeRefusesMalformedS3Prefix(t *testing.T) {
	// Direct unit check of the widening guard: an empty user id must never
	// produce a bucket-sweeping prefix purge.
	p, _, fs3, _, _, _ := newTestPurger(t)
	fs3.put("deliv-bucket", "deliverables/u2/idz/keep.txt")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	p.purgePrefix(context.Background(), log, "deliv-bucket", "deliverables//")
	p.purgePrefix(context.Background(), log, "deliv-bucket", "deliverables/")
	if len(fs3.keys("deliv-bucket", "deliverables/")) != 1 {
		t.Errorf("malformed prefix widened into a delete")
	}
}
