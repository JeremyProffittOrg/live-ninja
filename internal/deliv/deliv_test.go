package deliv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// ---- fakes (interface seams from deliv.go) ----

type fakeS3 struct {
	objects    map[string][]byte // key -> body
	types      map[string]string // key -> content type
	putErr     error
	deleteErr  error
	deleted    []string
	putCalls   int
	delCalls   int
	lastBucket string
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, types: map[string]string{}}
}

func (f *fakeS3) PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putCalls++
	if f.putErr != nil {
		return nil, f.putErr
	}
	b, err := io.ReadAll(params.Body)
	if err != nil {
		return nil, err
	}
	f.lastBucket = aws.ToString(params.Bucket)
	f.objects[aws.ToString(params.Key)] = b
	f.types[aws.ToString(params.Key)] = aws.ToString(params.ContentType)
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.delCalls++
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	key := aws.ToString(params.Key)
	f.deleted = append(f.deleted, key)
	delete(f.objects, key)
	return &s3.DeleteObjectOutput{}, nil
}

type fakePresign struct {
	lastInput *s3.GetObjectInput
	err       error
}

func (f *fakePresign) PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastInput = params
	return &v4.PresignedHTTPRequest{
		URL: "https://signed.example/" + aws.ToString(params.Key) + "?sig=abc",
	}, nil
}

type fakeLambda struct {
	invokes []*lambda.InvokeInput
	err     error
}

func (f *fakeLambda) Invoke(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.invokes = append(f.invokes, params)
	return &lambda.InvokeOutput{StatusCode: 202}, nil
}

// failingCreateStore wraps the real store to force an index-write failure
// (exercises the orphan-cleanup path in Create).
type failingCreateStore struct {
	ItemStore
}

func (f *failingCreateStore) CreateDeliverable(ctx context.Context, d *store.Deliverable) error {
	return errors.New("boom: index down")
}

// ---- harness ----

type harness struct {
	svc     *Service
	s3      *fakeS3
	presign *fakePresign
	lam     *fakeLambda
	st      *store.Store
	emails  []string // "to|subject" per enqueued email
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	h := &harness{
		s3:      newFakeS3(),
		presign: &fakePresign{},
		lam:     &fakeLambda{},
		st:      store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja-test"),
	}
	cfg := Config{
		S3:       h.s3,
		Presign:  h.presign,
		Lambda:   h.lam,
		Store:    h.st,
		Bucket:   "deliv-bucket",
		ZipperFn: "zipper-fn",
		EnqueueEmail: func(ctx context.Context, template, to, subject, text string) error {
			h.emails = append(h.emails, to+"|"+subject)
			return nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	svc, err := New(cfg)
	require.NoError(t, err)
	h.svc = svc
	return h
}

// ---- Create ----

func TestCreateStoresObjectAndIndexItem(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)

	d, err := h.svc.Create(ctx, "u1", "trip plan.md", "text/markdown; charset=utf-8", []byte("# Plan"))
	require.NoError(t, err)
	assert.Equal(t, "trip-plan.md", d.Name)
	assert.Equal(t, store.DeliverableKindFile, d.Kind)
	assert.Equal(t, store.DeliverableStatusReady, d.Status)
	assert.Equal(t, int64(6), d.SizeBytes)
	assert.Equal(t, UserPrefix("u1")+d.DeliverableID+"/trip-plan.md", d.S3Key)

	// Object landed in the right bucket/key with the right content type.
	assert.Equal(t, "deliv-bucket", h.s3.lastBucket)
	assert.Equal(t, []byte("# Plan"), h.s3.objects[d.S3Key])
	assert.Equal(t, "text/markdown; charset=utf-8", h.s3.types[d.S3Key])

	// Index item is readable back through the store.
	got, err := h.st.GetDeliverable(ctx, "u1", d.DeliverableID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, d.S3Key, got.S3Key)
}

func TestCreateRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)

	_, err := h.svc.Create(ctx, "", "a.md", "text/plain", []byte("x"))
	require.ErrorIs(t, err, ErrBadInput)
	_, err = h.svc.Create(ctx, "u1", "a.md", "text/plain", nil)
	require.ErrorIs(t, err, ErrBadInput)
	_, err = h.svc.Create(ctx, "u1", "a.md", "  ", []byte("x"))
	require.ErrorIs(t, err, ErrBadInput)
	_, err = h.svc.Create(ctx, "u1", "a.md", "text/plain", bytes.Repeat([]byte("x"), MaxContentBytes+1))
	require.ErrorIs(t, err, ErrBadInput)
	assert.Zero(t, h.s3.putCalls, "no invalid input may reach S3")
}

func TestCreateCleansUpOrphanOnIndexFailure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) {
		c.Store = &failingCreateStore{ItemStore: nil}
	})

	_, err := h.svc.Create(ctx, "u1", "a.md", "text/plain", []byte("x"))
	require.Error(t, err)
	require.Len(t, h.s3.deleted, 1, "the orphaned object must be deleted")
	assert.True(t, strings.HasPrefix(h.s3.deleted[0], UserPrefix("u1")))
	assert.Empty(t, h.s3.objects, "no object may survive a failed index write")
}

// ---- Zip ----

func TestZipCreatesPendingItemAndInvokesZipper(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)

	a, err := h.svc.Create(ctx, "u1", "a.md", "text/markdown", []byte("A"))
	require.NoError(t, err)
	b, err := h.svc.Create(ctx, "u1", "b.md", "text/markdown", []byte("B"))
	require.NoError(t, err)

	z, err := h.svc.Zip(ctx, "u1", []string{a.DeliverableID, b.DeliverableID}, "bundle")
	require.NoError(t, err)
	assert.Equal(t, "bundle.zip", z.Name)
	assert.Equal(t, store.DeliverableKindZip, z.Kind)
	assert.Equal(t, store.DeliverableStatusPending, z.Status)
	assert.Equal(t, []string{a.DeliverableID, b.DeliverableID}, z.Sources)
	assert.Equal(t, "application/zip", z.ContentType)

	// The async invoke carried a valid, prefix-safe ZipJob.
	require.Len(t, h.lam.invokes, 1)
	in := h.lam.invokes[0]
	assert.Equal(t, "zipper-fn", aws.ToString(in.FunctionName))
	assert.EqualValues(t, "Event", in.InvocationType)
	var job ZipJob
	require.NoError(t, json.Unmarshal(in.Payload, &job))
	require.NoError(t, job.Validate())
	assert.Equal(t, "u1", job.UserID)
	assert.Equal(t, z.DeliverableID, job.DeliverableID)
	assert.Equal(t, z.SK(), job.SK)
	assert.Equal(t, z.S3Key, job.Key)
	require.Len(t, job.Sources, 2)
	assert.Equal(t, a.S3Key, job.Sources[0].Key)
	assert.Equal(t, "a.md", job.Sources[0].Name)

	// The pending item is visible in the listing.
	got, err := h.st.GetDeliverable(ctx, "u1", z.DeliverableID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, store.DeliverableStatusPending, got.Status)
}

func TestZipValidatesSources(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)

	a, err := h.svc.Create(ctx, "u1", "a.md", "text/markdown", []byte("A"))
	require.NoError(t, err)

	// Empty / duplicate / too many ids.
	_, err = h.svc.Zip(ctx, "u1", nil, "")
	require.ErrorIs(t, err, ErrBadInput)
	_, err = h.svc.Zip(ctx, "u1", []string{a.DeliverableID, a.DeliverableID}, "")
	require.ErrorIs(t, err, ErrBadInput)
	many := make([]string, MaxZipSources+1)
	for i := range many {
		many[i] = a.DeliverableID
	}
	_, err = h.svc.Zip(ctx, "u1", many, "")
	require.ErrorIs(t, err, ErrBadInput)

	// Unknown id, and another user's id, are both not-found.
	_, err = h.svc.Zip(ctx, "u1", []string{"ghost"}, "")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = h.svc.Zip(ctx, "u2", []string{a.DeliverableID}, "")
	require.ErrorIs(t, err, ErrNotFound)

	// A pending source cannot be bundled.
	pending := &store.Deliverable{
		DeliverableID: "p1", UserID: "u1", Name: "p.zip",
		ContentType: "application/zip", Kind: store.DeliverableKindZip,
		Status: store.DeliverableStatusPending,
		S3Key:  UserPrefix("u1") + "p1/p.zip", CreatedAt: "2026-07-17T11:00:00Z",
	}
	require.NoError(t, h.st.CreateDeliverable(ctx, pending))
	_, err = h.svc.Zip(ctx, "u1", []string{"p1"}, "")
	require.ErrorIs(t, err, ErrNotReady)

	// A corrupted index item whose key escapes the caller's prefix fails closed.
	escaped := &store.Deliverable{
		DeliverableID: "e1", UserID: "u1", Name: "e.md",
		ContentType: "text/markdown", Kind: store.DeliverableKindFile,
		Status: store.DeliverableStatusReady,
		S3Key:  "deliverables/other-user/e1/e.md", CreatedAt: "2026-07-17T11:01:00Z",
	}
	require.NoError(t, h.st.CreateDeliverable(ctx, escaped))
	_, err = h.svc.Zip(ctx, "u1", []string{"e1"}, "")
	require.ErrorIs(t, err, ErrKeyEscape)

	assert.Empty(t, h.lam.invokes, "no invalid zip request may reach the zipper")
}

func TestZipInvokeFailureMarksItemFailed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	h.lam.err = errors.New("lambda down")

	a, err := h.svc.Create(ctx, "u1", "a.md", "text/markdown", []byte("A"))
	require.NoError(t, err)

	_, err = h.svc.Zip(ctx, "u1", []string{a.DeliverableID}, "bundle")
	require.Error(t, err)

	// The pending item must not dangle: it is flipped to failed.
	items, _, lerr := h.st.ListDeliverables(ctx, "u1", 10, "")
	require.NoError(t, lerr)
	var zipItem *store.Deliverable
	for i := range items {
		if items[i].Kind == store.DeliverableKindZip {
			zipItem = &items[i]
		}
	}
	require.NotNil(t, zipItem)
	assert.Equal(t, store.DeliverableStatusFailed, zipItem.Status)
}

// ---- Presign / Deliver ----

func TestPresignDownload(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)

	d, err := h.svc.Create(ctx, "u1", "report.md", "text/markdown", []byte("hi"))
	require.NoError(t, err)

	res, err := h.svc.PresignDownload(ctx, "u1", d.DeliverableID)
	require.NoError(t, err)
	assert.Contains(t, res.URL, d.S3Key)
	assert.Equal(t, time.Date(2026, 7, 17, 12, 15, 0, 0, time.UTC), res.ExpiresAt)
	require.NotNil(t, h.presign.lastInput)
	assert.Equal(t, `attachment; filename="report.md"`,
		aws.ToString(h.presign.lastInput.ResponseContentDisposition))

	// Absent id / other user's id → ErrNotFound.
	_, err = h.svc.PresignDownload(ctx, "u1", "ghost")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = h.svc.PresignDownload(ctx, "u2", d.DeliverableID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPresignDownloadRefusesNotReadyAndEscapedKeys(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)

	pending := &store.Deliverable{
		DeliverableID: "p1", UserID: "u1", Name: "p.zip",
		ContentType: "application/zip", Kind: store.DeliverableKindZip,
		Status: store.DeliverableStatusPending,
		S3Key:  UserPrefix("u1") + "p1/p.zip", CreatedAt: "2026-07-17T11:00:00Z",
	}
	require.NoError(t, h.st.CreateDeliverable(ctx, pending))
	_, err := h.svc.PresignDownload(ctx, "u1", "p1")
	require.ErrorIs(t, err, ErrNotReady)

	escaped := &store.Deliverable{
		DeliverableID: "e1", UserID: "u1", Name: "e.md",
		ContentType: "text/markdown", Kind: store.DeliverableKindFile,
		Status: store.DeliverableStatusReady,
		S3Key:  "wakewords/whatever.onnx", CreatedAt: "2026-07-17T11:01:00Z",
	}
	require.NoError(t, h.st.CreateDeliverable(ctx, escaped))
	_, err = h.svc.PresignDownload(ctx, "u1", "e1")
	require.ErrorIs(t, err, ErrKeyEscape)
	assert.Nil(t, h.presign.lastInput, "no presign may happen for refused downloads")
}

func TestDeliverWithEmail(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)

	d, err := h.svc.Create(ctx, "u1", "report.md", "text/markdown", []byte("hi"))
	require.NoError(t, err)

	// Without an email, no message is enqueued.
	res, err := h.svc.Deliver(ctx, "u1", d.DeliverableID, "")
	require.NoError(t, err)
	assert.Empty(t, res.EmailedTo)
	assert.Empty(t, h.emails)

	// With an email, exactly one message goes to the queue.
	res, err = h.svc.Deliver(ctx, "u1", d.DeliverableID, "user@example.com")
	require.NoError(t, err)
	assert.Equal(t, "user@example.com", res.EmailedTo)
	require.Len(t, h.emails, 1)
	assert.True(t, strings.HasPrefix(h.emails[0], "user@example.com|"))
	assert.Contains(t, h.emails[0], "report.md")
}

func TestDeliverEmailFailureSurfaces(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) {
		c.EnqueueEmail = func(ctx context.Context, template, to, subject, text string) error {
			return errors.New("queue down")
		}
	})
	d, err := h.svc.Create(ctx, "u1", "a.md", "text/markdown", []byte("x"))
	require.NoError(t, err)
	_, err = h.svc.Deliver(ctx, "u1", d.DeliverableID, "user@example.com")
	require.Error(t, err)
}

// ---- Delete ----

func TestDeleteRemovesObjectAndItem(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)

	d, err := h.svc.Create(ctx, "u1", "a.md", "text/markdown", []byte("x"))
	require.NoError(t, err)

	require.NoError(t, h.svc.Delete(ctx, "u1", d.DeliverableID))
	assert.Contains(t, h.s3.deleted, d.S3Key)
	got, err := h.st.GetDeliverable(ctx, "u1", d.DeliverableID)
	require.NoError(t, err)
	assert.Nil(t, got)

	// Absent id and cross-user id → ErrNotFound.
	require.ErrorIs(t, h.svc.Delete(ctx, "u1", d.DeliverableID), ErrNotFound)
	d2, err := h.svc.Create(ctx, "u2", "b.md", "text/markdown", []byte("y"))
	require.NoError(t, err)
	require.ErrorIs(t, h.svc.Delete(ctx, "u1", d2.DeliverableID), ErrNotFound)
}

func TestDeleteToleratesS3Failure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)

	d, err := h.svc.Create(ctx, "u1", "a.md", "text/markdown", []byte("x"))
	require.NoError(t, err)
	h.s3.deleteErr = errors.New("s3 down")

	// The index row (user-visible truth) still goes away; the object is
	// left for the bucket lifecycle to reap.
	require.NoError(t, h.svc.Delete(ctx, "u1", d.DeliverableID))
	got, err := h.st.GetDeliverable(ctx, "u1", d.DeliverableID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// ---- filenames ----

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"report.md":          "report.md",
		"trip plan.md":       "trip-plan.md",
		"../../etc/passwd":   "etcpasswd",
		"..\\..\\evil.txt":   "evil.txt",
		"  spaced  ":         "spaced",
		"":                   "file",
		"™©®":                "file",
		".hidden":            "hidden",
		"weird\"quo'tes.csv": "weirdquotes.csv",
	}
	for in, want := range cases {
		assert.Equal(t, want, SanitizeFilename(in), "input %q", in)
	}
	long := strings.Repeat("a", 300) + ".md"
	got := SanitizeFilename(long)
	assert.LessOrEqual(t, len(got), 100)
}

func TestZipFileName(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, "vacation-docs.zip", zipFileName("vacation docs", now))
	assert.Equal(t, "already.zip", zipFileName("already.zip", now))
	assert.Equal(t, "UPPER.ZIP", zipFileName("UPPER.ZIP", now))
	assert.Equal(t, "deliverables-20260717-120000.zip", zipFileName("", now))
	assert.Equal(t, "deliverables-20260717-120000.zip", zipFileName("   ", now))
}
