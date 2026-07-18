package wakeword

import (
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
	"github.com/aws/aws-sdk-go-v2/service/batch"
	batchtypes "github.com/aws/aws-sdk-go-v2/service/batch/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// ---- typed errors (mapped to HTTP statuses in wakeword_routes.go) ----

var (
	// ErrNotFound: no such wake word for this user (→ 404).
	ErrNotFound = errors.New("wakeword: not found")
	// ErrBuiltinModel: builtin entries ship inside the client/firmware —
	// there is nothing to download or delete server-side (→ 404 on the
	// model endpoint, 400 on delete).
	ErrBuiltinModel = errors.New("wakeword: builtin models ship with the client")
	// ErrEngineUnavailable: only openwakeword trains server-side (→ 400).
	ErrEngineUnavailable = errors.New("wakeword: engine not trainable server-side")
	// ErrCollision: normalized phrase collides with a builtin or an
	// existing (non-failed) custom entry (→ 409).
	ErrCollision = errors.New("wakeword: phrase already exists in the catalog")
	// ErrDailyLimit: the ≤3/day/user cap is reached (→ 429).
	ErrDailyLimit = errors.New("wakeword: daily training limit reached")
	// ErrQueueFull: too many training jobs already queued/running (→ 429).
	ErrQueueFull = errors.New("wakeword: training queue is full")
	// ErrPlatformUnsupported: custom models have no esp32 variant yet
	// (honest capability flag, → 404 on the model endpoint).
	ErrPlatformUnsupported = errors.New("wakeword: no model for that platform")
)

// ValidationError carries the human-readable phrase-validation message
// (→ 400 with the message verbatim).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return "wakeword: " + e.Msg }

// NotReadyError is returned by Model while training is in flight. The
// route maps it to the contract's chosen not-ready form: 409 with a
// {"status": "..."} body (contracts/wakeword-manifest.md offers 404 or
// 409 — this codebase picks 409 consistently, an additive contract
// finalization for M6).
type NotReadyError struct{ Status string }

func (e *NotReadyError) Error() string { return "wakeword: model not ready (" + e.Status + ")" }

// ---- dependency interfaces (test seams) ----

// Store is the subset of *store.Store this service needs.
type Store interface {
	CreateWakeword(ctx context.Context, w *store.Wakeword) error
	ReplaceWakeword(ctx context.Context, w *store.Wakeword) error
	GetWakeword(ctx context.Context, userID, id string) (*store.Wakeword, error)
	ListWakewords(ctx context.Context, userID string) ([]store.Wakeword, error)
	UpdateWakewordStatus(ctx context.Context, userID, id, status, failureReason string, platforms []string, readyAt string) error
	SetWakewordJobID(ctx context.Context, userID, id, jobID string) error
	DeleteWakeword(ctx context.Context, userID, id string) error
	TakeWakewordTrainingSlot(ctx context.Context, userID, day string, max int) (bool, error)
	ReturnWakewordTrainingSlot(ctx context.Context, userID, day string) error
	ConditionalPut(ctx context.Context, pk, sk string, attrs map[string]any, ttlUnix int64) error
}

// S3API is the subset of the S3 client used (manifest reads, existence
// checks, delete-on-remove).
type S3API interface {
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// PresignAPI mints the short-lived model download URLs.
type PresignAPI interface {
	PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// BatchAPI is the subset of the AWS Batch client used for submission,
// lazy status finalization, the pre-submit backlog check, and
// cancel-on-delete.
type BatchAPI interface {
	SubmitJob(ctx context.Context, params *batch.SubmitJobInput, optFns ...func(*batch.Options)) (*batch.SubmitJobOutput, error)
	DescribeJobs(ctx context.Context, params *batch.DescribeJobsInput, optFns ...func(*batch.Options)) (*batch.DescribeJobsOutput, error)
	ListJobs(ctx context.Context, params *batch.ListJobsInput, optFns ...func(*batch.Options)) (*batch.ListJobsOutput, error)
	TerminateJob(ctx context.Context, params *batch.TerminateJobInput, optFns ...func(*batch.Options)) (*batch.TerminateJobOutput, error)
}

// ---- configuration ----

// Config carries the environment-derived settings. The infra names must
// match template.yaml's Batch/ECR/S3 resources (env vars on the web
// function: WAKEWORDS_BUCKET, WAKEWORD_JOB_QUEUE, WAKEWORD_JOB_DEFINITION).
type Config struct {
	Bucket        string // live-ninja-wakewords-<acct>
	JobQueue      string // Batch job queue (Fargate ARM64, conc≤2 via compute env)
	JobDefinition string // containers/wakeword-train job definition

	DailyLimit    int           // ≤3/day/user (plan.md M6); default 3
	MaxActiveJobs int           // pre-submit backlog cap (queued+running); default 4
	PresignTTL    time.Duration // model URL validity; default 15 min (contract)
	CacheTTL      time.Duration // catalog cache; default 5 min
}

func (c *Config) fillDefaults() {
	if c.DailyLimit <= 0 {
		c.DailyLimit = 3
	}
	if c.MaxActiveJobs <= 0 {
		// The compute environment itself caps concurrency at 2 (locked
		// decision); this pre-submit gate additionally bounds the queued
		// backlog so a burst can't pile up hours of work.
		c.MaxActiveJobs = 4
	}
	if c.PresignTTL <= 0 {
		c.PresignTTL = 15 * time.Minute
	}
	if c.CacheTTL <= 0 {
		c.CacheTTL = 5 * time.Minute
	}
}

// trainedPlatforms are the platforms the openWakeWord training container
// produces artifacts for (int8 .onnx each — locked decision; esp32 is
// deliberately absent, see package comment).
var trainedPlatforms = []string{"web", "android"}

// formatForPlatform maps a trained platform to its manifest format tag
// (contracts/wakeword-manifest.md format table; oww-onnx-android-v1 is
// this build's additive tag for the onnx-on-android decision).
func formatForPlatform(platform string) string {
	switch platform {
	case "web":
		return "oww-onnx-web-v1"
	case "android":
		return "oww-onnx-android-v1"
	default:
		return ""
	}
}

// S3 key layout (single source of truth, matched by the training
// container in containers/wakeword-train):
//
//	wakewords/<wwId>/<platform>/model.onnx
//	wakewords/<wwId>/<platform>/manifest.json
func modelKey(wwID, platform string) string {
	return "wakewords/" + wwID + "/" + platform + "/model.onnx"
}
func manifestKey(wwID, platform string) string {
	return "wakewords/" + wwID + "/" + platform + "/manifest.json"
}
func wakewordPrefix(wwID string) string { return "wakewords/" + wwID + "/" }

// storedManifest is the manifest.json shape the training container
// uploads next to each model artifact.
type storedManifest struct {
	ID        string `json:"id"`
	Platform  string `json:"platform"`
	Engine    string `json:"engine"`
	Format    string `json:"format"`
	Key       string `json:"key"` // S3 key of the model object
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
	CreatedAt string `json:"createdAt"`
}

// ModelManifest is the GET /v1/wakeword/{id}/model response body —
// field-for-field the contracts/wakeword-manifest.md schema.
type ModelManifest struct {
	ID        string `json:"id"`
	Platform  string `json:"platform"`
	Engine    string `json:"engine"`
	Format    string `json:"format"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
	ExpiresAt string `json:"expiresAt"`
}

// ---- service ----

// EmailFunc enqueues one email (Deps.EnqueueEmail's shape).
type EmailFunc func(ctx context.Context, template, to, subject, text string) error

// UserEmailFunc resolves a userID to the address the "ready" mail goes to.
type UserEmailFunc func(ctx context.Context, userID string) (string, error)

// Params bundles Service construction inputs.
type Params struct {
	Store   Store
	S3      S3API
	Presign PresignAPI
	Batch   BatchAPI
	Log     *slog.Logger
	Config  Config

	UserEmail UserEmailFunc
	SendEmail EmailFunc

	Now func() time.Time // test seam; defaults to time.Now
}

// Service implements the wake-word backend behind
// internal/webapp/wakeword_routes.go.
type Service struct {
	store     Store
	s3        S3API
	presign   PresignAPI
	batch     BatchAPI
	log       *slog.Logger
	cfg       Config
	cache     *catalogCache
	userEmail UserEmailFunc
	sendEmail EmailFunc
	now       func() time.Time
}

// New builds a Service. Store/S3/Presign/Batch/Log are required for full
// function; the constructor does not error on nil optional email hooks
// (the ready email is best-effort by design).
func New(p Params) *Service {
	p.Config.fillDefaults()
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.Log == nil {
		p.Log = slog.Default()
	}
	return &Service{
		store:     p.Store,
		s3:        p.S3,
		presign:   p.Presign,
		batch:     p.Batch,
		log:       p.Log,
		cfg:       p.Config,
		cache:     newCatalogCache(p.Config.CacheTTL),
		userEmail: p.UserEmail,
		sendEmail: p.SendEmail,
		now:       p.Now,
	}
}

// NewFromAWS builds a Service with real AWS clients from an ambient
// aws.Config (the Lambda execution role).
func NewFromAWS(awsCfg aws.Config, cfg Config, st Store, log *slog.Logger, userEmail UserEmailFunc, sendEmail EmailFunc) *Service {
	s3c := s3.NewFromConfig(awsCfg)
	return New(Params{
		Store:     st,
		S3:        s3c,
		Presign:   s3.NewPresignClient(s3c),
		Batch:     batch.NewFromConfig(awsCfg),
		Log:       log,
		Config:    cfg,
		UserEmail: userEmail,
		SendEmail: sendEmail,
	})
}

// ---- create ----

// Create validates and registers a custom wake phrase, consumes a daily
// training slot, and submits the AWS Batch training job. Returns the
// stored (pending) item.
func (s *Service) Create(ctx context.Context, userID, phrase, engine string) (*store.Wakeword, error) {
	if engine == "" {
		engine = "openwakeword"
	}
	if engine != "openwakeword" {
		// porcupine → needs a Picovoice account (locked deferral);
		// wakenet → curated builtins only. Never fake a training path.
		return nil, ErrEngineUnavailable
	}

	normalized, msg := ValidatePhrase(phrase)
	if msg != "" {
		return nil, &ValidationError{Msg: msg}
	}
	if collidesWithBuiltin(normalized) {
		return nil, ErrCollision
	}

	// Collision vs the user's own entries by normalized phrase. A failed
	// entry with the same phrase is retrainable (replaced in place).
	existing, err := s.store.ListWakewords(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("wakeword: list for collision check: %w", err)
	}
	retrain := false
	for i := range existing {
		if existing[i].NormalizedPhrase == normalized {
			if existing[i].Status != store.WakewordStatusFailed {
				return nil, ErrCollision
			}
			retrain = true
		}
	}

	// Daily quota gate FIRST (pre-spend discipline, same shape as the
	// broker's mint gate): atomic counter item, ≤3/day/user.
	day := s.now().UTC().Format("2006-01-02")
	ok, err := s.store.TakeWakewordTrainingSlot(ctx, userID, day, s.cfg.DailyLimit)
	if err != nil {
		return nil, fmt.Errorf("wakeword: take training slot: %w", err)
	}
	if !ok {
		return nil, ErrDailyLimit
	}
	// Any failure past this point returns the slot (the training it
	// gated never started).
	returnSlot := func() {
		if rerr := s.store.ReturnWakewordTrainingSlot(ctx, userID, day); rerr != nil {
			s.log.Warn("wakeword: return training slot failed", slog.String("error", rerr.Error()))
		}
	}

	// Backlog gate: count this queue's live jobs (all pre-terminal
	// statuses). The compute environment enforces the hard conc≤2; this
	// ListJobs check keeps the queued backlog bounded pre-submit.
	active, err := s.countActiveJobs(ctx)
	if err != nil {
		returnSlot()
		return nil, fmt.Errorf("wakeword: count active jobs: %w", err)
	}
	if active >= s.cfg.MaxActiveJobs {
		returnSlot()
		return nil, ErrQueueFull
	}

	item := &store.Wakeword{
		ID:               WakewordID(userID, normalized),
		UserID:           userID,
		Phrase:           normalized,
		NormalizedPhrase: normalized,
		Engine:           engine,
		Status:           store.WakewordStatusPending,
		CreatedAt:        s.now().UTC().Format(time.RFC3339),
	}

	if retrain {
		err = s.store.ReplaceWakeword(ctx, item)
	} else {
		err = s.store.CreateWakeword(ctx, item)
	}
	if err != nil {
		returnSlot()
		if errors.Is(err, store.ErrAlreadyExists) {
			// Different-phrase slug collision within this user (or a
			// concurrent create of the same phrase) — same 409.
			return nil, ErrCollision
		}
		return nil, fmt.Errorf("wakeword: store item: %w", err)
	}

	jobID, err := s.submitTrainingJob(ctx, item)
	if err != nil {
		// Mark the item failed so the UI shows a truthful state and the
		// phrase becomes retrainable; the slot goes back.
		if uerr := s.store.UpdateWakewordStatus(ctx, userID, item.ID, store.WakewordStatusFailed, "training job submission failed", nil, ""); uerr != nil {
			s.log.Error("wakeword: mark submit-failure failed", slog.String("error", uerr.Error()))
		}
		returnSlot()
		return nil, fmt.Errorf("wakeword: submit training job: %w", err)
	}
	item.BatchJobID = jobID
	if err := s.store.SetWakewordJobID(ctx, userID, item.ID, jobID); err != nil {
		// Non-fatal: the manifest-existence path still finalizes ready;
		// only failure detection degrades to "stuck pending".
		s.log.Error("wakeword: persist job id failed", slog.String("error", err.Error()))
	}

	s.cache.invalidate(userID)
	return item, nil
}

// activeJobStatuses are every pre-terminal Batch status. ListJobs takes
// exactly one status per call (and defaults to RUNNING alone), so the
// backlog count is the sum over one call per status — 5 tiny calls on a
// rare, already-quota-gated path. (DescribeJobs can't enumerate a queue,
// hence ListJobs here; DescribeJobs is used for per-job finalization.)
var activeJobStatuses = []batchtypes.JobStatus{
	batchtypes.JobStatusSubmitted,
	batchtypes.JobStatusPending,
	batchtypes.JobStatusRunnable,
	batchtypes.JobStatusStarting,
	batchtypes.JobStatusRunning,
}

func (s *Service) countActiveJobs(ctx context.Context) (int, error) {
	total := 0
	for _, st := range activeJobStatuses {
		out, err := s.batch.ListJobs(ctx, &batch.ListJobsInput{
			JobQueue:   aws.String(s.cfg.JobQueue),
			JobStatus:  st,
			MaxResults: aws.Int32(int32(s.cfg.MaxActiveJobs + 1)),
		})
		if err != nil {
			return 0, err
		}
		total += len(out.JobSummaryList)
		if total >= s.cfg.MaxActiveJobs {
			return total, nil
		}
	}
	return total, nil
}

// jobTimeoutSeconds is the per-job hard timeout (20 min, plan.md M6
// locked decision), enforced at submit in addition to the job
// definition so a template drift can't silently unbound it.
const jobTimeoutSeconds = int32(20 * 60)

func (s *Service) submitTrainingJob(ctx context.Context, w *store.Wakeword) (string, error) {
	// Environment contract with containers/wakeword-train: the trainer
	// reads WW_ID/WW_PHRASE, trains openWakeWord (piper-sample-generator
	// synthetic positives, small CPU preset), exports int8 .onnx per
	// platform, and uploads model.onnx + manifest.json under
	// OUTPUT_PREFIX/<platform>/ in OUTPUT_BUCKET.
	out, err := s.batch.SubmitJob(ctx, &batch.SubmitJobInput{
		JobName:       aws.String("ww-train-" + w.ID),
		JobQueue:      aws.String(s.cfg.JobQueue),
		JobDefinition: aws.String(s.cfg.JobDefinition),
		Timeout:       &batchtypes.JobTimeout{AttemptDurationSeconds: aws.Int32(jobTimeoutSeconds)},
		ContainerOverrides: &batchtypes.ContainerOverrides{
			Environment: []batchtypes.KeyValuePair{
				{Name: aws.String("WW_ID"), Value: aws.String(w.ID)},
				{Name: aws.String("WW_USER_ID"), Value: aws.String(w.UserID)},
				{Name: aws.String("WW_PHRASE"), Value: aws.String(w.Phrase)},
				{Name: aws.String("OUTPUT_BUCKET"), Value: aws.String(s.cfg.Bucket)},
				{Name: aws.String("OUTPUT_PREFIX"), Value: aws.String("wakewords/" + w.ID)},
			},
		},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.JobId), nil
}

// ---- read paths ----

// Get returns one catalog entry (builtin or the caller's custom item),
// lazily finalizing in-flight training state. ErrNotFound when absent.
func (s *Service) Get(ctx context.Context, userID, id string) (*CatalogEntry, error) {
	if b := BuiltinEntry(id); b != nil {
		return b, nil
	}
	w, err := s.store.GetWakeword(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if w == nil {
		return nil, ErrNotFound
	}
	w = s.finalize(ctx, w)
	e := entryFromItem(w)
	return &e, nil
}

// Catalog returns builtins + the caller's customs, from the 5-min cache
// when warm.
func (s *Service) Catalog(ctx context.Context, userID string) (*Catalog, error) {
	if cached := s.cache.get(userID, s.now()); cached != nil {
		return cached, nil
	}

	items, err := s.store.ListWakewords(ctx, userID)
	if err != nil {
		return nil, err
	}
	entries := BuiltinEntries()
	for i := range items {
		w := &items[i]
		if w.Status == store.WakewordStatusPending || w.Status == store.WakewordStatusTraining {
			w = s.finalize(ctx, w)
		}
		entries = append(entries, entryFromItem(w))
	}
	cat := &Catalog{
		Engines:              engines,
		Entries:              entries,
		Esp32CustomSupported: false,
	}
	s.cache.put(userID, cat, s.now())
	return cat, nil
}

// ---- lazy finalization ----

// finalize advances a pending/training item's status without any
// callback infrastructure (locked decision: the GET paths lazily check
// S3 for the trainer's manifests and, failing that, DescribeJobs — no
// poller Lambda, no EventBridge rule). Returns the (possibly updated)
// item; store/AWS hiccups log and return the item unchanged so a read
// never fails on a finalize error.
func (s *Service) finalize(ctx context.Context, w *store.Wakeword) *store.Wakeword {
	if w.Status != store.WakewordStatusPending && w.Status != store.WakewordStatusTraining {
		return w
	}

	// Ready check: every trained platform's manifest.json exists.
	allManifests := true
	for _, p := range trainedPlatforms {
		exists, err := s.objectExists(ctx, manifestKey(w.ID, p))
		if err != nil {
			s.log.Warn("wakeword: manifest existence check failed", slog.String("wwId", w.ID), slog.String("error", err.Error()))
			return w
		}
		if !exists {
			allManifests = false
			break
		}
	}
	if allManifests {
		readyAt := s.now().UTC().Format(time.RFC3339)
		if err := s.store.UpdateWakewordStatus(ctx, w.UserID, w.ID, store.WakewordStatusReady, "", trainedPlatforms, readyAt); err != nil {
			s.log.Error("wakeword: mark ready failed", slog.String("wwId", w.ID), slog.String("error", err.Error()))
			return w
		}
		w.Status = store.WakewordStatusReady
		w.Platforms = trainedPlatforms
		w.ReadyAt = readyAt
		s.cache.invalidate(w.UserID)
		s.sendReadyEmailOnce(ctx, w)
		return w
	}

	if w.BatchJobID == "" {
		return w // nothing to interrogate; stays pending until manifests land
	}

	out, err := s.batch.DescribeJobs(ctx, &batch.DescribeJobsInput{Jobs: []string{w.BatchJobID}})
	if err != nil {
		s.log.Warn("wakeword: describe job failed", slog.String("wwId", w.ID), slog.String("error", err.Error()))
		return w
	}
	if len(out.Jobs) == 0 {
		// Batch retains job records ~7 days; gone + no manifest = failed.
		s.transition(ctx, w, store.WakewordStatusFailed, "training job record expired without producing a model")
		return w
	}
	job := out.Jobs[0]
	switch job.Status {
	case batchtypes.JobStatusFailed:
		reason := aws.ToString(job.StatusReason)
		if reason == "" {
			reason = "training job failed"
		}
		s.transition(ctx, w, store.WakewordStatusFailed, reason)
	case batchtypes.JobStatusSucceeded:
		// Succeeded but a manifest is missing: the trainer's upload
		// contract was not met (S3 is strongly consistent — nothing to
		// wait for). Truthfully failed.
		s.transition(ctx, w, store.WakewordStatusFailed, "training job succeeded but model manifest is missing")
	case batchtypes.JobStatusStarting, batchtypes.JobStatusRunning:
		if w.Status != store.WakewordStatusTraining {
			s.transition(ctx, w, store.WakewordStatusTraining, "")
		}
	default:
		// SUBMITTED/PENDING/RUNNABLE → stays pending.
	}
	return w
}

func (s *Service) transition(ctx context.Context, w *store.Wakeword, status, reason string) {
	if err := s.store.UpdateWakewordStatus(ctx, w.UserID, w.ID, status, reason, nil, ""); err != nil {
		s.log.Error("wakeword: status transition failed",
			slog.String("wwId", w.ID), slog.String("to", status), slog.String("error", err.Error()))
		return
	}
	w.Status = status
	w.FailureReason = reason
	s.cache.invalidate(w.UserID)
}

// sendReadyEmailOnce enqueues the FR-K03 "your wake word is ready" SES
// mail exactly once per (user, wwId) via an IDEMP# conditional-put
// marker (same primitive email-dispatch itself uses). Best-effort: a
// mail failure never fails the read path.
func (s *Service) sendReadyEmailOnce(ctx context.Context, w *store.Wakeword) {
	if s.sendEmail == nil || s.userEmail == nil {
		return
	}
	err := s.store.ConditionalPut(ctx,
		"IDEMP#WWREADY#"+w.UserID+"#"+w.ID, "IDEMP",
		map[string]any{"wwId": w.ID, "sentAt": s.now().UTC().Format(time.RFC3339)},
		s.now().Add(30*24*time.Hour).Unix())
	if errors.Is(err, store.ErrAlreadyExists) {
		return // already sent
	}
	if err != nil {
		s.log.Warn("wakeword: ready-email idempotency marker failed", slog.String("error", err.Error()))
		return
	}
	to, err := s.userEmail(ctx, w.UserID)
	if err != nil || to == "" {
		s.log.Warn("wakeword: resolve user email failed", slog.String("wwId", w.ID))
		return
	}
	subject := fmt.Sprintf("Your wake word %q is ready", w.Phrase)
	text := fmt.Sprintf(
		"Training finished for your custom wake word %q.\n\n"+
			"It is now available on web and Android: open Settings → Wake word "+
			"on either surface and select it — the model downloads and hot-swaps "+
			"automatically after its SHA-256 check.\n\n(id: %s)",
		w.Phrase, w.ID)
	if err := s.sendEmail(ctx, "wakeword-ready", to, subject, text); err != nil {
		s.log.Warn("wakeword: enqueue ready email failed", slog.String("error", err.Error()))
	}
}

// ---- model manifest ----

// Model implements GET /v1/wakeword/{id}/model?platform= per
// contracts/wakeword-manifest.md: manifest fields from the trainer's
// manifest.json plus a presigned 15-min model URL.
func (s *Service) Model(ctx context.Context, userID, id, platform string) (*ModelManifest, error) {
	switch platform {
	case "web", "android", "esp32":
	default:
		return nil, &ValidationError{Msg: "platform must be one of web, android, esp32"}
	}

	if BuiltinEntry(id) != nil {
		// Builtin model bytes ship inside the client (web/android bundle)
		// or the firmware model partition (esp32) — never served here.
		return nil, ErrBuiltinModel
	}

	w, err := s.store.GetWakeword(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if w == nil {
		return nil, ErrNotFound
	}
	w = s.finalize(ctx, w)
	switch w.Status {
	case store.WakewordStatusReady:
	case store.WakewordStatusFailed:
		return nil, &NotReadyError{Status: w.Status}
	default:
		return nil, &NotReadyError{Status: w.Status}
	}
	if platform == "esp32" {
		// Honest capability: no oWW-ESP conversion path yet (locked
		// decision) — the client falls back per FR-K04.
		return nil, ErrPlatformUnsupported
	}

	man, err := s.readManifest(ctx, w.ID, platform)
	if err != nil {
		return nil, err
	}

	// Defense in depth: only ever presign keys inside this wake word's
	// own prefix, no matter what the manifest file claims.
	key := man.Key
	if key == "" {
		key = modelKey(w.ID, platform)
	}
	if !strings.HasPrefix(key, wakewordPrefix(w.ID)) {
		return nil, fmt.Errorf("wakeword: manifest names key %q outside prefix %q", key, wakewordPrefix(w.ID))
	}

	expires := s.now().Add(s.cfg.PresignTTL)
	presigned, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	}, func(o *s3.PresignOptions) { o.Expires = s.cfg.PresignTTL })
	if err != nil {
		return nil, fmt.Errorf("wakeword: presign model url: %w", err)
	}

	format := man.Format
	if format == "" {
		format = formatForPlatform(platform)
	}
	return &ModelManifest{
		ID:        w.ID,
		Platform:  platform,
		Engine:    w.Engine,
		Format:    format,
		URL:       presigned.URL,
		SHA256:    man.SHA256,
		SizeBytes: man.SizeBytes,
		ExpiresAt: expires.UTC().Format(time.RFC3339),
	}, nil
}

func (s *Service) readManifest(ctx context.Context, wwID, platform string) (*storedManifest, error) {
	out, err := s.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(manifestKey(wwID, platform)),
	})
	if err != nil {
		if isS3NotFound(err) {
			// Ready item but this platform's manifest vanished — treat as
			// unsupported platform rather than 500.
			return nil, ErrPlatformUnsupported
		}
		return nil, fmt.Errorf("wakeword: read manifest: %w", err)
	}
	defer out.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(out.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("wakeword: read manifest body: %w", err)
	}
	var man storedManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return nil, fmt.Errorf("wakeword: parse manifest: %w", err)
	}
	if len(man.SHA256) != 64 {
		return nil, fmt.Errorf("wakeword: manifest sha256 malformed")
	}
	return &man, nil
}

// ---- delete ----

// Delete removes a custom wake word: cancels an in-flight training job
// (best-effort), deletes the S3 artifacts under its prefix, and removes
// the item.
func (s *Service) Delete(ctx context.Context, userID, id string) error {
	if BuiltinEntry(id) != nil {
		return ErrBuiltinModel
	}
	w, err := s.store.GetWakeword(ctx, userID, id)
	if err != nil {
		return err
	}
	if w == nil {
		return ErrNotFound
	}

	if w.BatchJobID != "" && (w.Status == store.WakewordStatusPending || w.Status == store.WakewordStatusTraining) {
		if _, terr := s.batch.TerminateJob(ctx, &batch.TerminateJobInput{
			JobId:  aws.String(w.BatchJobID),
			Reason: aws.String("wake word deleted by user"),
		}); terr != nil {
			s.log.Warn("wakeword: terminate job failed", slog.String("jobId", w.BatchJobID), slog.String("error", terr.Error()))
		}
	}

	if err := s.deletePrefix(ctx, wakewordPrefix(id)); err != nil {
		return fmt.Errorf("wakeword: delete artifacts: %w", err)
	}
	if err := s.store.DeleteWakeword(ctx, userID, id); err != nil {
		return err
	}
	s.cache.invalidate(userID)
	return nil
}

func (s *Service) deletePrefix(ctx context.Context, prefix string) error {
	var token *string
	for {
		list, err := s.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.cfg.Bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return err
		}
		if len(list.Contents) > 0 {
			objs := make([]s3types.ObjectIdentifier, 0, len(list.Contents))
			for _, o := range list.Contents {
				objs = append(objs, s3types.ObjectIdentifier{Key: o.Key})
			}
			if _, err := s.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(s.cfg.Bucket),
				Delete: &s3types.Delete{Objects: objs, Quiet: aws.Bool(true)},
			}); err != nil {
				return err
			}
		}
		if list.IsTruncated == nil || !*list.IsTruncated {
			return nil
		}
		token = list.NextContinuationToken
	}
}

// ---- S3 helpers ----

func (s *Service) objectExists(ctx context.Context, key string) (bool, error) {
	_, err := s.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// isS3NotFound recognizes the S3 absence shapes: typed NotFound /
// NoSuchKey plus the generic APIError codes (HeadObject surfaces a bare
// "NotFound" code with no typed model).
func isS3NotFound(err error) bool {
	var nf *s3types.NotFound
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nf) || errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	return false
}
