// Command account-purge is the M7 right-to-delete worker (FR `[PRIV]`,
// contracts/api.md DELETE /v1/account): a direct-invoke Lambda
// (InvocationType=Event from the web function's DELETE /api/v1/account
// handler) that permanently erases one user's footprint. Synchronous
// orchestration inside the web function was rejected (locked M7 decision:
// timeout risk) — this worker gets a 300s budget and is idempotent and
// resumable: every step tolerates "already gone", so a crashed or retried
// invocation simply re-runs to completion.
//
// Purge order is deliberate — metadata is read first, external side
// effects next, and the DynamoDB partitions last, so a mid-run crash
// never strands data that a re-run can no longer find:
//
//  1. Read the profile + device list + wakeword ids (Query/GetItem only).
//  2. S3 prefix purges: deliverables/<uid>/ (deliverables bucket),
//     users/<uid>/ (user bucket), wakewords/<wwId>/ per user wakeword
//     (wakewords bucket — wakeword objects are keyed by wwId, not userId,
//     so the ids collected in step 1 drive the prefixes).
//  3. IoT teardown per device with provisioned material (thingName/cert):
//     detach policies, detach principal, deactivate + delete cert, delete
//     thing — every call best-effort (ResourceNotFound = already done).
//  4. DEVICE#<id> partition deletes (META/TELEM/…).
//  5. CONFIG cleanup: the user's ALLOW# entries (their identifiers must
//     not outlive them) and ACTIVEUSER#<uid># markers.
//  6. The whole USER#<uid> partition via paginated Query +
//     BatchWriteItem deletes (25/batch, unprocessed-item retry) — LOG#,
//     SESS#, SETTINGS#, CONSENT#, DELIV#, WAKEWORD#, ENT#/EMB#, CONV#/
//     TREF#, PROFILE, everything.
//  7. LWA revoke: Live Ninja never persists LWA access/refresh tokens
//     (ExchangeCode's result is used transiently during sign-in and
//     dropped — see internal/auth/lwa.go), so there is no stored Amazon
//     token to revoke; the user's LWA grant is managed on Amazon's side.
//     Logged for the audit trail.
//  8. SES confirmation via the shared email queue (exactly once across
//     retries via a conditional IDEMP marker).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	awslambda "github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

const metricsNamespace = "LiveNinja/AccountPurge"

// batchWriteMax is DynamoDB's BatchWriteItem request cap.
const batchWriteMax = 25

// unprocessedRetries bounds the UnprocessedItems retry loop per batch.
const unprocessedRetries = 8

// emailIdempotencyTTL keeps the "confirmation already sent" marker long
// enough to cover any realistic async-retry window.
const emailIdempotencyTTL = 7 * 24 * time.Hour

// Event is the invoke payload from the web function's DELETE
// /api/v1/account handler (internal/webapp/account_routes.go mirrors this
// shape). Email/AmazonUserID are captured there from the profile before it
// is marked deleting, so a retried purge still knows them after the
// profile row is gone.
type Event struct {
	UserID       string `json:"userId"`
	Email        string `json:"email,omitempty"`
	AmazonUserID string `json:"amazonUserId,omitempty"`
	RequestedAt  string `json:"requestedAt,omitempty"`
}

// ---- narrow AWS client interfaces (tests inject fakes) ----

type ddbAPI interface {
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	BatchWriteItem(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

type s3API interface {
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

type iotAPI interface {
	ListAttachedPolicies(ctx context.Context, params *iot.ListAttachedPoliciesInput, optFns ...func(*iot.Options)) (*iot.ListAttachedPoliciesOutput, error)
	DetachPolicy(ctx context.Context, params *iot.DetachPolicyInput, optFns ...func(*iot.Options)) (*iot.DetachPolicyOutput, error)
	DetachThingPrincipal(ctx context.Context, params *iot.DetachThingPrincipalInput, optFns ...func(*iot.Options)) (*iot.DetachThingPrincipalOutput, error)
	UpdateCertificate(ctx context.Context, params *iot.UpdateCertificateInput, optFns ...func(*iot.Options)) (*iot.UpdateCertificateOutput, error)
	DeleteCertificate(ctx context.Context, params *iot.DeleteCertificateInput, optFns ...func(*iot.Options)) (*iot.DeleteCertificateOutput, error)
	DeleteThing(ctx context.Context, params *iot.DeleteThingInput, optFns ...func(*iot.Options)) (*iot.DeleteThingOutput, error)
}

type sqsAPI interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// emailMessage mirrors cmd/email-dispatch's EmailMessage queue body shape.
type emailMessage struct {
	Template string `json:"template"`
	To       string `json:"to"`
	Subject  string `json:"subject"`
	Text     string `json:"text"`
}

// Purger carries every dependency of one purge run.
type Purger struct {
	Log   *slog.Logger
	DDB   ddbAPI
	S3    s3API
	IoT   iotAPI
	SQS   sqsAPI
	Store *store.Store

	Table              string
	DeliverablesBucket string // env DELIVERABLES_BUCKET ("" -> skip)
	UserBucket         string // env USER_BUCKET ("" -> skip)
	WakewordsBucket    string // env WAKEWORDS_BUCKET ("" -> skip)
	EmailQueueURL      string // env EMAIL_QUEUE_URL ("" -> skip confirm)

	// Sleep is time.Sleep in production; tests replace it.
	Sleep func(time.Duration)
}

type itemKey struct{ pk, sk string }

// Run executes the full purge for one event. Errors from best-effort
// steps (S3, IoT, email) are logged and do not fail the run; errors that
// would leave user data behind in DynamoDB do fail it, so the async-retry
// (or a user-retried DELETE) runs again.
func (p *Purger) Run(ctx context.Context, ev Event) error {
	if strings.TrimSpace(ev.UserID) == "" {
		return errors.New("account-purge: userId is required")
	}
	log := p.Log.With(slog.String("userId", ev.UserID))
	started := time.Now()

	// ---- 1. read metadata before anything is deleted ----
	email, amazonUserID := ev.Email, ev.AmazonUserID
	if u, err := p.Store.GetUser(ctx, ev.UserID); err != nil {
		return fmt.Errorf("account-purge: read profile: %w", err)
	} else if u != nil {
		if email == "" {
			email = u.Email
		}
		if amazonUserID == "" {
			amazonUserID = u.AmazonUserID
		}
	}

	devices, err := p.Store.ListDevices(ctx, ev.UserID)
	if err != nil {
		return fmt.Errorf("account-purge: list devices: %w", err)
	}

	userKeys, err := p.listPartitionKeys(ctx, "USER#"+ev.UserID)
	if err != nil {
		return fmt.Errorf("account-purge: list user partition: %w", err)
	}
	var wakewordIDs []string
	for _, k := range userKeys {
		if id, ok := strings.CutPrefix(k.sk, "WAKEWORD#"); ok && id != "" {
			wakewordIDs = append(wakewordIDs, id)
		}
	}
	log.Info("account-purge: starting",
		slog.Int("items", len(userKeys)), slog.Int("devices", len(devices)),
		slog.Int("wakewords", len(wakewordIDs)))

	// ---- 2. S3 prefix purges (best-effort) ----
	p.purgePrefix(ctx, log, p.DeliverablesBucket, "deliverables/"+ev.UserID+"/")
	p.purgePrefix(ctx, log, p.UserBucket, "users/"+ev.UserID+"/")
	for _, wwID := range wakewordIDs {
		p.purgePrefix(ctx, log, p.WakewordsBucket, "wakewords/"+wwID+"/")
	}

	// ---- 3 + 4. IoT teardown + DEVICE# partition deletes ----
	deleted := 0
	for _, d := range devices {
		p.teardownIoT(ctx, log, d)
		devKeys, err := p.listPartitionKeys(ctx, "DEVICE#"+d.DeviceID)
		if err != nil {
			return fmt.Errorf("account-purge: list device partition %s: %w", d.DeviceID, err)
		}
		if err := p.batchDelete(ctx, devKeys); err != nil {
			return fmt.Errorf("account-purge: delete device partition %s: %w", d.DeviceID, err)
		}
		deleted += len(devKeys)
	}

	// ---- 5. CONFIG cleanup: allowlist identifiers + active-user markers ----
	for _, key := range []string{amazonUserID, strings.ToLower(strings.TrimSpace(email))} {
		if key == "" {
			continue
		}
		if _, err := p.DDB.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(p.Table),
			Key:       ddbKey("CONFIG", "ALLOW#"+key),
		}); err != nil {
			return fmt.Errorf("account-purge: delete allowlist entry: %w", err)
		}
	}
	markerKeys, err := p.listPrefixKeys(ctx, "CONFIG", "ACTIVEUSER#"+ev.UserID+"#")
	if err != nil {
		return fmt.Errorf("account-purge: list activeuser markers: %w", err)
	}
	if err := p.batchDelete(ctx, markerKeys); err != nil {
		return fmt.Errorf("account-purge: delete activeuser markers: %w", err)
	}
	deleted += len(markerKeys)

	// ---- 6. the USER# partition itself (PROFILE last is not required —
	// the whole partition goes in one keys-then-delete pass; a re-run
	// simply finds an empty partition) ----
	if err := p.batchDelete(ctx, userKeys); err != nil {
		return fmt.Errorf("account-purge: delete user partition: %w", err)
	}
	deleted += len(userKeys)

	// ---- 7. LWA revoke (documented no-op — see package comment) ----
	log.Info("account-purge: LWA tokens are never persisted server-side; no stored Amazon token to revoke")

	// ---- 8. SES confirmation, exactly once across retries ----
	p.sendConfirmation(ctx, log, ev.UserID, email)

	observ.EmitMetric(metricsNamespace, "AccountPurged", 1, "Count", nil)
	observ.EmitMetric(metricsNamespace, "ItemsDeleted", float64(deleted), "Count", nil)
	log.Info("account-purge: complete",
		slog.Int("itemsDeleted", deleted),
		slog.Duration("elapsed", time.Since(started)))
	return nil
}

// ---- DynamoDB helpers ----

func ddbKey(pk, sk string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		"pk": &ddbtypes.AttributeValueMemberS{Value: pk},
		"sk": &ddbtypes.AttributeValueMemberS{Value: sk},
	}
}

// listPartitionKeys pages a whole partition's pk/sk pairs (keys-only
// projection — the cheapest possible read of the partition).
func (p *Purger) listPartitionKeys(ctx context.Context, pk string) ([]itemKey, error) {
	in := &dynamodb.QueryInput{
		TableName:                aws.String(p.Table),
		KeyConditionExpression:   aws.String("#pk = :pk"),
		ProjectionExpression:     aws.String("#pk, #sk"),
		ExpressionAttributeNames: map[string]string{"#pk": "pk", "#sk": "sk"},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":pk": &ddbtypes.AttributeValueMemberS{Value: pk},
		},
	}
	return p.queryKeys(ctx, in)
}

// listPrefixKeys pages pk + begins_with(sk, prefix) keys.
func (p *Purger) listPrefixKeys(ctx context.Context, pk, skPrefix string) ([]itemKey, error) {
	in := &dynamodb.QueryInput{
		TableName:                aws.String(p.Table),
		KeyConditionExpression:   aws.String("#pk = :pk AND begins_with(#sk, :pfx)"),
		ProjectionExpression:     aws.String("#pk, #sk"),
		ExpressionAttributeNames: map[string]string{"#pk": "pk", "#sk": "sk"},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":pk":  &ddbtypes.AttributeValueMemberS{Value: pk},
			":pfx": &ddbtypes.AttributeValueMemberS{Value: skPrefix},
		},
	}
	return p.queryKeys(ctx, in)
}

func (p *Purger) queryKeys(ctx context.Context, in *dynamodb.QueryInput) ([]itemKey, error) {
	var keys []itemKey
	for {
		out, err := p.DDB.Query(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, it := range out.Items {
			pkAttr, okPK := it["pk"].(*ddbtypes.AttributeValueMemberS)
			skAttr, okSK := it["sk"].(*ddbtypes.AttributeValueMemberS)
			if !okPK || !okSK {
				return nil, fmt.Errorf("account-purge: item missing string pk/sk")
			}
			keys = append(keys, itemKey{pk: pkAttr.Value, sk: skAttr.Value})
		}
		if len(out.LastEvaluatedKey) == 0 {
			return keys, nil
		}
		in.ExclusiveStartKey = out.LastEvaluatedKey
	}
}

// batchDelete deletes keys in BatchWriteItem chunks of 25, retrying
// UnprocessedItems with exponential backoff. Duplicate keys within one
// batch are collapsed first (BatchWriteItem rejects duplicates).
func (p *Purger) batchDelete(ctx context.Context, keys []itemKey) error {
	seen := make(map[itemKey]bool, len(keys))
	uniq := keys[:0]
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			uniq = append(uniq, k)
		}
	}

	for start := 0; start < len(uniq); start += batchWriteMax {
		end := min(start+batchWriteMax, len(uniq))
		reqs := make([]ddbtypes.WriteRequest, 0, end-start)
		for _, k := range uniq[start:end] {
			reqs = append(reqs, ddbtypes.WriteRequest{
				DeleteRequest: &ddbtypes.DeleteRequest{Key: ddbKey(k.pk, k.sk)},
			})
		}

		pending := map[string][]ddbtypes.WriteRequest{p.Table: reqs}
		for attempt := 0; ; attempt++ {
			out, err := p.DDB.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: pending})
			if err != nil {
				return fmt.Errorf("batch write: %w", err)
			}
			if len(out.UnprocessedItems) == 0 || len(out.UnprocessedItems[p.Table]) == 0 {
				break
			}
			if attempt >= unprocessedRetries {
				return fmt.Errorf("batch write: %d items still unprocessed after %d retries",
					len(out.UnprocessedItems[p.Table]), unprocessedRetries)
			}
			p.Sleep(time.Duration(1<<attempt) * 50 * time.Millisecond)
			pending = out.UnprocessedItems
		}
	}
	return nil
}

// ---- S3 helpers ----

// purgePrefix deletes every object under bucket/prefix (paged ListObjectsV2
// + DeleteObjects). Best-effort: failures are logged, never fatal — the
// bucket lifecycle rules are the backstop, and a purge re-run retries.
func (p *Purger) purgePrefix(ctx context.Context, log *slog.Logger, bucket, prefix string) {
	if bucket == "" || p.S3 == nil {
		return
	}
	// Defense in depth: a malformed prefix (missing trailing slash, empty
	// segment from an empty userId/wwId) must never widen into a
	// bucket-wide delete.
	valid := strings.HasSuffix(prefix, "/")
	segs := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
	if len(segs) < 2 {
		valid = false
	}
	for _, s := range segs {
		if s == "" {
			valid = false
		}
	}
	if !valid {
		log.Error("account-purge: refusing malformed s3 purge prefix",
			slog.String("bucket", bucket), slog.String("prefix", prefix))
		return
	}

	// List-then-delete-then-relist (never a continuation token): deleting
	// the listed page invalidates any token's position, so each pass
	// simply re-lists the prefix from the start until it comes back empty.
	// maxPasses bounds a pathological loop (e.g. deletes silently denied).
	const maxPasses = 1000
	deleted := 0
	for pass := 0; pass < maxPasses; pass++ {
		out, err := p.S3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket),
			Prefix: aws.String(prefix),
		})
		if err != nil {
			log.Warn("account-purge: s3 list failed",
				slog.String("bucket", bucket), slog.String("prefix", prefix), slog.String("error", err.Error()))
			return
		}
		if len(out.Contents) == 0 {
			break
		}
		objs := make([]s3types.ObjectIdentifier, 0, len(out.Contents))
		for _, o := range out.Contents {
			objs = append(objs, s3types.ObjectIdentifier{Key: o.Key})
		}
		if _, err := p.S3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &s3types.Delete{Objects: objs, Quiet: aws.Bool(true)},
		}); err != nil {
			log.Warn("account-purge: s3 delete failed",
				slog.String("bucket", bucket), slog.String("prefix", prefix), slog.String("error", err.Error()))
			return
		}
		deleted += len(objs)
	}
	if deleted > 0 {
		log.Info("account-purge: s3 prefix purged",
			slog.String("bucket", bucket), slog.String("prefix", prefix), slog.Int("objects", deleted))
	}
}

// ---- IoT helpers ----

// teardownIoT detaches and deletes a device's IoT material. Everything is
// best-effort: devices provisioned before M5's IoT flow have no
// thingName/cert at all, and a purge re-run finds resources already gone.
func (p *Purger) teardownIoT(ctx context.Context, log *slog.Logger, d store.Device) {
	if p.IoT == nil || (d.ThingName == "" && d.CertArn == "" && d.CertID == "") {
		return
	}
	log = log.With(slog.String("deviceId", d.DeviceID), slog.String("thingName", d.ThingName))

	if d.CertArn != "" {
		if out, err := p.IoT.ListAttachedPolicies(ctx, &iot.ListAttachedPoliciesInput{
			Target: aws.String(d.CertArn),
		}); err != nil {
			logIoT(log, "list attached policies", err)
		} else {
			for _, pol := range out.Policies {
				if _, err := p.IoT.DetachPolicy(ctx, &iot.DetachPolicyInput{
					PolicyName: pol.PolicyName,
					Target:     aws.String(d.CertArn),
				}); err != nil {
					logIoT(log, "detach policy", err)
				}
			}
		}
	}
	if d.ThingName != "" && d.CertArn != "" {
		if _, err := p.IoT.DetachThingPrincipal(ctx, &iot.DetachThingPrincipalInput{
			ThingName: aws.String(d.ThingName),
			Principal: aws.String(d.CertArn),
		}); err != nil {
			logIoT(log, "detach thing principal", err)
		}
	}
	if d.CertID != "" {
		if _, err := p.IoT.UpdateCertificate(ctx, &iot.UpdateCertificateInput{
			CertificateId: aws.String(d.CertID),
			NewStatus:     "INACTIVE",
		}); err != nil {
			logIoT(log, "deactivate certificate", err)
		}
		if _, err := p.IoT.DeleteCertificate(ctx, &iot.DeleteCertificateInput{
			CertificateId: aws.String(d.CertID),
			ForceDelete:   true,
		}); err != nil {
			logIoT(log, "delete certificate", err)
		}
	}
	if d.ThingName != "" {
		if _, err := p.IoT.DeleteThing(ctx, &iot.DeleteThingInput{
			ThingName: aws.String(d.ThingName),
		}); err != nil {
			logIoT(log, "delete thing", err)
		} else {
			log.Info("account-purge: iot thing deleted")
		}
	}
}

func logIoT(log *slog.Logger, op string, err error) {
	var nf *iottypes.ResourceNotFoundException
	if errors.As(err, &nf) {
		return // already gone — idempotent re-run
	}
	log.Warn("account-purge: iot "+op+" failed", slog.String("error", err.Error()))
}

// ---- email confirmation ----

func (p *Purger) sendConfirmation(ctx context.Context, log *slog.Logger, userID, email string) {
	if email == "" {
		log.Warn("account-purge: no email captured; skipping confirmation")
		return
	}
	if p.SQS == nil || p.EmailQueueURL == "" {
		log.Warn("account-purge: email queue not configured; skipping confirmation")
		return
	}

	// Exactly-once across async retries: a conditional IDEMP marker wins
	// only for the first successful run.
	err := p.Store.ConditionalPut(ctx, "IDEMP#account-purge-"+userID, "IDEMP",
		map[string]any{"purpose": "account-purge-confirmation"},
		time.Now().Add(emailIdempotencyTTL).Unix())
	if errors.Is(err, store.ErrAlreadyExists) {
		log.Info("account-purge: confirmation already sent (idempotent re-run)")
		return
	}
	if err != nil {
		log.Warn("account-purge: idempotency marker write failed; sending anyway",
			slog.String("error", err.Error()))
	}

	body, err := json.Marshal(emailMessage{
		Template: "account-deleted",
		To:       email,
		Subject:  "Your Live Ninja account has been deleted",
		Text: "Your Live Ninja account and all associated data (conversations, " +
			"settings, devices, memories, wake words, and files) have been " +
			"permanently deleted as you requested.\n\nIf you did not request " +
			"this, reply to this email immediately.\n",
	})
	if err != nil {
		log.Error("account-purge: marshal confirmation email failed", slog.String("error", err.Error()))
		return
	}
	if _, err := p.SQS.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(p.EmailQueueURL),
		MessageBody: aws.String(string(body)),
	}); err != nil {
		log.Warn("account-purge: enqueue confirmation failed", slog.String("error", err.Error()))
		return
	}
	log.Info("account-purge: confirmation email enqueued")
}

// ---- entrypoint ----

func main() {
	cfg := config.FromEnv()
	logger := observ.NewLogger(os.Stdout, cfg.LogLevel)

	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		logger.Error("account-purge: load aws config failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	ddbClient := dynamodb.NewFromConfig(awsCfg)
	p := &Purger{
		Log:                logger,
		DDB:                ddbClient,
		S3:                 s3.NewFromConfig(awsCfg),
		IoT:                iot.NewFromConfig(awsCfg),
		SQS:                sqs.NewFromConfig(awsCfg),
		Store:              store.NewWithClient(ddbClient, cfg.TableName),
		Table:              cfg.TableName,
		DeliverablesBucket: os.Getenv("DELIVERABLES_BUCKET"),
		UserBucket:         os.Getenv("USER_BUCKET"),
		WakewordsBucket:    os.Getenv("WAKEWORDS_BUCKET"),
		EmailQueueURL:      cfg.EmailQueueURL,
		Sleep:              time.Sleep,
	}

	awslambda.Start(func(ctx context.Context, ev Event) error {
		return p.Run(ctx, ev)
	})
}
