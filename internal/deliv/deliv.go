// Package deliv implements the M9 Deliverables Store service layer
// (FR-DLV-01..06): assistant-created files persisted per-user in the
// dedicated deliverables S3 bucket, indexed in DynamoDB (Query-only), and
// downloaded exclusively through short-lived presigned URLs.
//
// Object key discipline (the load-bearing security invariant): every
// object lives under
//
//	deliverables/<userId>/<deliverableId>/<filename>
//
// and every read path (presign, zip streaming) MUST verify the key
// carries the caller's own "deliverables/<userId>/" prefix before
// touching S3 — defense in depth on top of the DynamoDB ownership check
// in store.GetDeliverable (which already refuses cross-user id lookups).
//
// Zip bundling is asynchronous (locked M9 decision): Zip writes a
// status=pending index item and fire-and-forget invokes the
// deliverables-zipper Lambda (cmd/deliverables-zipper), which streams the
// source objects through archive/zip into a new object and flips the item
// to ready/failed. Delivery is a 15-minute presigned GET, optionally
// emailed via the shared email queue (SQS → cmd/email-dispatch → SES).
package deliv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

const (
	// KeyPrefix is the bucket-wide namespace for deliverable objects.
	KeyPrefix = "deliverables/"

	// PresignTTL is the fixed lifetime of every download URL (locked M9
	// decision: 15 minutes).
	PresignTTL = 15 * time.Minute

	// MaxContentBytes caps direct content uploads (deliverable_create is
	// text-shaped: text/markdown/html/csv — 1 MiB is far beyond any
	// realistic assistant output and bounds S3/DDB abuse).
	MaxContentBytes = 1 << 20

	// MaxZipSources bounds one zip request; each source costs a GSI1
	// Query + an S3 GET, so this also bounds the zipper's work.
	MaxZipSources = 50

	// MaxReadBytes caps the text returned by ReadContent (the file_read
	// tool): 64 KiB is plenty for the model's context, and larger files
	// come back truncated with an explicit flag so the model offers a
	// download link instead.
	MaxReadBytes = 64 << 10

	// nameLookupPageSize / maxNameLookupPages bound FindByName's walk of
	// the caller's single-partition DELIV# Query (never a Scan): up to
	// 1000 items, far beyond any real per-user corpus.
	nameLookupPageSize = 100
	maxNameLookupPages = 10

	// maxFilenameLen bounds the sanitized display filename.
	maxFilenameLen = 100

	// emailTemplateDeliverable tags queue messages from Deliver (the
	// email-dispatch consumer logs/metrics by template).
	emailTemplateDeliverable = "deliverable-link"
)

// Client-safe request errors (mapped to invalid_args / not-found shapes by
// the tool and HTTP layers).
var (
	ErrNotFound  = errors.New("deliv: deliverable not found")
	ErrNotReady  = errors.New("deliv: deliverable is not ready")
	ErrBadInput  = errors.New("deliv: invalid input")
	ErrNameTaken = errors.New("deliv: a deliverable with that name already exists") // create-only corpus: never overwritten
	ErrNotText   = errors.New("deliv: deliverable content is not readable text")
	ErrKeyEscape = errors.New("deliv: object key escapes the caller's prefix") // never expected; fail closed
)

// S3API is the narrow S3 surface the service needs (tests inject fakes).
type S3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// PresignAPI matches s3.PresignClient.PresignGetObject.
type PresignAPI interface {
	PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// LambdaAPI is the Invoke subset used for the async zipper dispatch.
type LambdaAPI interface {
	Invoke(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

// ItemStore is the deliverable index surface of *store.Store (interface
// seam so tests run against a fake without DynamoDB).
type ItemStore interface {
	CreateDeliverable(ctx context.Context, d *store.Deliverable) error
	GetDeliverable(ctx context.Context, userID, deliverableID string) (*store.Deliverable, error)
	ListDeliverables(ctx context.Context, userID string, limit int32, cursor string) ([]store.Deliverable, string, error)
	UpdateDeliverableStatus(ctx context.Context, userID, sk, status string, sizeBytes int64) error
	DeleteDeliverable(ctx context.Context, userID, sk string) error
	ClaimDeliverableName(ctx context.Context, userID, name, deliverableID string) error
	ReleaseDeliverableName(ctx context.Context, userID, name string) error
}

// EnqueueEmailFunc enqueues one email onto the shared email queue —
// signature-compatible with webapp.Deps.EnqueueEmail.
type EnqueueEmailFunc func(ctx context.Context, template, to, subject, text string) error

// Config wires a Service. Bucket + the store are mandatory; the rest
// degrade per-feature (nil Lambda → Zip fails cleanly, nil EnqueueEmail →
// email delivery unavailable).
type Config struct {
	S3           S3API
	Presign      PresignAPI
	Lambda       LambdaAPI
	Store        ItemStore
	Bucket       string // env DELIVERABLES_BUCKET
	ZipperFn     string // env ZIPPER_FUNCTION_NAME
	EnqueueEmail EnqueueEmailFunc
	Log          *slog.Logger
	Now          func() time.Time
}

// Service is the deliverables backend shared by the tool handlers
// (internal/tools/deliverable.go) and the HTTP routes
// (internal/webapp/deliverables_routes.go).
type Service struct{ cfg Config }

// New validates the hard dependencies and returns the service.
func New(cfg Config) (*Service, error) {
	if cfg.S3 == nil || cfg.Presign == nil || cfg.Store == nil {
		return nil, errors.New("deliv: S3, Presign, and Store are required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("deliv: bucket name is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg}, nil
}

// UserPrefix is the caller's private key namespace; every key the service
// reads or presigns must start with it.
func UserPrefix(userID string) string { return KeyPrefix + userID + "/" }

// keyWithinUser is the mandatory prefix check (FR-DLV: "key MUST be
// prefix-checked deliverables/<uid>/").
func keyWithinUser(userID, key string) bool {
	return userID != "" && strings.HasPrefix(key, UserPrefix(userID))
}

// ---- create ----

// Create uploads content as a new ready deliverable: one S3 object at
// deliverables/<uid>/<id>/<filename> plus its index item.
//
// The corpus is create-only (owner rule: the assistant must never
// overwrite or delete a document), so the display filename is claimed
// FIRST via a conditional DynamoDB write — the atomic authority; two
// concurrent creates of the same name race on that single conditional
// put and exactly one wins. Deliverables that predate name claims are
// then covered by a bounded Query walk. ErrNameTaken when the name is
// already in use.
func (s *Service) Create(ctx context.Context, userID, filename, contentType string, content []byte) (*store.Deliverable, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: user id is required", ErrBadInput)
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("%w: content is empty", ErrBadInput)
	}
	if len(content) > MaxContentBytes {
		return nil, fmt.Errorf("%w: content exceeds %d bytes", ErrBadInput, MaxContentBytes)
	}
	if strings.TrimSpace(contentType) == "" {
		return nil, fmt.Errorf("%w: content type is required", ErrBadInput)
	}
	name := SanitizeFilename(filename)

	now := s.cfg.Now().UTC()
	id := uuid.NewString()
	key := UserPrefix(userID) + id + "/" + name

	if err := s.claimName(ctx, userID, name, id); err != nil {
		return nil, err
	}
	releaseClaim := func() {
		if rerr := s.cfg.Store.ReleaseDeliverableName(ctx, userID, name); rerr != nil {
			s.cfg.Log.Warn("deliv: release name claim failed", "name", name, "error", rerr.Error())
		}
	}

	if _, err := s.cfg.S3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(content),
		ContentType: aws.String(contentType),
	}); err != nil {
		releaseClaim()
		return nil, fmt.Errorf("deliv: put object: %w", err)
	}

	d := &store.Deliverable{
		DeliverableID: id,
		UserID:        userID,
		Name:          name,
		ContentType:   contentType,
		Kind:          store.DeliverableKindFile,
		Status:        store.DeliverableStatusReady,
		S3Key:         key,
		SizeBytes:     int64(len(content)),
		CreatedAt:     now.Format(time.RFC3339),
	}
	if err := s.cfg.Store.CreateDeliverable(ctx, d); err != nil {
		// Index write lost after the object landed: best-effort cleanup so
		// the bucket doesn't accumulate orphans (lifecycle expiry is the
		// backstop either way), and the name claim is rolled back so the
		// name isn't locked by a create that never happened.
		if _, derr := s.cfg.S3.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key),
		}); derr != nil {
			s.cfg.Log.Warn("deliv: orphan cleanup failed", "key", key, "error", derr.Error())
		}
		releaseClaim()
		return nil, fmt.Errorf("deliv: index deliverable: %w", err)
	}
	return d, nil
}

// claimName performs the atomic filename claim plus the legacy-corpus
// check (deliverables created before name claims existed have no claim
// item, so a fresh claim alone wouldn't notice them). The conditional
// write happens first and is the authority; on any post-claim failure
// the claim is rolled back.
func (s *Service) claimName(ctx context.Context, userID, name, deliverableID string) error {
	if err := s.cfg.Store.ClaimDeliverableName(ctx, userID, name, deliverableID); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return fmt.Errorf("%w: %s", ErrNameTaken, name)
		}
		return fmt.Errorf("deliv: claim name: %w", err)
	}
	legacy, err := s.FindByName(ctx, userID, name)
	if err != nil {
		if rerr := s.cfg.Store.ReleaseDeliverableName(ctx, userID, name); rerr != nil {
			s.cfg.Log.Warn("deliv: release name claim failed", "name", name, "error", rerr.Error())
		}
		return err
	}
	if legacy != nil {
		if rerr := s.cfg.Store.ReleaseDeliverableName(ctx, userID, name); rerr != nil {
			s.cfg.Log.Warn("deliv: release name claim failed", "name", name, "error", rerr.Error())
		}
		return fmt.Errorf("%w: %s", ErrNameTaken, name)
	}
	return nil
}

// FindByName resolves the caller's deliverable by exact display filename,
// walking the single-partition DELIV# Query newest-first (never a Scan;
// bounded at maxNameLookupPages pages). Returns (nil, nil) when no
// deliverable carries the name.
func (s *Service) FindByName(ctx context.Context, userID, name string) (*store.Deliverable, error) {
	if userID == "" || name == "" {
		return nil, fmt.Errorf("%w: user id and name are required", ErrBadInput)
	}
	cursor := ""
	for page := 0; page < maxNameLookupPages; page++ {
		items, next, err := s.cfg.Store.ListDeliverables(ctx, userID, nameLookupPageSize, cursor)
		if err != nil {
			return nil, fmt.Errorf("deliv: find by name: %w", err)
		}
		for i := range items {
			if items[i].Name == name {
				return &items[i], nil
			}
		}
		if next == "" {
			return nil, nil
		}
		cursor = next
	}
	return nil, nil
}

// ---- read ----

// IsTextContentType reports whether a stored content type is safe to
// return as text to the model: text/* plus the JSON/CSV/markdown
// application aliases. Everything else (zip archives, images, unknown
// binaries) must be delivered as a download link instead.
func IsTextContentType(contentType string) bool {
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	switch mt {
	case "application/json", "application/csv", "application/markdown":
		return true
	}
	return false
}

// ReadContent fetches a ready deliverable's bytes for the file_read tool.
// Returns the resolved deliverable, up to MaxReadBytes of content, and
// whether the object was truncated at that cap. Sentinels: ErrNotFound,
// ErrNotReady, ErrKeyEscape, and ErrNotText (returned WITH the resolved
// deliverable so callers can name the offending content type) for
// non-text objects.
func (s *Service) ReadContent(ctx context.Context, userID, deliverableID string) (*store.Deliverable, []byte, bool, error) {
	d, err := s.cfg.Store.GetDeliverable(ctx, userID, deliverableID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("deliv: resolve deliverable: %w", err)
	}
	if d == nil {
		return nil, nil, false, ErrNotFound
	}
	if d.Status != store.DeliverableStatusReady {
		return d, nil, false, fmt.Errorf("%w: status %s", ErrNotReady, d.Status)
	}
	// Mandatory prefix check before any S3 read — same invariant as
	// presign/zip: a corrupted index item must never read outside the
	// caller's own namespace.
	if !keyWithinUser(userID, d.S3Key) {
		return nil, nil, false, ErrKeyEscape
	}
	if !IsTextContentType(d.ContentType) {
		return d, nil, false, fmt.Errorf("%w: %s", ErrNotText, d.ContentType)
	}

	out, err := s.cfg.S3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(d.S3Key),
	})
	if err != nil {
		return nil, nil, false, fmt.Errorf("deliv: get object: %w", err)
	}
	defer func() { _ = out.Body.Close() }()

	// Read one byte past the cap to learn whether truncation happened
	// without ever buffering more than the cap.
	data, err := io.ReadAll(io.LimitReader(out.Body, MaxReadBytes+1))
	if err != nil {
		return nil, nil, false, fmt.Errorf("deliv: read object: %w", err)
	}
	truncated := false
	if len(data) > MaxReadBytes {
		data = data[:MaxReadBytes]
		truncated = true
	}
	return d, data, truncated, nil
}

// ---- zip ----

// Zip validates and resolves the caller's source deliverables, writes a
// status=pending zip item, and asynchronously invokes the zipper Lambda
// to build the archive. The returned item is pending; clients observe
// ready/failed via the list endpoint (or the SES "ready" note is a later
// enhancement — the zipper itself owns no email today).
func (s *Service) Zip(ctx context.Context, userID string, deliverableIDs []string, zipName string) (*store.Deliverable, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: user id is required", ErrBadInput)
	}
	if len(deliverableIDs) == 0 {
		return nil, fmt.Errorf("%w: at least one deliverable id is required", ErrBadInput)
	}
	if len(deliverableIDs) > MaxZipSources {
		return nil, fmt.Errorf("%w: at most %d deliverables per zip", ErrBadInput, MaxZipSources)
	}
	if s.cfg.Lambda == nil || s.cfg.ZipperFn == "" {
		return nil, errors.New("deliv: zipper is not configured (ZIPPER_FUNCTION_NAME)")
	}

	// Resolve every source: must exist, be the caller's own, be ready, and
	// sit inside the caller's key prefix. Duplicate ids are rejected —
	// almost certainly a model slip, and silently deduping would surprise.
	seen := make(map[string]bool, len(deliverableIDs))
	sources := make([]ZipSource, 0, len(deliverableIDs))
	sourceIDs := make([]string, 0, len(deliverableIDs))
	for _, id := range deliverableIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return nil, fmt.Errorf("%w: deliverable ids must be unique and non-empty", ErrBadInput)
		}
		seen[id] = true
		d, err := s.cfg.Store.GetDeliverable(ctx, userID, id)
		if err != nil {
			return nil, fmt.Errorf("deliv: resolve source %s: %w", id, err)
		}
		if d == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		if d.Status != store.DeliverableStatusReady {
			return nil, fmt.Errorf("%w: %s is %s", ErrNotReady, id, d.Status)
		}
		if !keyWithinUser(userID, d.S3Key) {
			return nil, ErrKeyEscape
		}
		sources = append(sources, ZipSource{Key: d.S3Key, Name: d.Name})
		sourceIDs = append(sourceIDs, id)
	}

	now := s.cfg.Now().UTC()
	name := zipFileName(zipName, now)
	id := uuid.NewString()
	key := UserPrefix(userID) + id + "/" + name

	// Same create-only corpus rule as Create: the archive's name is
	// claimed atomically so a zip can never shadow or duplicate an
	// existing deliverable name.
	if err := s.claimName(ctx, userID, name, id); err != nil {
		return nil, err
	}

	d := &store.Deliverable{
		DeliverableID: id,
		UserID:        userID,
		Name:          name,
		ContentType:   "application/zip",
		Kind:          store.DeliverableKindZip,
		Status:        store.DeliverableStatusPending,
		S3Key:         key,
		CreatedAt:     now.Format(time.RFC3339),
		Sources:       sourceIDs,
	}
	if err := s.cfg.Store.CreateDeliverable(ctx, d); err != nil {
		if rerr := s.cfg.Store.ReleaseDeliverableName(ctx, userID, name); rerr != nil {
			s.cfg.Log.Warn("deliv: release name claim failed", "name", name, "error", rerr.Error())
		}
		return nil, fmt.Errorf("deliv: index zip deliverable: %w", err)
	}

	payload, err := json.Marshal(ZipJob{
		UserID:        userID,
		DeliverableID: id,
		SK:            d.SK(),
		Key:           key,
		Sources:       sources,
	})
	if err != nil {
		return nil, fmt.Errorf("deliv: marshal zip job: %w", err)
	}
	if _, err := s.cfg.Lambda.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(s.cfg.ZipperFn),
		InvocationType: lambdatypes.InvocationTypeEvent, // async, per locked M9 decision
		Payload:        payload,
	}); err != nil {
		// The pending item would dangle forever without a zipper run —
		// flip it to failed so the UI shows the truth.
		if uerr := s.cfg.Store.UpdateDeliverableStatus(ctx, userID, d.SK(), store.DeliverableStatusFailed, 0); uerr != nil {
			s.cfg.Log.Error("deliv: mark zip failed after invoke error", "error", uerr.Error())
		}
		return nil, fmt.Errorf("deliv: invoke zipper: %w", err)
	}
	return d, nil
}

// ---- deliver / download ----

// DeliverResult is the outcome of Deliver/PresignDownload.
type DeliverResult struct {
	Deliverable *store.Deliverable
	URL         string
	ExpiresAt   time.Time
	EmailedTo   string // "" when no email was sent
}

// PresignDownload resolves the caller's deliverable and mints a
// 15-minute presigned GET with an attachment Content-Disposition.
// Returns ErrNotFound / ErrNotReady for the corresponding states.
func (s *Service) PresignDownload(ctx context.Context, userID, deliverableID string) (*DeliverResult, error) {
	d, err := s.cfg.Store.GetDeliverable(ctx, userID, deliverableID)
	if err != nil {
		return nil, fmt.Errorf("deliv: resolve deliverable: %w", err)
	}
	if d == nil {
		return nil, ErrNotFound
	}
	if d.Status != store.DeliverableStatusReady {
		return nil, fmt.Errorf("%w: status %s", ErrNotReady, d.Status)
	}
	// Mandatory prefix check before any presign — even a corrupted or
	// hand-edited index item must never yield a URL outside the caller's
	// own namespace.
	if !keyWithinUser(userID, d.S3Key) {
		return nil, ErrKeyEscape
	}

	expiresAt := s.cfg.Now().UTC().Add(PresignTTL)
	req, err := s.cfg.Presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(s.cfg.Bucket),
		Key:                        aws.String(d.S3Key),
		ResponseContentDisposition: aws.String(`attachment; filename="` + d.Name + `"`),
	}, func(o *s3.PresignOptions) { o.Expires = PresignTTL })
	if err != nil {
		return nil, fmt.Errorf("deliv: presign download: %w", err)
	}
	return &DeliverResult{Deliverable: d, URL: req.URL, ExpiresAt: expiresAt}, nil
}

// Deliver mints the presigned URL and, when emailTo is non-empty, mails
// it via the shared email queue. The email is best-effort in shape but a
// failure is surfaced (the caller asked for delivery, not just a link).
func (s *Service) Deliver(ctx context.Context, userID, deliverableID, emailTo string) (*DeliverResult, error) {
	res, err := s.PresignDownload(ctx, userID, deliverableID)
	if err != nil {
		return nil, err
	}
	if emailTo == "" {
		return res, nil
	}
	if s.cfg.EnqueueEmail == nil {
		return nil, errors.New("deliv: email delivery is not configured")
	}

	d := res.Deliverable
	subject := fmt.Sprintf("Your file from Live Ninja: %s", d.Name)
	text := fmt.Sprintf(
		"Your Live Ninja deliverable %q is ready.\n\nDownload (link expires in 15 minutes):\n%s\n\nSize: %d bytes\nCreated: %s\n",
		d.Name, res.URL, d.SizeBytes, d.CreatedAt)
	if err := s.cfg.EnqueueEmail(ctx, emailTemplateDeliverable, emailTo, subject, text); err != nil {
		return nil, fmt.Errorf("deliv: enqueue delivery email: %w", err)
	}
	res.EmailedTo = emailTo
	return res, nil
}

// ---- list / delete (HTTP surface plumbing) ----

// List pages the caller's deliverables newest-first (pure pass-through to
// the store's single-partition Query).
func (s *Service) List(ctx context.Context, userID string, limit int32, cursor string) ([]store.Deliverable, string, error) {
	return s.cfg.Store.ListDeliverables(ctx, userID, limit, cursor)
}

// Delete removes the caller's deliverable: the S3 object first
// (best-effort — the lifecycle rule expires stragglers), then the index
// item. Returns ErrNotFound when the id doesn't resolve for this caller.
func (s *Service) Delete(ctx context.Context, userID, deliverableID string) error {
	d, err := s.cfg.Store.GetDeliverable(ctx, userID, deliverableID)
	if err != nil {
		return fmt.Errorf("deliv: resolve deliverable: %w", err)
	}
	if d == nil {
		return ErrNotFound
	}
	if keyWithinUser(userID, d.S3Key) {
		if _, err := s.cfg.S3.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.cfg.Bucket),
			Key:    aws.String(d.S3Key),
		}); err != nil {
			// Keep going: the index row is the user-visible truth and the
			// bucket lifecycle (180d) reaps orphaned objects.
			s.cfg.Log.Warn("deliv: delete object failed", "key", d.S3Key, "error", err.Error())
		}
	}
	if err := s.cfg.Store.DeleteDeliverable(ctx, userID, d.SK()); err != nil {
		return fmt.Errorf("deliv: delete deliverable item: %w", err)
	}
	// Free the filename claim so the name becomes creatable again
	// (releasing an unclaimed legacy name is a harmless no-op).
	if err := s.cfg.Store.ReleaseDeliverableName(ctx, userID, d.Name); err != nil {
		s.cfg.Log.Warn("deliv: release name claim failed", "name", d.Name, "error", err.Error())
	}
	return nil
}

// ---- filenames ----

// SanitizeFilename reduces an arbitrary display name to a safe object-key
// leaf: ASCII letters/digits/dot/dash/underscore only, no path
// separators, no leading dot (hidden files / ".." can never form), and a
// bounded length. Empty input degrades to "file".
func SanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		default:
			// drop everything else (path separators, control chars, quotes)
		}
	}
	out := strings.Trim(b.String(), ".-_")
	if len(out) > maxFilenameLen {
		out = out[:maxFilenameLen]
		out = strings.Trim(out, ".-_")
	}
	if out == "" {
		return "file"
	}
	return out
}

// zipFileName sanitizes/derives the archive filename, always .zip-suffixed.
func zipFileName(requested string, now time.Time) string {
	name := SanitizeFilename(requested)
	if name == "file" && strings.TrimSpace(requested) == "" {
		name = "deliverables-" + now.Format("20060102-150405")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".zip") {
		name += ".zip"
	}
	return name
}
