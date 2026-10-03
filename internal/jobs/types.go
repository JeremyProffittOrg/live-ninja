// Package jobs implements durable, user-owned reminders and human review checkpoints.
// Executing a reminder only writes its in-app receipt. No model, email, machine,
// or external-action provider is implicitly enabled by creating a job.
package jobs

import (
	"context"
	"errors"
	"time"
)

var (
	ErrValidation   = errors.New("jobs: invalid request")
	ErrNotFound     = errors.New("jobs: not found")
	ErrConflict     = errors.New("jobs: version or request conflict; refresh and try again")
	ErrInvalidState = errors.New("jobs: action is not available in this state")
	ErrForbidden    = errors.New("jobs: account is not active")
	ErrLimit        = errors.New("jobs: pending run limit reached; resolve or cancel pending runs")
	ErrCorrupt      = errors.New("jobs: stored record requires operator recovery")
)

const MaxRuns = 50
const MaxReceipts = 100

type Schedule struct {
	Kind     string `json:"kind"`
	At       string `json:"at,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	Time     string `json:"time,omitempty"`
	Weekday  int    `json:"weekday,omitempty"`
}
type Input struct {
	Title        string   `json:"title"`
	Instructions string   `json:"instructions"`
	Kind         string   `json:"kind"`
	Schedule     Schedule `json:"schedule"`
}
type Job struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Instructions string   `json:"instructions"`
	Kind         string   `json:"kind"`
	Schedule     Schedule `json:"schedule"`
	Status       string   `json:"status"`
	Version      int64    `json:"version"`
	NextRunAt    string   `json:"nextRunAt,omitempty"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt"`
	LastRun      *Run     `json:"lastRun,omitempty"`
	Error        string   `json:"error,omitempty"`
}
type Run struct {
	ID                string `json:"id"`
	JobID             string `json:"jobId"`
	Status            string `json:"status"`
	Progress          string `json:"progress"`
	Result            string `json:"result,omitempty"`
	Error             string `json:"error,omitempty"`
	Attempt           int    `json:"attempt"`
	Title             string `json:"title"`
	Instructions      string `json:"instructions"`
	Kind              string `json:"kind"`
	CreatedAt         string `json:"createdAt"`
	UpdatedAt         string `json:"updatedAt"`
	FinishedAt        string `json:"finishedAt,omitempty"`
	ApprovalExpiresAt string `json:"approvalExpiresAt,omitempty"`
	ApprovedBy        string `json:"approvedBy,omitempty"`
	ScheduledFor      string `json:"scheduledFor,omitempty"`
	Generation        int64  `json:"generation"`
	RetryOf           string `json:"retryOf,omitempty"`
}
type Page struct {
	Jobs       []Job  `json:"jobs"`
	NextCursor string `json:"nextCursor,omitempty"`
}
type RunPage struct {
	Runs       []Run  `json:"runs"`
	NextCursor string `json:"nextCursor,omitempty"`
}
type TickResult struct {
	Processed int `json:"processed"`
	Conflicts int `json:"conflicts"`
	Skipped   int `json:"skipped"`
	Failed    int `json:"failed"`
}

// Record keeps job changes, runs and idempotency receipts in one CAS boundary.
// The bounded record is wholly in USER#uid, so account export/purge includes it.
type Record struct {
	Job               Job       `json:"job"`
	UserID            string    `json:"userId"`
	Generation        int64     `json:"generation"`
	Runs              []Run     `json:"runs"`
	Receipts          []Receipt `json:"receipts"`
	CreateFingerprint string    `json:"createFingerprint"`
	RetryAfter        string    `json:"retryAfter,omitempty"`
}
type Receipt struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	RunID       string `json:"runId,omitempty"`
}
type Ref struct{ UserID, ID string }
type Store interface {
	Get(context.Context, string, string) (*Record, error)
	List(context.Context, string, int, string) ([]Record, string, error)
	CompareAndSwap(context.Context, *Record, int64) error
	Due(context.Context, time.Time, int) ([]Ref, error)
}
