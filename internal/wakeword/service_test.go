package wakeword

// Service tests for the M6 role-spec scope: training-slot / queue gates
// on Create, and the lazy status finalization (S3 manifests + Batch
// DescribeJobs — the locked no-poller design) including the
// exactly-once ready email and the model-manifest read path. All AWS
// seams are the in-memory fakes below.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/batch"
	batchtypes "github.com/aws/aws-sdk-go-v2/service/batch/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// ---- fakes ----

type fakeStore struct {
	items map[string]*store.Wakeword // key: userID+"/"+id
	slots map[string]int             // key: userID+"/"+day
	idemp map[string]bool            // key: pk+"/"+sk
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		items: map[string]*store.Wakeword{},
		slots: map[string]int{},
		idemp: map[string]bool{},
	}
}

func (f *fakeStore) key(userID, id string) string { return userID + "/" + id }

func (f *fakeStore) CreateWakeword(_ context.Context, w *store.Wakeword) error {
	k := f.key(w.UserID, w.ID)
	if _, ok := f.items[k]; ok {
		return store.ErrAlreadyExists
	}
	cp := *w
	f.items[k] = &cp
	return nil
}

func (f *fakeStore) ReplaceWakeword(_ context.Context, w *store.Wakeword) error {
	cp := *w
	f.items[f.key(w.UserID, w.ID)] = &cp
	return nil
}

func (f *fakeStore) GetWakeword(_ context.Context, userID, id string) (*store.Wakeword, error) {
	w, ok := f.items[f.key(userID, id)]
	if !ok {
		return nil, nil
	}
	cp := *w // mimic a fresh unmarshal — callers never share our pointer
	return &cp, nil
}

func (f *fakeStore) ListWakewords(_ context.Context, userID string) ([]store.Wakeword, error) {
	var out []store.Wakeword
	for k, w := range f.items {
		if strings.HasPrefix(k, userID+"/") {
			out = append(out, *w)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateWakewordStatus(_ context.Context, userID, id, status, failureReason string, platforms []string, readyAt string) error {
	w, ok := f.items[f.key(userID, id)]
	if !ok {
		return store.ErrNotFound
	}
	w.Status = status
	if failureReason != "" {
		w.FailureReason = failureReason
	}
	if len(platforms) > 0 {
		w.Platforms = platforms
	}
	if readyAt != "" {
		w.ReadyAt = readyAt
	}
	return nil
}

func (f *fakeStore) SetWakewordJobID(_ context.Context, userID, id, jobID string) error {
	w, ok := f.items[f.key(userID, id)]
	if !ok {
		return store.ErrNotFound
	}
	w.BatchJobID = jobID
	return nil
}

func (f *fakeStore) DeleteWakeword(_ context.Context, userID, id string) error {
	delete(f.items, f.key(userID, id))
	return nil
}

func (f *fakeStore) TakeWakewordTrainingSlot(_ context.Context, userID, day string, max int) (bool, error) {
	k := userID + "/" + day
	if f.slots[k] >= max {
		return false, nil
	}
	f.slots[k]++
	return true, nil
}

func (f *fakeStore) ReturnWakewordTrainingSlot(_ context.Context, userID, day string) error {
	k := userID + "/" + day
	if f.slots[k] > 0 {
		f.slots[k]--
	}
	return nil
}

func (f *fakeStore) ConditionalPut(_ context.Context, pk, sk string, _ map[string]any, _ int64) error {
	k := pk + "/" + sk
	if f.idemp[k] {
		return store.ErrAlreadyExists
	}
	f.idemp[k] = true
	return nil
}

type fakeS3 struct {
	objects map[string][]byte
	deleted []string
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) HeadObject(_ context.Context, p *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if _, ok := f.objects[aws.ToString(p.Key)]; !ok {
		return nil, &s3types.NotFound{}
	}
	return &s3.HeadObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, p *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	b, ok := f.objects[aws.ToString(p.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b))}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, p *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}
	for k := range f.objects {
		if strings.HasPrefix(k, aws.ToString(p.Prefix)) {
			out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
		}
	}
	return out, nil
}

func (f *fakeS3) DeleteObjects(_ context.Context, p *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	for _, o := range p.Delete.Objects {
		k := aws.ToString(o.Key)
		delete(f.objects, k)
		f.deleted = append(f.deleted, k)
	}
	return &s3.DeleteObjectsOutput{}, nil
}

type fakePresign struct{}

func (fakePresign) PresignGetObject(_ context.Context, p *s3.GetObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	return &v4.PresignedHTTPRequest{
		URL: "https://s3.test/" + aws.ToString(p.Bucket) + "/" + aws.ToString(p.Key) + "?sig=x",
	}, nil
}

type fakeBatch struct {
	submitted   []*batch.SubmitJobInput
	nextJobID   string
	jobs        map[string]batchtypes.JobDetail // by job id; absent = record expired
	activeCount int                             // spread over the first ListJobs status
	terminated  []string
}

func newFakeBatch() *fakeBatch {
	return &fakeBatch{nextJobID: "job-1", jobs: map[string]batchtypes.JobDetail{}}
}

func (f *fakeBatch) SubmitJob(_ context.Context, p *batch.SubmitJobInput, _ ...func(*batch.Options)) (*batch.SubmitJobOutput, error) {
	f.submitted = append(f.submitted, p)
	return &batch.SubmitJobOutput{JobId: aws.String(f.nextJobID)}, nil
}

func (f *fakeBatch) DescribeJobs(_ context.Context, p *batch.DescribeJobsInput, _ ...func(*batch.Options)) (*batch.DescribeJobsOutput, error) {
	out := &batch.DescribeJobsOutput{}
	for _, id := range p.Jobs {
		if j, ok := f.jobs[id]; ok {
			out.Jobs = append(out.Jobs, j)
		}
	}
	return out, nil
}

func (f *fakeBatch) ListJobs(_ context.Context, p *batch.ListJobsInput, _ ...func(*batch.Options)) (*batch.ListJobsOutput, error) {
	out := &batch.ListJobsOutput{}
	if p.JobStatus == batchtypes.JobStatusSubmitted { // report the whole backlog under one status
		for i := 0; i < f.activeCount; i++ {
			out.JobSummaryList = append(out.JobSummaryList, batchtypes.JobSummary{
				JobId: aws.String(fmt.Sprintf("active-%d", i)),
			})
		}
	}
	return out, nil
}

func (f *fakeBatch) TerminateJob(_ context.Context, p *batch.TerminateJobInput, _ ...func(*batch.Options)) (*batch.TerminateJobOutput, error) {
	f.terminated = append(f.terminated, aws.ToString(p.JobId))
	return &batch.TerminateJobOutput{}, nil
}

// ---- harness ----

type sentMail struct {
	template, to, subject, text string
}

type testEnv struct {
	svc   *Service
	store *fakeStore
	s3    *fakeS3
	batch *fakeBatch
	mails []sentMail
	now   time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	env := &testEnv{
		store: newFakeStore(),
		s3:    newFakeS3(),
		batch: newFakeBatch(),
		now:   time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
	}
	env.svc = New(Params{
		Store:   env.store,
		S3:      env.s3,
		Presign: fakePresign{},
		Batch:   env.batch,
		Config:  Config{Bucket: "test-bucket", JobQueue: "q", JobDefinition: "jd"},
		UserEmail: func(_ context.Context, userID string) (string, error) {
			return userID + "@example.com", nil
		},
		SendEmail: func(_ context.Context, template, to, subject, text string) error {
			env.mails = append(env.mails, sentMail{template, to, subject, text})
			return nil
		},
		Now: func() time.Time { return env.now },
	})
	return env
}

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// putManifests uploads trainer-shaped manifest.json files for every
// trained platform of wwID (what containers/wakeword-train does last).
func (e *testEnv) putManifests(t *testing.T, wwID string) {
	t.Helper()
	for _, p := range trainedPlatforms {
		man := storedManifest{
			ID: wwID, Platform: p, Engine: "openwakeword",
			Format: formatForPlatform(p), Key: modelKey(wwID, p),
			SHA256: testSHA, SizeBytes: 123456,
		}
		raw, err := json.Marshal(man)
		require.NoError(t, err)
		e.s3.objects[manifestKey(wwID, p)] = raw
		e.s3.objects[modelKey(wwID, p)] = []byte("onnx-bytes")
	}
}

// putTrainerManifests writes manifests in the shape containers/wakeword-train
// actually uploads: per-artifact files map, no flat key/sha256 fields.
func (e *testEnv) putTrainerManifests(t *testing.T, wwID string) {
	t.Helper()
	for _, p := range trainedPlatforms {
		raw, err := json.Marshal(map[string]any{
			"phrase":   "hey live ninja",
			"engine":   "openwakeword",
			"platform": p,
			"format":   formatForPlatform(p),
			"files": map[string]any{
				"onnx":     map[string]any{"key": modelKey(wwID, p), "sha256": testSHA, "sizeBytes": 123456},
				"onnxFp32": map[string]any{"key": "wakewords/" + wwID + "/" + p + "/model_fp32.onnx", "sha256": testSHA, "sizeBytes": 999},
			},
		})
		require.NoError(t, err)
		e.s3.objects[manifestKey(wwID, p)] = raw
		e.s3.objects[modelKey(wwID, p)] = []byte("onnx-bytes")
	}
}

func TestModelManifestTrainerShape(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	env.putTrainerManifests(t, w.ID)
	env.svc.cache.invalidate("u1")

	man, err := env.svc.Model(ctx, "u1", w.ID, "web")
	require.NoError(t, err)
	assert.Equal(t, testSHA, man.SHA256)
	assert.EqualValues(t, 123456, man.SizeBytes)
	assert.Contains(t, man.URL, modelKey(w.ID, "web"))
}

// ---- Create ----

func TestCreateSubmitsTrainingJob(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "  Hey   Purple Parrot ", "")
	require.NoError(t, err)
	assert.Equal(t, store.WakewordStatusPending, w.Status)
	assert.Equal(t, "hey purple parrot", w.Phrase)
	assert.Equal(t, "openwakeword", w.Engine)
	assert.Equal(t, WakewordID("u1", "hey purple parrot"), w.ID)
	assert.Equal(t, "job-1", w.BatchJobID)

	// One slot consumed for today.
	assert.Equal(t, 1, env.store.slots["u1/2026-07-17"])

	// Job carried the trainer CLI contract (train.py argparse: --phrase /
	// --ww-id / --user-id) + the 20-min hard timeout.
	require.Len(t, env.batch.submitted, 1)
	sub := env.batch.submitted[0]
	require.NotNil(t, sub.Timeout)
	assert.EqualValues(t, 1200, aws.ToInt32(sub.Timeout.AttemptDurationSeconds))
	assert.Equal(t, []string{
		"--phrase", "hey purple parrot",
		"--ww-id", w.ID,
		"--user-id", "u1",
	}, sub.ContainerOverrides.Command)

	// Stored item is retrievable.
	got, err := env.store.GetWakeword(ctx, "u1", w.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "job-1", got.BatchJobID)
}

func TestCreateValidationAndEngineGates(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.svc.Create(ctx, "u1", "hey ninja99", "")
	var vErr *ValidationError
	require.ErrorAs(t, err, &vErr)

	_, err = env.svc.Create(ctx, "u1", "hey ninja", "porcupine")
	assert.ErrorIs(t, err, ErrEngineUnavailable)

	// No slots were consumed and nothing was submitted.
	assert.Empty(t, env.store.slots)
	assert.Empty(t, env.batch.submitted)
}

func TestCreateCollisions(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Builtin phrase collides ("hey jarvis" is the client-bundled model).
	_, err := env.svc.Create(ctx, "u1", "Hey Jarvis", "")
	assert.ErrorIs(t, err, ErrCollision)

	// "Hey Live Ninja" is NOT builtin (no client ships a model for it) —
	// it must be trainable through the normal custom pipeline.
	_, err = env.svc.Create(ctx, "u1", "Hey Live Ninja", "")
	require.NoError(t, err)

	// Own non-failed entry collides.
	_, err = env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	_, err = env.svc.Create(ctx, "u1", "HEY  parrot", "")
	assert.ErrorIs(t, err, ErrCollision)

	// A failed entry with the same phrase is retrainable in place.
	w := env.store.items["u1/"+WakewordID("u1", "hey parrot")]
	require.NotNil(t, w)
	w.Status = store.WakewordStatusFailed
	env.svc.cache.invalidate("u1")
	re, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	assert.Equal(t, store.WakewordStatusPending, re.Status)
	assert.Equal(t, w.ID, re.ID) // same deterministic id → same S3 prefix
}

func TestCreateDailyLimit(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	for i, phrase := range []string{"alpha bird", "beta bird", "gamma bird"} {
		_, err := env.svc.Create(ctx, "u1", phrase, "")
		require.NoError(t, err, "create %d", i)
	}
	_, err := env.svc.Create(ctx, "u1", "delta bird", "")
	assert.ErrorIs(t, err, ErrDailyLimit)
	assert.Equal(t, 3, env.store.slots["u1/2026-07-17"])

	// Next UTC day: the gate opens again.
	env.now = env.now.Add(24 * time.Hour)
	_, err = env.svc.Create(ctx, "u1", "delta bird", "")
	require.NoError(t, err)
}

func TestCreateQueueFullReturnsSlot(t *testing.T) {
	env := newTestEnv(t)
	env.batch.activeCount = 4 // == default MaxActiveJobs
	ctx := context.Background()

	_, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	assert.ErrorIs(t, err, ErrQueueFull)
	// The consumed slot was returned — the user wasn't charged for a
	// training run that never started.
	assert.Equal(t, 0, env.store.slots["u1/2026-07-17"])
	assert.Empty(t, env.batch.submitted)
}

// ---- lazy status finalization ----

func TestFinalizeReadyWhenManifestsExistAndEmailsOnce(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)

	// Trainer finishes: manifests land in S3.
	env.putManifests(t, w.ID)
	env.svc.cache.invalidate("u1")

	entry, err := env.svc.Get(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WakewordStatusReady, entry.Status)
	assert.Equal(t, []string{"web", "android"}, entry.Platforms)
	assert.NotEmpty(t, entry.ReadyAt)

	// Persisted, not just in-memory.
	stored, err := env.store.GetWakeword(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WakewordStatusReady, stored.Status)

	// Ready email sent exactly once, even across repeated finalize paths.
	require.Len(t, env.mails, 1)
	assert.Equal(t, "wakeword-ready", env.mails[0].template)
	assert.Equal(t, "u1@example.com", env.mails[0].to)
	assert.Contains(t, env.mails[0].subject, "hey parrot")

	// Force a second finalize pass (fresh pending status in the store
	// simulates a second warm container that hasn't observed ready yet).
	stored2 := env.store.items["u1/"+w.ID]
	stored2.Status = store.WakewordStatusPending
	env.svc.cache.invalidate("u1")
	_, err = env.svc.Get(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Len(t, env.mails, 1, "IDEMP marker must suppress a duplicate ready email")
}

func TestFinalizeFailedWhenJobFailed(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	env.batch.jobs["job-1"] = batchtypes.JobDetail{
		JobId:        aws.String("job-1"),
		Status:       batchtypes.JobStatusFailed,
		StatusReason: aws.String("Essential container in task exited"),
	}
	env.svc.cache.invalidate("u1")

	entry, err := env.svc.Get(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WakewordStatusFailed, entry.Status)
	assert.Equal(t, "Essential container in task exited", entry.FailureReason)
	assert.Empty(t, env.mails)
}

func TestFinalizeTrainingWhenJobRunning(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	env.batch.jobs["job-1"] = batchtypes.JobDetail{
		JobId:  aws.String("job-1"),
		Status: batchtypes.JobStatusRunning,
	}
	env.svc.cache.invalidate("u1")

	entry, err := env.svc.Get(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WakewordStatusTraining, entry.Status)
}

func TestFinalizeStaysPendingWhileQueued(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	env.batch.jobs["job-1"] = batchtypes.JobDetail{
		JobId:  aws.String("job-1"),
		Status: batchtypes.JobStatusRunnable,
	}
	env.svc.cache.invalidate("u1")

	entry, err := env.svc.Get(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WakewordStatusPending, entry.Status)
}

func TestFinalizeFailedWhenJobRecordExpired(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	// No env.batch.jobs entry: Batch retains records ~7 days — gone with
	// no manifest means the training never delivered.
	env.svc.cache.invalidate("u1")

	entry, err := env.svc.Get(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WakewordStatusFailed, entry.Status)
	assert.NotEmpty(t, entry.FailureReason)
}

func TestFinalizeFailedWhenSucceededWithoutManifest(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	env.batch.jobs["job-1"] = batchtypes.JobDetail{
		JobId:  aws.String("job-1"),
		Status: batchtypes.JobStatusSucceeded,
	}
	env.svc.cache.invalidate("u1")

	entry, err := env.svc.Get(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WakewordStatusFailed, entry.Status)
	assert.Contains(t, entry.FailureReason, "manifest is missing")
}

// ---- catalog ----

func TestCatalogMergesBuiltinsAndCustoms(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	env.putManifests(t, w.ID)
	env.svc.cache.invalidate("u1")

	cat, err := env.svc.Catalog(ctx, "u1")
	require.NoError(t, err)
	assert.False(t, cat.Esp32CustomSupported)
	assert.Len(t, cat.Entries, len(builtinEntries)+1)

	byID := map[string]CatalogEntry{}
	for _, e := range cat.Entries {
		byID[e.ID] = e
	}
	assert.Equal(t, "builtin", byID["hey-jarvis"].Source)
	assert.Equal(t, "builtin", byID["wn9_hiesp"].Source)
	custom := byID[w.ID]
	assert.Equal(t, "custom", custom.Source)
	assert.Equal(t, store.WakewordStatusReady, custom.Status) // lazily finalized in the list path

	// Engine trainability flags are honest.
	trainable := map[string]bool{}
	for _, e := range cat.Engines {
		trainable[e.ID] = e.Trainable
	}
	assert.True(t, trainable["openwakeword"])
	assert.False(t, trainable["porcupine"])
	assert.False(t, trainable["wakenet"])
}

func TestCatalogCacheServesWithoutStoreReads(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	first, err := env.svc.Catalog(ctx, "u1")
	require.NoError(t, err)
	// Mutate the store behind the cache's back; within TTL the cached
	// catalog is returned as-is.
	env.store.items["u1/x"] = &store.Wakeword{ID: "x", UserID: "u1", Phrase: "sneaky insert", Status: store.WakewordStatusReady}
	second, err := env.svc.Catalog(ctx, "u1")
	require.NoError(t, err)
	assert.Equal(t, len(first.Entries), len(second.Entries))

	// Past the 5-min TTL the fresh state is read.
	env.now = env.now.Add(6 * time.Minute)
	third, err := env.svc.Catalog(ctx, "u1")
	require.NoError(t, err)
	assert.Len(t, third.Entries, len(first.Entries)+1)
}

// ---- model manifest ----

func TestModelManifestForReadyWakeword(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	env.putManifests(t, w.ID)
	env.svc.cache.invalidate("u1")

	for _, platform := range []string{"web", "android"} {
		man, err := env.svc.Model(ctx, "u1", w.ID, platform)
		require.NoError(t, err, platform)
		assert.Equal(t, w.ID, man.ID)
		assert.Equal(t, platform, man.Platform)
		assert.Equal(t, "openwakeword", man.Engine)
		assert.Equal(t, formatForPlatform(platform), man.Format)
		assert.Equal(t, testSHA, man.SHA256)
		assert.EqualValues(t, 123456, man.SizeBytes)
		assert.Contains(t, man.URL, modelKey(w.ID, platform))
		// 15-min presign TTL reflected in expiresAt.
		assert.Equal(t, env.now.Add(15*time.Minute).UTC().Format(time.RFC3339), man.ExpiresAt)
	}
}

func TestModelManifestErrorSurface(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Builtin ids never serve bytes from this endpoint.
	_, err := env.svc.Model(ctx, "u1", "hey-jarvis", "web")
	assert.ErrorIs(t, err, ErrBuiltinModel)

	// Unknown id.
	_, err = env.svc.Model(ctx, "u1", "nope-nope-abc123", "web")
	assert.ErrorIs(t, err, ErrNotFound)

	// A bare slug ("hey-live-ninja" from pre-M6 settings) resolves to the
	// user's trained item whose normalized phrase slugs to it.
	lw, err := env.svc.Create(ctx, "u1", "Hey Live Ninja", "")
	require.NoError(t, err)
	env.putManifests(t, lw.ID)
	env.svc.cache.invalidate("u1")
	man, err := env.svc.Model(ctx, "u1", "hey-live-ninja", "web")
	require.NoError(t, err)
	assert.Equal(t, lw.ID, man.ID)
	assert.Equal(t, "hey live ninja", man.Phrase)

	// Invalid platform value.
	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	_, verr := env.svc.Model(ctx, "u1", w.ID, "ios")
	var vErr *ValidationError
	require.ErrorAs(t, verr, &vErr)

	// Not ready yet → NotReadyError carrying the live status.
	env.batch.jobs["job-1"] = batchtypes.JobDetail{JobId: aws.String("job-1"), Status: batchtypes.JobStatusRunning}
	_, nerr := env.svc.Model(ctx, "u1", w.ID, "web")
	var nrErr *NotReadyError
	require.ErrorAs(t, nerr, &nrErr)
	assert.Equal(t, store.WakewordStatusTraining, nrErr.Status)

	// Ready, but esp32 has no custom variant (honest capability flag).
	env.putManifests(t, w.ID)
	env.svc.cache.invalidate("u1")
	_, err = env.svc.Model(ctx, "u1", w.ID, "esp32")
	assert.ErrorIs(t, err, ErrPlatformUnsupported)

	// Cross-user isolation: another user never sees u1's item.
	_, err = env.svc.Model(ctx, "u2", w.ID, "web")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestModelManifestRejectsKeyOutsidePrefix(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	env.putManifests(t, w.ID)

	// Tamper: manifest names a key outside this wake word's own prefix.
	man := storedManifest{
		ID: w.ID, Platform: "web", Engine: "openwakeword",
		Format: "oww-onnx-web-v1", Key: "wakewords/other-id/web/model.onnx",
		SHA256: testSHA, SizeBytes: 1,
	}
	raw, err := json.Marshal(man)
	require.NoError(t, err)
	env.s3.objects[manifestKey(w.ID, "web")] = raw
	env.svc.cache.invalidate("u1")

	_, err = env.svc.Model(ctx, "u1", w.ID, "web")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside prefix")
}

// ---- delete ----

func TestDeleteCancelsJobAndPurgesArtifacts(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	env.s3.objects[modelKey(w.ID, "web")] = []byte("partial")

	require.NoError(t, env.svc.Delete(ctx, "u1", w.ID))
	assert.Equal(t, []string{"job-1"}, env.batch.terminated)
	assert.Empty(t, env.s3.objects)
	got, err := env.store.GetWakeword(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Nil(t, got)

	// Builtins are not deletable; absent ids 404.
	assert.ErrorIs(t, env.svc.Delete(ctx, "u1", "hey-jarvis"), ErrBuiltinModel)
	assert.ErrorIs(t, env.svc.Delete(ctx, "u1", w.ID), ErrNotFound)
}

func TestDeleteGuards(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)

	// Not-owner: items live in the caller's own partition — another user
	// resolves nothing and gets ErrNotFound (→ 404), never u1's item.
	assert.ErrorIs(t, env.svc.Delete(ctx, "u2", w.ID), ErrNotFound)
	stillThere, err := env.store.GetWakeword(ctx, "u1", w.ID)
	require.NoError(t, err)
	require.NotNil(t, stillThere)

	// Actively training: delete is refused (→ 409) and nothing is touched.
	env.store.items["u1/"+w.ID].Status = store.WakewordStatusTraining
	env.s3.objects[modelKey(w.ID, "web")] = []byte("partial")
	assert.ErrorIs(t, env.svc.Delete(ctx, "u1", w.ID), ErrTrainingInProgress)
	assert.Empty(t, env.batch.terminated)
	assert.Contains(t, env.s3.objects, modelKey(w.ID, "web"))
	stillThere, err = env.store.GetWakeword(ctx, "u1", w.ID)
	require.NoError(t, err)
	require.NotNil(t, stillThere)

	// Terminal (failed) items delete fine — the stuck-item escape hatch.
	env.store.items["u1/"+w.ID].Status = store.WakewordStatusFailed
	env.svc.cache.invalidate("u1")
	require.NoError(t, env.svc.Delete(ctx, "u1", w.ID))
	assert.Empty(t, env.s3.objects)
	gone, err := env.store.GetWakeword(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Nil(t, gone)
}

// ---- retry ----

func TestRetryOnlyFromFailed(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)

	// Pending with a live queued job → finalize keeps it pending → refused.
	env.batch.jobs["job-1"] = batchtypes.JobDetail{JobId: aws.String("job-1"), Status: batchtypes.JobStatusRunnable}
	_, err = env.svc.Retry(ctx, "u1", w.ID)
	assert.ErrorIs(t, err, ErrNotRetryable)

	// Training → refused.
	env.batch.jobs["job-1"] = batchtypes.JobDetail{JobId: aws.String("job-1"), Status: batchtypes.JobStatusRunning}
	env.svc.cache.invalidate("u1")
	_, err = env.svc.Retry(ctx, "u1", w.ID)
	assert.ErrorIs(t, err, ErrNotRetryable)

	// Ready → refused.
	env.store.items["u1/"+w.ID].Status = store.WakewordStatusReady
	_, err = env.svc.Retry(ctx, "u1", w.ID)
	assert.ErrorIs(t, err, ErrNotRetryable)

	// Builtin ids and unknown ids are not retryable at all.
	_, err = env.svc.Retry(ctx, "u1", "hey-jarvis")
	assert.ErrorIs(t, err, ErrBuiltinModel)
	_, err = env.svc.Retry(ctx, "u1", "nope-nope-abc123")
	assert.ErrorIs(t, err, ErrNotFound)

	// Not-owner: partition isolation → ErrNotFound.
	env.store.items["u1/"+w.ID].Status = store.WakewordStatusFailed
	_, err = env.svc.Retry(ctx, "u2", w.ID)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRetryFromFailedResubmitsAndConsumesQuota(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	require.Equal(t, 1, env.store.slots["u1/2026-07-17"])

	// The job fails (the exact prod shape: Batch reports FAILED).
	env.batch.jobs["job-1"] = batchtypes.JobDetail{
		JobId:        aws.String("job-1"),
		Status:       batchtypes.JobStatusFailed,
		StatusReason: aws.String("Essential container in task exited"),
	}
	env.svc.cache.invalidate("u1")
	entry, err := env.svc.Get(ctx, "u1", w.ID)
	require.NoError(t, err)
	require.Equal(t, store.WakewordStatusFailed, entry.Status)

	env.batch.nextJobID = "job-2"
	re, err := env.svc.Retry(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Equal(t, w.ID, re.ID) // same deterministic id → same S3 prefix
	assert.Equal(t, store.WakewordStatusPending, re.Status)
	assert.Equal(t, "job-2", re.BatchJobID)
	require.Len(t, env.batch.submitted, 2)

	// A retry consumes a daily slot exactly like a fresh train.
	assert.Equal(t, 2, env.store.slots["u1/2026-07-17"])

	// The stored item is a fresh pending record (stale failure cleared
	// via replace-in-place).
	stored, err := env.store.GetWakeword(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WakewordStatusPending, stored.Status)
	assert.Empty(t, stored.FailureReason)
}

func TestRetryBlockedByDailyLimit(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	w, err := env.svc.Create(ctx, "u1", "hey parrot", "")
	require.NoError(t, err)
	env.store.items["u1/"+w.ID].Status = store.WakewordStatusFailed
	env.store.items["u1/"+w.ID].FailureReason = "training job failed"
	env.store.slots["u1/2026-07-17"] = 3 // cap reached

	_, err = env.svc.Retry(ctx, "u1", w.ID)
	assert.ErrorIs(t, err, ErrDailyLimit)
	require.Len(t, env.batch.submitted, 1) // no second submission

	// Next UTC day the retry goes through.
	env.now = env.now.Add(24 * time.Hour)
	env.batch.nextJobID = "job-2"
	env.svc.cache.invalidate("u1")
	re, err := env.svc.Retry(ctx, "u1", w.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WakewordStatusPending, re.Status)
	assert.Equal(t, 1, env.store.slots["u1/2026-07-18"])
}
