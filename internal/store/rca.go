package store

// Tool-failure RCA persistence (M17, plan.md WS-2). Four item families, all
// single-partition key lookups or Queries — never a Scan:
//
//	RCA:       pk=RCA#<tool>#<errorCode>  sk=AT#<occurredAt RFC3339Nano>#<rcaId>
//	           one analysis (or one recorded-but-unanalysed failure), 30d TTL
//	COOLDOWN:  pk=RCA#<tool>#<errorCode>  sk=COOLDOWN#<signature>
//	           the 1-analysis-per-hour-per-signature marker
//	BUDGET:    pk=RCA#BUDGET              sk=DAY#<yyyy-mm-dd>
//	           the atomic per-UTC-day analysis counter (the cost control)
//	NOTICE:    pk=RCA#NOTICE              sk=KIND#<kind>
//	           the once-per-window operational-notification marker
//	PROFSUGG:  pk=USER#<uid>              sk=PROFSUGG#<createdAt>#<suggId>
//	           one pending base-knowledge proposal (M16's approval queue)
//
// The cooldown/budget/notice claims are the interesting part: each is ONE
// atomic conditional write, so two analyzer invocations racing the same
// failure cannot both proceed. There is deliberately no read-then-write
// anywhere in this file — a "read the marker, decide, write it back" shape
// would let a duplicate SQS delivery slip a second Opus call through the
// window it exists to close.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ---- key builders and retention (single source of truth for RCA keys) ----

const (
	// rcaBudgetPK / rcaNoticePK are the two singleton RCA partitions (one
	// counter per day, one marker per notice kind). They deliberately sit
	// under the same "RCA#" leading-key prefix as the per-family partitions
	// so ONE IAM LeadingKeys condition ("RCA#*") covers the analyzer's whole
	// read/write surface outside the user partition.
	rcaBudgetPK = "RCA#BUDGET"
	rcaNoticePK = "RCA#NOTICE"

	rcaRecordSKPrefix   = "AT#"
	rcaCooldownSKPrefix = "COOLDOWN#"
	rcaBudgetSKPrefix   = "DAY#"
	rcaNoticeSKPrefix   = "KIND#"

	// profSuggSKPrefix keys the M16 pending-suggestion queue inside the
	// user's own partition (see ProfileSuggestion's doc comment for why it
	// lives there rather than in a PROFSUGG#<uid> partition of its own).
	profSuggSKPrefix = "PROFSUGG#"
)

const (
	// rcaTTL bounds how long an RCA record survives. 30 days is long enough
	// that a failure recorded while Bedrock model access was still pending
	// can be re-run by hand after the grant lands, and short enough that the
	// table never accumulates diagnostics indefinitely.
	rcaTTL = 30 * 24 * time.Hour

	// profSuggTTL bounds a pending suggestion. An un-approved proposal is
	// stale after a month; the owner approving it writes a real settings
	// document, which has no TTL.
	profSuggTTL = 30 * 24 * time.Hour

	// markerTTLGrace is added to a claim marker's own expiresAt before it is
	// stored as the DynamoDB `ttl`. Correctness never depends on TTL
	// deletion (see ClaimRCACooldown) — this only bounds table growth, and
	// the grace keeps DynamoDB's own sweep (which can lag up to 48h) from
	// ever being the thing that decides whether a window is open.
	markerTTLGrace = 7 * 24 * time.Hour
)

func rcaRecordSK(occurredAt, rcaID string) string {
	return rcaRecordSKPrefix + occurredAt + "#" + rcaID
}

func profSuggSK(createdAt, suggID string) string {
	return profSuggSKPrefix + createdAt + "#" + suggID
}

// ---- item shapes ----

// RCA record statuses. Exactly one is stamped on every RCARecord: either the
// analysis succeeded, or the record explains why it could not.
const (
	// RCAStatusAnalyzed — Opus returned a parseable structured report.
	RCAStatusAnalyzed = "analyzed"
	// RCAStatusModelUnavailable — the account cannot call the configured
	// model at all (access not granted, or the id does not exist here). The
	// context is recorded so the analysis can be re-run once access lands.
	RCAStatusModelUnavailable = "model_unavailable"
	// RCAStatusMalformedResponse — the model replied, but no JSON report
	// could be extracted (RawResponse carries the reply verbatim).
	RCAStatusMalformedResponse = "malformed_response"
	// RCAStatusUpstreamError — a genuine, non-retryable Bedrock failure.
	RCAStatusUpstreamError = "upstream_error"
)

// RCARecord is one root-cause analysis, or one recorded failure that could
// not be analysed. Family partition so "the last N for this tool+errorCode"
// is a single-partition Query.
//
//	pk  = "RCA#<tool>#<errorCode>"                (rca.Family)
//	sk  = "AT#<occurredAt RFC3339Nano>#<rcaId>"   (sortable; Query desc = newest first)
//	ttl = createdAt + 30d
type RCARecord struct {
	PK string `dynamodbav:"pk"`
	SK string `dynamodbav:"sk"`

	RCAID     string `dynamodbav:"rcaId"`  // 12 lowercase hex chars
	Status    string `dynamodbav:"status"` // see RCAStatus* above
	Tool      string `dynamodbav:"tool"`
	ErrorCode string `dynamodbav:"errorCode"`
	Signature string `dynamodbav:"signature"`

	// --- gathered context (always populated, whatever the status) ---
	RequestedTool string `dynamodbav:"requestedTool,omitempty"`
	ErrorMessage  string `dynamodbav:"errorMessage"`
	ArgsJSON      string `dynamodbav:"args"`
	TxID          string `dynamodbav:"txId,omitempty"`
	CallID        string `dynamodbav:"callId,omitempty"`
	UserID        string `dynamodbav:"userId"`
	SessionID     string `dynamodbav:"sessionId,omitempty"`
	Surface       string `dynamodbav:"surface,omitempty"`
	// Engine is recovered from the transcript window, not from the failure
	// envelope: the session's voice engine never reaches the tool router
	// (writeAudit stamps a constant engine="tool-router" instead).
	Engine        string `dynamodbav:"engine,omitempty"`
	TurnsInWindow int    `dynamodbav:"turnsInWindow"`
	OccurredAt    string `dynamodbav:"occurredAt"`
	CreatedAt     string `dynamodbav:"createdAt"`

	// --- model + analysis ---
	ModelID string `dynamodbav:"modelId"`
	// PromptSHA256 is the full 64-hex digest of the rendered prompt, which
	// makes prompt drift auditable: two records with the same signature and
	// different digests mean the context-gathering code changed between them.
	PromptSHA256  string   `dynamodbav:"promptSha256"`
	StopReason    string   `dynamodbav:"stopReason,omitempty"`
	InputTokens   int      `dynamodbav:"inputTokens,omitempty"`
	OutputTokens  int      `dynamodbav:"outputTokens,omitempty"`
	Symptom       string   `dynamodbav:"symptom,omitempty"`
	RootCause     string   `dynamodbav:"rootCause,omitempty"`
	Evidence      []string `dynamodbav:"evidence,omitempty"`
	Confidence    string   `dynamodbav:"confidence,omitempty"` // low|medium|high
	CodeFixes     []string `dynamodbav:"codeFixSuggestions,omitempty"`
	ReproSteps    []string `dynamodbav:"reproSteps,omitempty"`
	SuggestionIDs []string `dynamodbav:"suggestionIds,omitempty"` // PROFSUGG# ids filed from this RCA

	// --- degradation bookkeeping ---
	// DegradeReason is the AWS error shape that produced a non-analyzed
	// status; RawResponse is the model's reply verbatim and is ONLY set for
	// status=malformed_response, so a bad extraction is debuggable without
	// re-running (and re-paying for) the call.
	DegradeReason string `dynamodbav:"degradeReason,omitempty"`
	RawResponse   string `dynamodbav:"rawResponse,omitempty"`

	Emailed         bool   `dynamodbav:"emailed"`
	EmailMessageID  string `dynamodbav:"emailMessageId,omitempty"`
	SuppressedCount int    `dynamodbav:"suppressedCount,omitempty"`

	TTL int64 `dynamodbav:"ttl"`
}

// Suggestion lifecycle states (M16's approval queue).
const (
	SuggestionStatusPending  = "pending"
	SuggestionStatusApproved = "approved"
	SuggestionStatusRejected = "rejected"
)

// ProfileSuggestion is one pending base-knowledge proposal (M16's
// PROFSUGG# queue, written by M17's RCA pipeline and, later, by M16's
// profile_suggest tool).
//
//	pk  = "USER#<userId>"
//	sk  = "PROFSUGG#<createdAt RFC3339Nano>#<suggId>"
//	ttl = createdAt + 30d
//
// *** M16 MUST READ EXACTLY THESE ATTRIBUTE NAMES. *** The Settings "About
// you" pending list Queries pk=USER#<uid> with begins_with(sk,"PROFSUGG#"),
// ScanIndexForward=false, and renders Field/CurrentValue/ProposedValue/Reason;
// Approve turns Field+ProposedValue into a normal versioned settings PUT and
// then UpdateItem's Status to "approved".
//
// It lives in the user's own partition (not a PROFSUGG#<uid> partition) for
// two reasons: cmd/account-purge deletes the whole USER#<uid> partition, so
// right-to-delete covers suggestions for free; and every other per-user
// collection in this table (PERSONA#, DELIV#, TOPIC#, ENT#) is keyed the same
// way. The IAM cost of that choice — the RCA role's PutItem grant cannot be
// narrowed below USER#* — is why PutProfileSuggestion is create-only.
type ProfileSuggestion struct {
	PK string `dynamodbav:"pk"`
	SK string `dynamodbav:"sk"`

	SuggID        string `dynamodbav:"suggId"` // 12 lowercase hex chars
	Status        string `dynamodbav:"status"` // pending|approved|rejected
	Field         string `dynamodbav:"field"`  // dotted settings path, allowlisted by the writer
	CurrentValue  string `dynamodbav:"currentValue"`
	ProposedValue string `dynamodbav:"proposedValue"`
	Reason        string `dynamodbav:"reason"`
	Source        string `dynamodbav:"source"`    // "rca" | "assistant" | "owner"
	SourceRef     string `dynamodbav:"sourceRef"` // rcaId for source=rca; sessionId for assistant
	CreatedAt     string `dynamodbav:"createdAt"`
	UpdatedAt     string `dynamodbav:"updatedAt"`

	// AutoApplied marks a row that was written ALREADY APPLIED under the M16
	// auto-apply policy (units / a notes[] addition the owner spoke
	// explicitly — see AutoApplyableProfileField). Such a row is created with
	// Status=approved and no ResolvedAt: the owner has not decided anything
	// yet, they are being *told*, and the drawer offers Keep or Undo.
	AutoApplied bool `dynamodbav:"autoApplied,omitempty"`
	// ResolvedAt is the resolve-once guard, stamped by
	// ResolveProfileSuggestion. Empty means "still needs the owner": either
	// pending approval, or auto-applied and not yet acknowledged.
	ResolvedAt string `dynamodbav:"resolvedAt,omitempty"`

	TTL int64 `dynamodbav:"ttl"`
}

// ---- claims (each one atomic conditional write) ----

// ClaimRCACooldown atomically claims the analysis slot for one failure
// signature. It returns true when the caller may proceed with an analysis and
// false when an analysis for this signature already ran inside the window.
//
// Implemented as a single conditional PutItem on
// pk=<family>, sk="COOLDOWN#<signature>" with
//
//	attribute_not_exists(pk) OR expiresAt < :now
//
// Correctness never depends on DynamoDB TTL deletion: TTL sweeps can lag up
// to 48h, so an expired-but-undeleted marker must still LOSE the condition
// on its own stored expiresAt. The `ttl` written alongside is expiresAt + 7d
// purely to bound table growth.
func (s *Store) ClaimRCACooldown(ctx context.Context, family, signature string, now time.Time, cooldown time.Duration) (bool, error) {
	if family == "" || signature == "" {
		return false, errors.New("store: family and signature are required")
	}
	return s.claimWindow(ctx, family, rcaCooldownSKPrefix+signature,
		map[string]types.AttributeValue{
			"signature":  &types.AttributeValueMemberS{Value: signature},
			"lastSeenAt": &types.AttributeValueMemberS{Value: now.UTC().Format(time.RFC3339Nano)},
		}, now, cooldown)
}

// ClaimRCANotice atomically claims the once-per-window slot for an
// operational notification (kind = "model_unavailable" | "malformed_response").
// Same primitive as ClaimRCACooldown — deliberately so: one code path and one
// test cover both, and "did we already tell the owner?" is exactly the same
// question as "did we already analyse this?".
func (s *Store) ClaimRCANotice(ctx context.Context, kind string, now time.Time, window time.Duration) (bool, error) {
	if kind == "" {
		return false, errors.New("store: notice kind is required")
	}
	return s.claimWindow(ctx, rcaNoticePK, rcaNoticeSKPrefix+kind,
		map[string]types.AttributeValue{
			"lastSentAt": &types.AttributeValueMemberS{Value: now.UTC().Format(time.RFC3339Nano)},
		}, now, window)
}

// claimWindow is the shared "claim this key for the next `window`" primitive
// behind ClaimRCACooldown and ClaimRCANotice.
func (s *Store) claimWindow(ctx context.Context, pk, sk string, attrs map[string]types.AttributeValue,
	now time.Time, window time.Duration) (bool, error) {
	expiresAt := now.Add(window).Unix()

	item := make(map[string]types.AttributeValue, len(attrs)+4)
	for k, v := range attrs {
		item[k] = v
	}
	item["pk"] = &types.AttributeValueMemberS{Value: pk}
	item["sk"] = &types.AttributeValueMemberS{Value: sk}
	item["expiresAt"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(expiresAt, 10)}
	item["ttl"] = &types.AttributeValueMemberN{
		Value: strconv.FormatInt(now.Add(window).Add(markerTTLGrace).Unix(), 10),
	}

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(pk) OR expiresAt < :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now": &types.AttributeValueMemberN{Value: strconv.FormatInt(now.Unix(), 10)},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return false, nil // window still open for someone else's claim
		}
		return false, fmt.Errorf("store: claim rca window %s/%s: %w", pk, sk, err)
	}
	return true, nil
}

// ClaimRCADailyBudget atomically consumes one unit of the day's RCA budget.
// It returns the post-increment count and whether the claim succeeded.
//
// One UpdateItem, no read-modify-write window:
//
//	UpdateExpression:    "SET #t = :ttl ADD #c :one"
//	ConditionExpression: "attribute_not_exists(#c) OR #c < :cap"
//
// (#c = "count", #t = "ttl"). The count is read back from the update's own
// UPDATED_NEW attributes rather than a follow-up GetItem — a second read
// could observe a *later* claim's value and report the wrong number.
func (s *Store) ClaimRCADailyBudget(ctx context.Context, day string, cap int, now time.Time) (int, bool, error) {
	if day == "" {
		return 0, false, errors.New("store: day is required")
	}
	if cap < 1 {
		return 0, false, fmt.Errorf("store: rca daily cap must be >= 1, got %d", cap)
	}

	out, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: rcaBudgetPK},
			"sk": &types.AttributeValueMemberS{Value: rcaBudgetSKPrefix + day},
		},
		UpdateExpression:    aws.String("SET #t = :ttl ADD #c :one"),
		ConditionExpression: aws.String("attribute_not_exists(#c) OR #c < :cap"),
		ExpressionAttributeNames: map[string]string{
			"#c": "count",
			"#t": "ttl",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one": &types.AttributeValueMemberN{Value: "1"},
			":cap": &types.AttributeValueMemberN{Value: strconv.Itoa(cap)},
			":ttl": &types.AttributeValueMemberN{
				Value: strconv.FormatInt(now.Add(markerTTLGrace).Unix(), 10),
			},
		},
		ReturnValues: types.ReturnValueUpdatedNew,
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return cap, false, nil // day's budget already spent
		}
		return 0, false, fmt.Errorf("store: claim rca daily budget: %w", err)
	}

	count := 0
	if n, ok := out.Attributes["count"].(*types.AttributeValueMemberN); ok {
		count, _ = strconv.Atoi(n.Value)
	}
	return count, true, nil
}

// ---- records ----

// PutRCA writes one RCA record. Unconditional: the sk carries a fresh random
// rcaId, so there is nothing to collide with — and the analyzer deliberately
// re-puts the same item once more after a successful send (to stamp
// emailed/emailMessageId), which a create-only condition would reject.
func (s *Store) PutRCA(ctx context.Context, rec *RCARecord) error {
	switch {
	case rec == nil:
		return errors.New("store: rca record is required")
	case rec.PK == "" || rec.RCAID == "" || rec.OccurredAt == "":
		return errors.New("store: rca record needs pk, rcaId and occurredAt")
	case strings.Contains(rec.RCAID, "#"):
		return errors.New("store: rcaId must not contain '#'")
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if rec.SK == "" {
		rec.SK = rcaRecordSK(rec.OccurredAt, rec.RCAID)
	}
	if rec.TTL == 0 {
		created, err := time.Parse(time.RFC3339Nano, rec.CreatedAt)
		if err != nil {
			created = time.Now().UTC()
		}
		rec.TTL = created.Add(rcaTTL).Unix()
	}

	av, err := attributevalue.MarshalMap(rec)
	if err != nil {
		return fmt.Errorf("store: marshal rca record: %w", err)
	}
	if _, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	}); err != nil {
		return fmt.Errorf("store: put rca record: %w", err)
	}
	return nil
}

// RecentRCAs returns up to limit analysed records for one family, newest
// first: a single-partition Query with begins_with(sk, "AT#") and
// ScanIndexForward=false. Never a Scan. The begins_with is load-bearing — it
// excludes the COOLDOWN# markers that share the partition.
func (s *Store) RecentRCAs(ctx context.Context, family string, limit int32) ([]RCARecord, error) {
	if family == "" {
		return nil, errors.New("store: family is required")
	}
	if limit < 1 {
		limit = 1
	}
	out, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: family},
			":pfx": &types.AttributeValueMemberS{Value: rcaRecordSKPrefix},
		},
		ScanIndexForward: aws.Bool(false), // newest first
		Limit:            aws.Int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("store: query recent rcas: %w", err)
	}
	recs := make([]RCARecord, 0, len(out.Items))
	for _, raw := range out.Items {
		var rec RCARecord
		if err := attributevalue.UnmarshalMap(raw, &rec); err != nil {
			return nil, fmt.Errorf("store: unmarshal rca record: %w", err)
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// IncrementRCASuppressed bumps suppressedCount on an existing record so the
// next report for that family can say "suppressed N similar since". A missing
// item yields ErrNotFound (condition attribute_exists(pk)) rather than
// resurrecting a ghost row; the caller swallows that — this is a reporting
// nicety, never a failure path.
func (s *Store) IncrementRCASuppressed(ctx context.Context, family, sk string) error {
	if family == "" || sk == "" {
		return errors.New("store: family and sk are required")
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: family},
			"sk": &types.AttributeValueMemberS{Value: sk},
		},
		ConditionExpression: aws.String("attribute_exists(pk)"),
		UpdateExpression:    aws.String("ADD suppressedCount :one"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one": &types.AttributeValueMemberN{Value: "1"},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrNotFound
		}
		return fmt.Errorf("store: increment rca suppressedCount: %w", err)
	}
	return nil
}

// PutProfileSuggestion writes one PROFSUGG# item, ALWAYS create-only.
//
// That is a security control, not a style choice: dynamodb:LeadingKeys can
// only constrain the PARTITION key, so the RCA role's write grant cannot be
// narrowed below USER#* — in IAM terms it also permits overwriting a
// SETTINGS or OAUTH# item in that partition. The
// attribute_not_exists(pk) AND attribute_not_exists(sk) condition plus a sort
// key built here from a server-generated random id (never from model output)
// is the structural guarantee that a bug — or a prompt injection that
// produced a different sort key — can only ever fail, never destroy an
// existing item.
func (s *Store) PutProfileSuggestion(ctx context.Context, userID string, sg *ProfileSuggestion) error {
	switch {
	case sg == nil:
		return errors.New("store: profile suggestion is required")
	case userID == "" || sg.SuggID == "" || sg.Field == "" || sg.ProposedValue == "":
		return errors.New("store: userID, suggId, field and proposedValue are required")
	case strings.Contains(sg.SuggID, "#"):
		return errors.New("store: suggId must not contain '#'")
	}
	now := time.Now().UTC()
	if sg.CreatedAt == "" {
		sg.CreatedAt = now.Format(time.RFC3339Nano)
	}
	if sg.UpdatedAt == "" {
		sg.UpdatedAt = sg.CreatedAt
	}
	if sg.Status == "" {
		sg.Status = SuggestionStatusPending
	}
	created, err := time.Parse(time.RFC3339Nano, sg.CreatedAt)
	if err != nil {
		created = now
	}
	if sg.TTL == 0 {
		sg.TTL = created.Add(profSuggTTL).Unix()
	}
	sg.PK = userPK(userID)
	sg.SK = profSuggSK(sg.CreatedAt, sg.SuggID)

	av, err := attributevalue.MarshalMap(sg)
	if err != nil {
		return fmt.Errorf("store: marshal profile suggestion: %w", err)
	}
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                av,
		ConditionExpression: aws.String("attribute_not_exists(pk) AND attribute_not_exists(sk)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("store: put profile suggestion: %w", err)
	}
	return nil
}

// ListProfileSuggestions returns the user's pending-suggestion queue newest
// first (single-partition Query on begins_with PROFSUGG#, never a Scan). M16's
// Settings "About you" drawer reads exactly this; M17 writes it.
func (s *Store) ListProfileSuggestions(ctx context.Context, userID string, limit int32) ([]ProfileSuggestion, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}
	if limit < 1 {
		limit = 50
	}
	out, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: profSuggSKPrefix},
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("store: query profile suggestions: %w", err)
	}
	sgs := make([]ProfileSuggestion, 0, len(out.Items))
	for _, raw := range out.Items {
		var sg ProfileSuggestion
		if err := attributevalue.UnmarshalMap(raw, &sg); err != nil {
			return nil, fmt.Errorf("store: unmarshal profile suggestion: %w", err)
		}
		sgs = append(sgs, sg)
	}
	return sgs, nil
}
