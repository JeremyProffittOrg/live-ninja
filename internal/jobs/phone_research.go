package jobs

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"time"
)

const PhoneResearchKind = "phone-research"
const PhoneResearchPurpose = "Ask each hotel about guest Wi-Fi login requirements and reported VPN restrictions. Do not request private router settings or credentials."

type PhoneTarget struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	City      string `json:"city"`
	Phone     string `json:"phone"`
	SourceURL string `json:"sourceUrl"`
}
type PhoneResearchSnapshot struct {
	Targets        []PhoneTarget `json:"targets"`
	Purpose        string        `json:"purpose"`
	MaxCallSeconds int           `json:"maxCallSeconds"`
	MaxAttempts    int           `json:"maxAttempts"`
	MaxCostUSD     string        `json:"maxCostUSD"`
}
type PhoneProperty struct {
	TargetID        string `json:"targetId"`
	Status          string `json:"status"`
	CallSID         string `json:"callSid,omitempty"`
	RecordingStatus string `json:"recordingStatus,omitempty"`
	RecordingSource string `json:"recordingSource,omitempty"`
	Result          string `json:"result,omitempty"`
	Error           string `json:"error,omitempty"`
}
type PhoneResearchRun struct {
	PhoneResearchSnapshot
	Command         string          `json:"command"`
	CommandVersion  int64           `json:"commandVersion"`
	ProviderVersion int64           `json:"providerVersion"`
	Properties      []PhoneProperty `json:"properties"`
	Reason          string          `json:"reason,omitempty"`
}
type PhoneCatalog struct {
	Targets       []PhoneTarget `json:"targets"`
	Enabled       bool          `json:"enabled"`
	BlockedReason string        `json:"blockedReason,omitempty"`
}
type PhoneCommand struct {
	OwnerUserID       string   `json:"ownerUserId"`
	JobID             string   `json:"jobId"`
	RunID             string   `json:"runId"`
	Generation        int64    `json:"generation"`
	Command           string   `json:"command"`
	CommandVersion    int64    `json:"commandVersion"`
	ApprovalExpiresAt int64    `json:"approvalExpiresAt"`
	TargetIDs         []string `json:"targetIds"`
}
type PhoneResult struct {
	Status         string          `json:"status"`
	Reason         string          `json:"reason"`
	Version        int64           `json:"version"`
	CommandVersion int64           `json:"commandVersion"`
	Properties     []PhoneProperty `json:"properties"`
}
type PhoneResearchProvider interface {
	Catalog(context.Context, string) (PhoneCatalog, error)
	Advance(context.Context, PhoneCommand) (PhoneResult, error)
	Status(context.Context, string, string, string) (PhoneResult, error)
}

func (s *Service) WithPhoneResearch(provider PhoneResearchProvider) *Service {
	copy := *s
	copy.phone = provider
	return &copy
}
func (s *Service) PhoneResearchAvailable() bool { return s.phone != nil }
func (s *Service) PhoneCatalog(ctx context.Context, uid string) (PhoneCatalog, error) {
	if uid == "" {
		return PhoneCatalog{}, ErrForbidden
	}
	if s.phone == nil {
		return PhoneCatalog{}, ErrUnsupported
	}
	c, e := s.phone.Catalog(ctx, uid)
	if e != nil {
		return c, e
	}
	if len(c.Targets) < 5 || len(c.Targets) > 100 {
		return PhoneCatalog{}, ErrCorrupt
	}
	seen := map[string]bool{}
	for _, t := range c.Targets {
		if !validPhoneTarget(t) || seen[t.ID] {
			return PhoneCatalog{}, ErrCorrupt
		}
		seen[t.ID] = true
	}
	return c, nil
}
func validPhoneTarget(t PhoneTarget) bool {
	u, e := url.Parse(t.SourceURL)
	return regexp.MustCompile(`^hotel-[0-9]{2}$`).MatchString(t.ID) && len(t.Name) > 0 && len(t.Name) <= 200 && len(t.City) > 0 && len(t.City) <= 100 && regexp.MustCompile(`^\+1[0-9]{10}$`).MatchString(t.Phone) && e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}
func validPhoneIDs(ids []string) bool {
	if len(ids) != 5 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] || !regexp.MustCompile(`^hotel-[0-9]{2}$`).MatchString(id) {
			return false
		}
		seen[id] = true
	}
	return true
}
func validPhoneSnapshot(v *PhoneResearchSnapshot) bool {
	if v == nil || v.Purpose != PhoneResearchPurpose || v.MaxCallSeconds != 180 || v.MaxAttempts != 1 || v.MaxCostUSD != "7.00" {
		return false
	}
	ids := []string{}
	for _, t := range v.Targets {
		if !validPhoneTarget(t) {
			return false
		}
		ids = append(ids, t.ID)
	}
	return validPhoneIDs(ids)
}
func (s *Service) phoneSnapshot(ctx context.Context, uid string, ids []string) (*PhoneResearchSnapshot, error) {
	if !validPhoneIDs(ids) {
		return nil, ErrValidation
	}
	c, e := s.PhoneCatalog(ctx, uid)
	if e != nil {
		return nil, e
	}
	byID := map[string]PhoneTarget{}
	for _, t := range c.Targets {
		byID[t.ID] = t
	}
	v := &PhoneResearchSnapshot{Purpose: PhoneResearchPurpose, MaxCallSeconds: 180, MaxAttempts: 1, MaxCostUSD: "7.00"}
	for _, id := range ids {
		t, ok := byID[id]
		if !ok {
			return nil, ErrValidation
		}
		v.Targets = append(v.Targets, t)
	}
	return v, nil
}
func phoneTerminal(status string) bool {
	return status == "succeeded" || status == "failed" || status == "cancelled"
}
func pendingRun(v Run) bool {
	if v.Kind == PhoneResearchKind {
		return !phoneTerminal(v.Status)
	}
	return v.Status == "queued" || v.Status == "waiting_approval"
}
func activePhoneRun(r *Record) *Run {
	for i := len(r.Runs) - 1; i >= 0; i-- {
		if r.Runs[i].Kind == PhoneResearchKind && !phoneTerminal(r.Runs[i].Status) {
			return &r.Runs[i]
		}
	}
	return nil
}

func (s *Service) runPhoneResearch(ctx context.Context, uid, id string, version int64, requestID string) (*Run, error) {
	if s.phone == nil {
		return nil, ErrUnsupported
	}
	r, rid, e := s.mutate(ctx, uid, id, requestID, digest([]any{"run", version}), version, func(r *Record, now time.Time) (string, error) {
		if r.Job.Kind != PhoneResearchKind || r.Job.Status != "active" || activePhoneRun(r) != nil || !validPhoneSnapshot(r.Job.PhoneResearch) {
			return "", ErrInvalidState
		}
		if e := reserveRun(r); e != nil {
			return "", e
		}
		run := newRun(r, ident("run_", []string{id, requestID}), now)
		snapshot := *r.Job.PhoneResearch
		snapshot.Targets = append([]PhoneTarget(nil), snapshot.Targets...)
		run.PhoneResearch = &PhoneResearchRun{PhoneResearchSnapshot: snapshot, Command: "run", CommandVersion: 1, Properties: []PhoneProperty{}}
		for _, t := range snapshot.Targets {
			run.PhoneResearch.Properties = append(run.PhoneResearch.Properties, PhoneProperty{TargetID: t.ID, Status: "not_started"})
		}
		run.ApprovedBy = uid
		run.ApprovalExpiresAt = stamp(now.Add(24 * time.Hour))
		run.Progress = "Approved five-hotel task; checking phone admission."
		r.Runs = append(r.Runs, run)
		return run.ID, nil
	})
	if e != nil {
		return nil, e
	}
	run, e := resultRun(r, rid, nil)
	if e != nil {
		return nil, e
	}
	if run.Status != "queued" {
		return run, nil
	}
	return s.dispatchPhone(ctx, r, run)
}

func (s *Service) phoneAction(ctx context.Context, uid, id, action string, version int64, requestID string) (*Job, error) {
	if s.phone == nil {
		return nil, ErrUnsupported
	}
	r, rid, e := s.mutate(ctx, uid, id, requestID, digest([]any{action, version}), version, func(r *Record, now time.Time) (string, error) {
		if r.Job.Kind != PhoneResearchKind || r.Job.Status == "cancelled" {
			return "", ErrInvalidState
		}
		run := activePhoneRun(r)
		switch action {
		case "pause":
			if r.Job.Status != "active" {
				return "", ErrInvalidState
			}
			r.Job.Status = "paused"
		case "resume":
			if r.Job.Status != "paused" {
				return "", ErrInvalidState
			}
			if run != nil && !parse(run.ApprovalExpiresAt).After(now) {
				return "", fmt.Errorf("%w: the original phone approval expired; cancel and approve a new run", ErrInvalidState)
			}
			r.Job.Status = "active"
		case "cancel":
			r.Job.Status = "cancelled"
		default:
			return "", ErrValidation
		}
		if run == nil {
			return "", nil
		}
		if run.PhoneResearch == nil || !validPhoneSnapshot(&run.PhoneResearch.PhoneResearchSnapshot) {
			return "", ErrCorrupt
		}
		command := action
		if action == "resume" {
			command = "run"
		}
		run.PhoneResearch.Command = command
		run.PhoneResearch.CommandVersion++
		run.Status = "stopping"
		if command == "run" {
			run.Status = "queued"
		}
		run.Progress = "Checking the requested phone control with the provider."
		run.UpdatedAt = stamp(now)
		return run.ID, nil
	})
	if e != nil {
		return nil, e
	}
	if rid != "" {
		run, e := resultRun(r, rid, nil)
		if e != nil {
			return nil, e
		}
		if run.Status == "queued" || run.Status == "stopping" {
			if _, e = s.dispatchPhone(ctx, r, run); e != nil {
				return nil, e
			}
		}
	}
	return s.Get(ctx, uid, id)
}

func (s *Service) dispatchPhone(ctx context.Context, r *Record, run *Run) (*Run, error) {
	if run.PhoneResearch == nil {
		return nil, ErrCorrupt
	}
	command := PhoneCommand{OwnerUserID: r.UserID, JobID: r.Job.ID, RunID: run.ID, Generation: run.Generation, Command: run.PhoneResearch.Command, CommandVersion: run.PhoneResearch.CommandVersion, ApprovalExpiresAt: parse(run.ApprovalExpiresAt).Unix()}
	for _, t := range run.PhoneResearch.Targets {
		command.TargetIDs = append(command.TargetIDs, t.ID)
	}
	result, e := s.phone.Advance(ctx, command)
	if e != nil {
		return nil, e
	}
	return s.applyPhoneResult(ctx, r.UserID, r.Job.ID, run.ID, result)
}

// SyncPhoneRun is called only by the IAM notification worker or an explicit
// owner refresh. The event supplies lookup IDs, never permission or results.
func (s *Service) SyncPhoneRun(ctx context.Context, uid, id, runID string) (*Run, error) {
	if s.phone == nil {
		return nil, ErrUnsupported
	}
	r, e := s.store.Get(ctx, uid, id)
	if e != nil {
		return nil, e
	}
	v, e := runByID(r, runID)
	if e != nil {
		return nil, e
	}
	if r.UserID != uid || r.Job.Kind != PhoneResearchKind || v.Kind != PhoneResearchKind || v.PhoneResearch == nil || v.ApprovedBy != uid {
		return nil, ErrForbidden
	}
	result, e := s.phone.Status(ctx, uid, id, runID)
	if e != nil {
		return nil, e
	}
	return s.applyPhoneResult(ctx, uid, id, runID, result)
}
func (s *Service) applyPhoneResult(ctx context.Context, uid, id, runID string, result PhoneResult) (*Run, error) {
	for tries := 0; tries < 3; tries++ {
		r, e := s.store.Get(ctx, uid, id)
		if e != nil {
			return nil, e
		}
		v, e := runByID(r, runID)
		if e != nil {
			return nil, e
		}
		if v.PhoneResearch == nil || v.ApprovedBy != uid || v.Kind != PhoneResearchKind {
			return nil, ErrForbidden
		}
		if result.CommandVersion != v.PhoneResearch.CommandVersion || result.Version <= v.PhoneResearch.ProviderVersion || (phoneTerminal(v.Status) && result.Status != v.Status) {
			return resultRun(r, runID, nil)
		}
		if !validPhoneResult(result, v.PhoneResearch.Targets) {
			return nil, ErrCorrupt
		}
		version := r.Job.Version
		now := s.now().UTC()
		v.PhoneResearch.ProviderVersion = result.Version
		v.PhoneResearch.Properties = append([]PhoneProperty(nil), result.Properties...)
		v.PhoneResearch.Reason = result.Reason
		v.Status = result.Status
		v.Progress = result.Reason
		v.UpdatedAt = stamp(now)
		if result.Status == "blocked" || result.Status == "paused" {
			if r.Job.Status != "cancelled" {
				r.Job.Status = "paused"
			}
		}
		if phoneTerminal(result.Status) {
			if v.FinishedAt == "" {
				v.FinishedAt = stamp(now)
			}
			v.Result = "Five-property phone task finished. Results report contact outcomes and staff statements; VPN compatibility is not verified."
			if result.Status == "failed" {
				v.Error = result.Reason
			}
		}
		r.Job.Version++
		r.Job.UpdatedAt = stamp(now)
		r.Job.LastRun = nil
		if e = s.store.CompareAndSwap(ctx, r, version); e == ErrConflict {
			continue
		} else if e != nil {
			return nil, e
		}
		return resultRun(r, runID, nil)
	}
	return nil, ErrConflict
}
func validPhoneResult(result PhoneResult, targets []PhoneTarget) bool {
	if result.Version < 1 || result.CommandVersion < 1 || len(result.Reason) > 2000 || len(result.Properties) != 5 {
		return false
	}
	switch result.Status {
	case "blocked", "running", "paused", "cancelled", "succeeded", "failed":
	default:
		return false
	}
	allowed := map[string]bool{}
	for _, t := range targets {
		allowed[t.ID] = true
	}
	for _, p := range result.Properties {
		if !allowed[p.TargetID] || len(p.Result) > 12000 || len(p.Error) > 2000 {
			return false
		}
		delete(allowed, p.TargetID)
		switch p.Status {
		case "not_started", "creating", "unknown", "queued", "initiated", "ringing", "in-progress", "completed", "busy", "no-answer", "failed", "canceled", "rejected", "skipped":
		default:
			return false
		}
		if p.CallSID != "" && !regexp.MustCompile(`^CA[0-9a-fA-F]{32}$`).MatchString(p.CallSID) {
			return false
		}
		if result.Status == "succeeded" {
			switch p.Status {
			case "completed", "busy", "no-answer", "failed", "canceled", "rejected":
			default:
				return false
			}
			if p.CallSID == "" && p.Status != "rejected" {
				return false
			}
		}
		if p.RecordingSource != "" && p.RecordingSource != "hotel-trial-2026-10-15" {
			return false
		}
	}
	return len(allowed) == 0
}

func validPhoneRecord(r *Record) bool {
	if r.Job.Kind != PhoneResearchKind {
		if r.Job.PhoneResearch != nil {
			return false
		}
		for _, run := range r.Runs {
			if run.Kind == PhoneResearchKind || run.PhoneResearch != nil {
				return false
			}
		}
		return true
	}
	if r.Generation < 1 || !validPhoneSnapshot(r.Job.PhoneResearch) || r.Job.Schedule.Kind != "once" || r.Job.Schedule.At != "" || r.Job.Instructions != PhoneResearchPurpose {
		return false
	}
	for _, run := range r.Runs {
		p := run.PhoneResearch
		if run.Kind != PhoneResearchKind || run.JobID != r.Job.ID || run.ApprovedBy != r.UserID || parse(run.ApprovalExpiresAt).IsZero() || run.Generation < 1 || run.Generation > r.Generation || p == nil || !validPhoneSnapshot(&p.PhoneResearchSnapshot) || p.CommandVersion < 1 || p.ProviderVersion < 0 {
			return false
		}
		if !phoneTerminal(run.Status) && run.Generation != r.Generation {
			return false
		}
		if p.Command != "run" && p.Command != "pause" && p.Command != "cancel" {
			return false
		}
		status := run.Status
		if status == "queued" || status == "stopping" {
			status = "running"
		}
		if !validPhoneResult(PhoneResult{Status: status, Reason: p.Reason, Version: 1, CommandVersion: p.CommandVersion, Properties: p.Properties}, p.Targets) {
			return false
		}
	}
	return true
}
