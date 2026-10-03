package jobs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const historyBoundary = "Append-only events recorded from this Jobs update onward."

// HistoryEntry is an immutable observation, never an instruction to a worker.
// Run is a full receipt snapshot. Notes remain user messages, not fake replies.
type HistoryEntry struct {
	ID        string `json:"id"`
	JobID     string `json:"jobId"`
	Sequence  int64  `json:"sequence"`
	Version   int64  `json:"version"`
	Role      string `json:"role"`
	Actor     string `json:"actor"`
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	CreatedAt string `json:"createdAt"`
	Status    string `json:"status,omitempty"`
	RunID     string `json:"runId,omitempty"`
	Run       *Run   `json:"run,omitempty"`
}
type HistoryPage struct {
	Entries                []HistoryEntry `json:"entries"`
	OlderCursor            string         `json:"olderCursor,omitempty"`
	NewerCursor            string         `json:"newerCursor,omitempty"`
	HasMore                bool           `json:"hasMore"`
	RetentionBoundary      string         `json:"retentionBoundary"`
	LegacyHistoryAvailable bool           `json:"legacyHistoryAvailable"`
	LegacyRunLimit         int            `json:"legacyRunLimit"`
}
type HistoryQuery struct {
	Limit               int
	Cursor, After, Kind string
}
type HistoryStore interface {
	ListHistory(context.Context, string, string, HistoryQuery) (HistoryPage, error)
}
type NoteInput struct {
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	RunID string `json:"runId,omitempty"`
}

func emptyHistoryPage() HistoryPage {
	return HistoryPage{Entries: []HistoryEntry{}, RetentionBoundary: historyBoundary, LegacyHistoryAvailable: true, LegacyRunLimit: MaxRuns}
}
func historyPrefix(id string) string { return "JOBEVENT#" + id + "#" }
func historyKey(e HistoryEntry) string {
	return fmt.Sprintf("%s%020d", historyPrefix(e.JobID), e.Sequence)
}

type historyCursor struct {
	UserID string `json:"u"`
	JobID  string `json:"j"`
	Key    string `json:"k"`
	Kind   string `json:"f,omitempty"`
}

func encodeHistoryCursor(uid string, e HistoryEntry, kind string) string {
	b, _ := json.Marshal(historyCursor{uid, e.JobID, historyKey(e), kind})
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeHistoryCursor(uid, id, kind, raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if len(raw) > 2048 {
		return "", ErrValidation
	}
	b, e := base64.RawURLEncoding.DecodeString(raw)
	if e != nil {
		return "", ErrValidation
	}
	var c historyCursor
	if json.Unmarshal(b, &c) != nil || c.UserID != uid || c.JobID != id || c.Kind != kind || !strings.HasPrefix(c.Key, historyPrefix(id)) || len(c.Key) != len(historyPrefix(id))+20 {
		return "", ErrValidation
	}
	for _, ch := range strings.TrimPrefix(c.Key, historyPrefix(id)) {
		if ch < '0' || ch > '9' {
			return "", ErrValidation
		}
	}
	return c.Key, nil
}
func validateHistoryQuery(uid, id string, q HistoryQuery) (string, error) {
	if uid == "" {
		return "", ErrForbidden
	}
	if q.Cursor != "" && q.After != "" {
		return "", ErrValidation
	}
	if q.Kind != "" && q.Kind != "note" {
		return "", ErrValidation
	}
	raw := q.Cursor
	if q.After != "" {
		raw = q.After
	}
	return decodeHistoryCursor(uid, id, q.Kind, raw)
}
func historyPage(uid string, entries []HistoryEntry, q HistoryQuery, more bool) HistoryPage {
	p := emptyHistoryPage()
	p.Entries = entries
	p.HasMore = more
	sort.Slice(p.Entries, func(i, j int) bool { return p.Entries[i].Sequence < p.Entries[j].Sequence })
	if len(p.Entries) > 0 {
		p.NewerCursor = encodeHistoryCursor(uid, p.Entries[len(p.Entries)-1], q.Kind)
		if more && q.After == "" {
			p.OlderCursor = encodeHistoryCursor(uid, p.Entries[0], q.Kind)
		}
	} else if q.After != "" {
		p.NewerCursor = q.After
	}
	return p
}

// buildHistory runs inside each adapter's atomic CAS. Existing records do not
// get fabricated backfill: only a newly observed state transition is archived.
func buildHistory(before *Record, after *Record) ([]HistoryEntry, error) {
	if after.Job.Version < 1 || after.Job.Version > (1<<63-1)/100 {
		return nil, ErrLimit
	}
	entries := []HistoryEntry{}
	appendEntry := func(e HistoryEntry) {
		e.JobID = after.Job.ID
		e.Version = after.Job.Version
		e.Sequence = after.Job.Version*100 + int64(len(entries))
		if e.ID == "" {
			e.ID = ident("event_", []any{e.JobID, e.Sequence, e.Kind})
		}
		if e.CreatedAt == "" {
			e.CreatedAt = after.Job.UpdatedAt
		}
		if e.Role == "" {
			e.Role = "system"
		}
		if e.Actor == "" {
			e.Actor = "system"
		}
		entries = append(entries, e)
	}
	kind := ""
	if before == nil {
		kind = "job_created"
	} else if before.Job.Status != after.Job.Status {
		switch after.Job.Status {
		case "active":
			kind = "job_resumed"
		case "paused":
			kind = "job_paused"
		case "cancelled":
			kind = "job_cancelled"
		}
	} else if before.Job.Title != after.Job.Title || before.Job.Instructions != after.Job.Instructions || before.Job.Kind != after.Job.Kind || before.Job.Schedule != after.Job.Schedule {
		kind = "job_updated"
	}
	if kind != "" {
		appendEntry(HistoryEntry{Kind: kind, Status: after.Job.Status, Text: after.Job.Title})
	}
	oldRuns := map[string]Run{}
	if before != nil {
		for _, r := range before.Runs {
			oldRuns[r.ID] = r
		}
	}
	observedRuns := append(append([]Run(nil), after.Runs...), after.evictedRuns...)
	for _, run := range observedRuns {
		old, exists := oldRuns[run.ID]
		if !exists || old != run {
			snapshot := run
			appendEntry(HistoryEntry{Kind: "run_" + run.Status, Status: run.Status, RunID: run.ID, Run: &snapshot, Text: run.Progress})
		}
	}
	for _, entry := range after.pendingEntries {
		appendEntry(entry)
	}
	if len(entries) > 90 {
		return nil, ErrLimit
	}
	for _, entry := range entries {
		b, e := json.Marshal(entry)
		if e != nil {
			return nil, e
		}
		if len(b) > 300000 {
			return nil, fmt.Errorf("%w: one history entry exceeds storage capacity", ErrLimit)
		}
	}
	return entries, nil
}

func (s *Service) ListHistory(ctx context.Context, uid, id string, q HistoryQuery) (HistoryPage, error) {
	if _, e := validateHistoryQuery(uid, id, q); e != nil {
		return emptyHistoryPage(), e
	}
	if _, e := s.store.Get(ctx, uid, id); e != nil {
		return emptyHistoryPage(), e
	}
	st, ok := s.store.(HistoryStore)
	if !ok {
		return emptyHistoryPage(), ErrUnsupported
	}
	return st.ListHistory(ctx, uid, id, q)
}

// AddNote archives user context only. There is deliberately no "steer" provider
// until a real executor can acknowledge consumption of a command.
func (s *Service) AddNote(ctx context.Context, uid, id string, in NoteInput, version int64, requestID string) (*HistoryEntry, *Job, error) {
	if in.Kind != "note" {
		return nil, nil, ErrUnsupported
	}
	in.Text = strings.TrimSpace(in.Text)
	if len(in.Text) == 0 || len(in.Text) > 2000 {
		return nil, nil, fmt.Errorf("%w: note must be 1 through 2000 UTF-8 bytes", ErrValidation)
	}
	if _, ok := s.store.(HistoryStore); !ok {
		return nil, nil, ErrUnsupported
	}
	entryID := ident("command_", []string{id, requestID})
	r, _, e := s.mutate(ctx, uid, id, requestID, digest([]any{"note", in, version}), version, func(r *Record, now time.Time) (string, error) {
		if in.RunID != "" {
			if _, e := runByID(r, in.RunID); e != nil {
				return "", e
			}
		}
		r.pendingEntries = []HistoryEntry{{ID: entryID, Kind: "note", Text: in.Text, RunID: in.RunID, Role: "user", Actor: uid, Status: "recorded", CreatedAt: stamp(now)}}
		return entryID, nil
	})
	if e != nil {
		return nil, nil, e
	}
	// Deterministic receipt fields make a lost-response retry return the exact
	// committed note without scanning unbounded history or inventing execution.
	entry := HistoryEntry{ID: entryID, JobID: id, Kind: "note", Text: in.Text, RunID: in.RunID, Role: "user", Actor: uid, Status: "recorded"}
	// The note's original version/time are retained in its command receipt.
	for _, rc := range r.Receipts {
		if rc.ID == requestID {
			entry.Version = rc.Version
			entry.Sequence = rc.Sequence
			entry.CreatedAt = rc.CreatedAt
			break
		}
	}
	j := cleanJob(r)
	return &entry, &j, nil
}
