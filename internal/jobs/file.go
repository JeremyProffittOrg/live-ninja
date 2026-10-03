package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// FileStore is the single-process durable local preview adapter. An OS-held
// exclusive lock rejects a second process and is released automatically on a
// crash. Changes are synced to a temporary file then atomically renamed.
// It is NOT a production auth provider: the preview host supplies a fixed user.
type FileStore struct {
	mu      sync.Mutex
	path    string
	records map[string]Record
	unlock  func() error
	closed  bool
}

func NewFileStore(path string) (*FileStore, error) {
	abs, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(filepath.Dir(abs), 0700); e != nil {
		return nil, e
	}
	unlock, e := lockFile(abs + ".lock")
	if e != nil {
		return nil, fmt.Errorf("jobs: local store is already open or cannot be locked: %w", e)
	}
	s := &FileStore{path: abs, records: map[string]Record{}, unlock: unlock}
	b, e := os.ReadFile(abs)
	if errors.Is(e, os.ErrNotExist) {
		return s, nil
	}
	if e != nil {
		unlock()
		return nil, e
	}
	if e = json.Unmarshal(b, &s.records); e != nil {
		unlock()
		return nil, fmt.Errorf("jobs: cannot read durable store (original retained): %w", e)
	}
	if s.records == nil {
		s.records = map[string]Record{}
	}
	return s, nil
}
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.unlock()
}
func recordKey(uid, id string) string { return uid + "\x00" + id }
func cloneRecord(r Record) Record {
	b, _ := json.Marshal(r)
	var out Record
	_ = json.Unmarshal(b, &out)
	return out
}
func (s *FileStore) Get(ctx context.Context, uid, id string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(ctx); e != nil {
		return nil, e
	}
	r, ok := s.records[recordKey(uid, id)]
	if !ok {
		return nil, ErrNotFound
	}
	v := cloneRecord(r)
	return &v, nil
}
func (s *FileStore) check(ctx context.Context) error {
	if s.closed {
		return errors.New("jobs: store is closed")
	}
	return ctx.Err()
}
func (s *FileStore) List(ctx context.Context, uid string, limit int, cursor string) ([]Record, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(ctx); e != nil {
		return nil, "", e
	}
	if cursor != "" && !validID(cursor) {
		return nil, "", ErrValidation
	}
	var ids []string
	for _, r := range s.records {
		if r.UserID == uid && r.Job.ID > cursor {
			ids = append(ids, r.Job.ID)
		}
	}
	sort.Strings(ids)
	limit = pageLimit(limit)
	more := len(ids) > limit
	if more {
		ids = ids[:limit]
	}
	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneRecord(s.records[recordKey(uid, id)]))
	}
	c := ""
	if more {
		c = ids[len(ids)-1]
	}
	return out, c, nil
}
func (s *FileStore) CompareAndSwap(ctx context.Context, r *Record, expected int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(ctx); e != nil {
		return e
	}
	k := recordKey(r.UserID, r.Job.ID)
	old, exists := s.records[k]
	if expected == 0 && exists || expected > 0 && (!exists || old.Job.Version != expected) {
		return ErrConflict
	}
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if len(b) > 300000 {
		return ErrLimit
	}
	s.records[k] = cloneRecord(*r)
	if e = s.persist(); e != nil {
		if exists {
			s.records[k] = old
		} else {
			delete(s.records, k)
		}
		return e
	}
	return nil
}
func (s *FileStore) persist() error {
	b, e := json.MarshalIndent(s.records, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(s.path), ".jobs-write-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(name, s.path); e != nil {
		return e
	}
	syncDirectory(filepath.Dir(s.path))
	return nil
}
func (s *FileStore) Due(ctx context.Context, now time.Time, limit int) ([]Ref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(ctx); e != nil {
		return nil, e
	}
	var records []Record
	for _, r := range s.records {
		at := dueAt(&r)
		if !at.IsZero() && !at.After(now) {
			records = append(records, r)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		a, b := dueAt(&records[i]), dueAt(&records[j])
		if a.Equal(b) {
			return recordKey(records[i].UserID, records[i].Job.ID) < recordKey(records[j].UserID, records[j].Job.ID)
		}
		return a.Before(b)
	})
	limit = pageLimit(limit)
	if len(records) > limit {
		records = records[:limit]
	}
	out := make([]Ref, 0, len(records))
	for _, r := range records {
		out = append(out, Ref{UserID: r.UserID, ID: r.Job.ID})
	}
	return out, nil
}
