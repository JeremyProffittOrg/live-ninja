package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestJobHistoryRetainsMoreThan100MixedEntriesAndLargeOutput(t *testing.T) {
	s, fs, _ := fixture(t)
	ctx := context.Background()
	j := create(t, s, input("reminder"))
	firstVersion := j.Version
	first, _, e := s.AddNote(ctx, "alice", j.ID, NoteInput{Kind: "note", Text: "first durable note"}, firstVersion, "note-first-request")
	if e != nil {
		t.Fatal(e)
	}
	for n := 0; n < 65; n++ {
		j = current(t, s, j.ID)
		if _, _, e = s.AddNote(ctx, "alice", j.ID, NoteInput{Kind: "note", Text: fmt.Sprintf("note %d", n)}, j.Version, fmt.Sprintf("note-request-%03d", n)); e != nil {
			t.Fatal(e)
		}
		j = current(t, s, j.ID)
		if _, e = s.RunNow(ctx, "alice", j.ID, j.Version, fmt.Sprintf("run-request-%03d", n)); e != nil {
			t.Fatal(e)
		}
	}
	// Full immutable receipt output survives future snapshot compaction.
	r, e := fs.Get(ctx, "alice", j.ID)
	if e != nil {
		t.Fatal(e)
	}
	oldVersion := r.Job.Version
	r.Job.Version++
	r.Runs[len(r.Runs)-1].Result = strings.Repeat("large output line\n", 6000)
	if e = fs.CompareAndSwap(ctx, r, oldVersion); e != nil {
		t.Fatal(e)
	}
	seen := map[string]HistoryEntry{}
	cursor := ""
	newestAnchor := ""
	pages := 0
	for {
		page, e := s.ListHistory(ctx, "alice", j.ID, HistoryQuery{Limit: 17, Cursor: cursor})
		if e != nil {
			t.Fatal(e)
		}
		if pages == 0 {
			newestAnchor = page.NewerCursor
		}
		pages++
		for i, entry := range page.Entries {
			if i > 0 && entry.Sequence <= page.Entries[i-1].Sequence {
				t.Fatal("page not chronological")
			}
			if _, exists := seen[entry.ID]; exists {
				t.Fatal("duplicate across older pages")
			}
			seen[entry.ID] = entry
		}
		if page.OlderCursor == "" {
			break
		}
		cursor = page.OlderCursor
	}
	if len(seen) != 133 || pages < 7 {
		t.Fatalf("history truncated: entries=%d pages=%d", len(seen), pages)
	}
	if seen[first.ID].Text != "first durable note" {
		t.Fatal("oldest note evicted")
	}
	large := false
	for _, entry := range seen {
		if entry.Run != nil && len(entry.Run.Result) > 100000 {
			large = true
		}
	}
	if !large {
		t.Fatal("large output truncated")
	}
	runs, e := s.ListRuns(ctx, "alice", j.ID, 100, "")
	if e != nil || len(runs.Runs) > 50 {
		t.Fatal("run cache contract changed", e)
	}
	// Reconnect reads only new events after the most recent previously seen anchor.
	j = current(t, s, j.ID)
	extra, _, e := s.AddNote(ctx, "alice", j.ID, NoteInput{Kind: "note", Text: "after reconnect"}, j.Version, "note-after-reconnect")
	if e != nil {
		t.Fatal(e)
	}
	page, e := s.ListHistory(ctx, "alice", j.ID, HistoryQuery{After: newestAnchor, Limit: 2})
	if e != nil || len(page.Entries) != 1 || page.Entries[0].ID != extra.ID {
		t.Fatalf("bad reconnect %+v %v", page, e)
	}
	if _, e = s.ListHistory(ctx, "bob", j.ID, HistoryQuery{Cursor: cursor}); !errors.Is(e, ErrValidation) {
		t.Fatal("cross-user cursor accepted", e)
	}
	if _, e = s.ListHistory(ctx, "bob", j.ID, HistoryQuery{}); !errors.Is(e, ErrNotFound) {
		t.Fatal("cross-user job accepted", e)
	}
	path := fs.path
	if e = fs.Close(); e != nil {
		t.Fatal(e)
	}
	reopened, e := NewFileStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	page, e = NewService(reopened).ListHistory(ctx, "alice", j.ID, HistoryQuery{Limit: 100})
	if e != nil || len(page.Entries) != 100 || page.OlderCursor == "" {
		t.Fatal("restart lost events", e)
	}
}

func TestJobNoteReplayAfterOtherChangesReturnsOriginalReceipt(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	j := create(t, s, input("review"))
	version := j.Version
	first, updated, e := s.AddNote(ctx, "alice", j.ID, NoteInput{Kind: "note", Text: "Keep this exact note"}, version, "note-replay-request")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.RunNow(ctx, "alice", j.ID, updated.Version, "run-after-note"); e != nil {
		t.Fatal(e)
	}
	again, _, e := s.AddNote(ctx, "alice", j.ID, NoteInput{Kind: "note", Text: "Keep this exact note"}, version, "note-replay-request")
	if e != nil || *first != *again {
		t.Fatalf("replay changed receipt %+v %+v %v", first, again, e)
	}
	p, e := s.ListHistory(ctx, "alice", j.ID, HistoryQuery{Kind: "note", Limit: 100})
	if e != nil || len(p.Entries) != 1 || p.Entries[0].Role != "user" || p.Entries[0].Actor != "alice" {
		t.Fatalf("duplicate/spoofed note %+v %v", p, e)
	}
	if _, _, e = s.AddNote(ctx, "alice", j.ID, NoteInput{Kind: "steer", Text: "execute code"}, current(t, s, j.ID).Version, "unsupported-steer"); !errors.Is(e, ErrUnsupported) {
		t.Fatal(e)
	}
	if _, _, e = s.AddNote(ctx, "alice", j.ID, NoteInput{Kind: "note", Text: "Changed"}, version, "note-replay-request"); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
}

func TestJobHistoryReturnedSnapshotsCannotMutateStoredReceipts(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	j := create(t, s, input("reminder"))
	if _, e := s.RunNow(ctx, "alice", j.ID, j.Version, "immutable-run-request"); e != nil {
		t.Fatal(e)
	}
	p, e := s.ListHistory(ctx, "alice", j.ID, HistoryQuery{})
	if e != nil {
		t.Fatal(e)
	}
	var original string
	for _, entry := range p.Entries {
		if entry.Run != nil {
			original = entry.Run.Result
			entry.Run.Result = "caller corrupted saved output"
		}
	}
	if original == "" {
		t.Fatal("fixture produced no receipt")
	}
	again, e := s.ListHistory(ctx, "alice", j.ID, HistoryQuery{})
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range again.Entries {
		if entry.Run != nil && entry.Run.Result != original {
			t.Fatal("returned history aliases persisted receipt")
		}
	}
}

func TestJobHistoryCASFailureDoesNotCommitNoteOrLoseLegacyFile(t *testing.T) {
	s, fs, _ := fixture(t)
	ctx := context.Background()
	j := create(t, s, input("reminder"))
	before, _ := s.ListHistory(ctx, "alice", j.ID, HistoryQuery{})
	goodPath := fs.path
	fs.path = filepath.Join(t.TempDir(), "missing-directory", "jobs.json")
	if _, _, e := s.AddNote(ctx, "alice", j.ID, NoteInput{Kind: "note", Text: "must not commit"}, j.Version, "failed-save-note"); e == nil {
		t.Fatal("save should fail")
	}
	fs.path = goodPath
	after, _ := s.ListHistory(ctx, "alice", j.ID, HistoryQuery{})
	if len(after.Entries) != len(before.Entries) || current(t, s, j.ID).Version != j.Version {
		t.Fatal("partial commit")
	}
	legacy := map[string]Record{}
	record, _ := fs.Get(ctx, "alice", j.ID)
	legacy[recordKey("alice", j.ID)] = *record
	b, _ := json.Marshal(legacy)
	path := filepath.Join(t.TempDir(), "legacy.json")
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	old, e := NewFileStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer old.Close()
	legacyService := NewService(old)
	p, e := legacyService.ListHistory(ctx, "alice", j.ID, HistoryQuery{})
	if e != nil || len(p.Entries) != 0 || !p.LegacyHistoryAvailable {
		t.Fatal("fabricated legacy backfill", e)
	}
	if _, _, e = legacyService.AddNote(ctx, "alice", j.ID, NoteInput{Kind: "note", Text: "new event"}, j.Version, "legacy-note-request"); e != nil {
		t.Fatal(e)
	}
}

func TestDynamoHistorySharesCASAndOwnerPartitionWithoutTTL(t *testing.T) {
	f := &fakeDynamo{}
	s := NewDynamoStoreWithClient(f, "table")
	r := &Record{UserID: "alice", Job: Job{ID: "job_12345678901234567890123456789012", Version: 1, Status: "active", Title: "Job"}}
	if e := s.CompareAndSwap(context.Background(), r, 0); e != nil {
		t.Fatal(e)
	}
	if len(f.tx.TransactItems) != 3 {
		t.Fatal("job and history not one transaction")
	}
	history := f.tx.TransactItems[2].Put
	if attributeString(history.Item, "pk") != "USER#alice" || !strings.HasPrefix(attributeString(history.Item, "sk"), historyPrefix(r.Job.ID)) || aws.ToString(history.ConditionExpression) != "attribute_not_exists(pk)" {
		t.Fatal("unsafe immutable event")
	}
	if _, ok := history.Item["ttl"]; ok {
		t.Fatal("history expires silently")
	}
	setFakeHistoryBase(f, r, 1)
	r.Job.Version = 2
	r.pendingEntries = []HistoryEntry{{Kind: "note", Text: "note", Role: "user", Actor: "alice"}}
	f.txErr = &types.TransactionCanceledException{CancellationReasons: []types.CancellationReason{{Code: aws.String("None")}, {Code: aws.String("ConditionalCheckFailed")}, {Code: aws.String("None")}}}
	if e := s.CompareAndSwap(context.Background(), r, 1); !errors.Is(e, ErrConflict) {
		t.Fatal("CAS not fenced", e)
	}
	entry := HistoryEntry{ID: "e1", JobID: r.Job.ID, Sequence: 100, Kind: "note", Role: "user"}
	raw, _ := json.Marshal(entry)
	f.pages = []*dynamodb.QueryOutput{{Items: []map[string]types.AttributeValue{{"pk": avs("USER#alice"), "sk": avs(historyKey(entry)), "payload": avs(string(raw))}}}}
	page, e := s.ListHistory(context.Background(), "alice", r.Job.ID, HistoryQuery{})
	if e != nil || len(page.Entries) != 1 || !aws.ToBool(f.query.ConsistentRead) || attributeString(f.query.ExpressionAttributeValues, ":pk") != "USER#alice" {
		t.Fatal("unsafe history query", e)
	}
}

func TestDynamoHistorySparsePagesProgressAndRejectCorruptOwnership(t *testing.T) {
	ctx := context.Background()
	id := "job_12345678901234567890123456789012"
	f := &fakeDynamo{}
	s := NewDynamoStoreWithClient(f, "table")
	entry := HistoryEntry{ID: "note-one", JobID: id, Sequence: 1500, Kind: "note", Role: "user"}
	b, _ := json.Marshal(entry)
	item := map[string]types.AttributeValue{"pk": avs("USER#alice"), "sk": avs(historyKey(entry)), "payload": avs(string(b))}
	for n := int64(0); n < 10; n++ {
		key := map[string]types.AttributeValue{"pk": avs("USER#alice"), "sk": avs(historyKey(HistoryEntry{JobID: id, Sequence: 3000 - n*100}))}
		f.pages = append(f.pages, &dynamodb.QueryOutput{LastEvaluatedKey: key})
	}
	p, e := s.ListHistory(ctx, "alice", id, HistoryQuery{Kind: "note", Limit: 2})
	if e != nil || !p.HasMore || p.OlderCursor == "" || len(p.Entries) != 0 || f.queries != 10 {
		t.Fatalf("sparse query failed to preserve continuation: %+v %v", p, e)
	}
	f.pages = []*dynamodb.QueryOutput{{Items: []map[string]types.AttributeValue{item}}}
	p, e = s.ListHistory(ctx, "alice", id, HistoryQuery{Kind: "note", Limit: 2, Cursor: p.OlderCursor})
	if e != nil || len(p.Entries) != 1 || p.Entries[0].ID != entry.ID || attributeString(f.query.ExclusiveStartKey, "sk") == "" {
		t.Fatalf("sparse query lost later note: %+v %v", p, e)
	}
	// The decoded payload must agree with the actual partition and sort key.
	item["pk"] = avs("USER#bob")
	f.pages = []*dynamodb.QueryOutput{{Items: []map[string]types.AttributeValue{item}}}
	if _, e = s.ListHistory(ctx, "alice", id, HistoryQuery{}); !errors.Is(e, ErrCorrupt) {
		t.Fatal("accepted an entry from another owner", e)
	}
	// after is a bounded same-job ascending key query, never a table scan.
	if _, e = s.ListHistory(ctx, "alice", id, HistoryQuery{After: encodeHistoryCursor("alice", entry, "")}); e != nil || !aws.ToBool(f.query.ScanIndexForward) || !strings.Contains(aws.ToString(f.query.KeyConditionExpression), "BETWEEN") {
		t.Fatal("invalid reconnect query", e)
	}
}

func TestJobHistoryFileRejectsIncompleteSnapshotAndImmutableKeyCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incomplete.json")
	original := []byte(`{"formatVersion":2,"records":{}}`)
	if e := os.WriteFile(path, original, 0600); e != nil {
		t.Fatal(e)
	}
	if s, e := NewFileStore(path); e == nil {
		s.Close()
		t.Fatal("incomplete durable snapshot accepted")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(original) {
		t.Fatal("corrupt original changed")
	}
	s, fs, _ := fixture(t)
	j := create(t, s, input("reminder"))
	r, _ := fs.Get(context.Background(), "alice", j.ID)
	// Invalid adapter caller attempts to reuse an already committed sequence.
	r.pendingEntries = []HistoryEntry{{Kind: "note", Text: "overwrite"}}
	if e := fs.CompareAndSwap(context.Background(), r, r.Job.Version); !errors.Is(e, ErrConflict) {
		t.Fatal("immutable history sequence overwritten", e)
	}
	p, e := s.ListHistory(context.Background(), "alice", j.ID, HistoryQuery{})
	if e != nil || len(p.Entries) != 1 || p.Entries[0].Kind != "job_created" {
		t.Fatal("failed collision changed history", e)
	}
}

func TestJobHistoryCompactionArchivesEveryNewCancellation(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	in := input("review")
	in.Instructions = strings.Repeat("\x01", 2000)
	j := create(t, s, in)
	pending := 0
	for n := 0; n < MaxRuns; n++ {
		j = current(t, s, j.ID)
		_, e := s.RunNow(ctx, "alice", j.ID, j.Version, fmt.Sprintf("archive-run-%03d", n))
		if errors.Is(e, ErrLimit) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		pending++
	}
	for n := 0; n < 100; n++ {
		j = current(t, s, j.ID)
		_, _, e := s.AddNote(ctx, "alice", j.ID, NoteInput{Kind: "note", Text: "capacity padding"}, j.Version, fmt.Sprintf("archive-note-%03d", n))
		if errors.Is(e, ErrLimit) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	j = current(t, s, j.ID)
	in.Title = strings.Repeat("\x01", 160)
	if _, e := s.Update(ctx, "alice", j.ID, in, j.Version, "archive-update-request"); e != nil {
		t.Fatal(e)
	}
	cancelled, cursor := 0, ""
	for {
		p, e := s.ListHistory(ctx, "alice", j.ID, HistoryQuery{Limit: 100, Cursor: cursor})
		if e != nil {
			t.Fatal(e)
		}
		for _, entry := range p.Entries {
			if entry.Kind == "run_cancelled" {
				cancelled++
			}
		}
		if p.OlderCursor == "" {
			break
		}
		cursor = p.OlderCursor
	}
	if pending < 2 || pending != cancelled {
		t.Fatalf("compaction lost terminal state: %d pending, %d archived cancellations", pending, cancelled)
	}
}

func TestDynamoInactiveSuspensionCreatesNoHistoryKeysDuringPurge(t *testing.T) {
	for _, status := range []string{"deleting", "disabled", ""} {
		t.Run(status, func(t *testing.T) {
			r := &Record{UserID: "alice", Job: Job{ID: "job_12345678901234567890123456789012", Version: 1, Status: "active"}}
			f := &fakeDynamo{profile: map[string]types.AttributeValue{"status": avs(status)}}
			setFakeHistoryBase(f, r, 1)
			if e := NewDynamoStoreWithClient(f, "table").SuspendInactive(context.Background(), Ref{UserID: "alice", ID: r.Job.ID}); e != nil {
				t.Fatal(e)
			}
			puts := 0
			for _, operation := range f.tx.TransactItems {
				if operation.Put == nil {
					continue
				}
				puts++
				if attributeString(operation.Put.Item, "sk") != "JOB#"+r.Job.ID || aws.ToString(operation.Put.ConditionExpression) != "version = :version" {
					t.Fatal("suspension could create a key absent from purge snapshot")
				}
			}
			if puts != 1 {
				t.Fatal("inactive suspension created event keys")
			}
		})
	}
}

type fakeConversation struct {
	calls int
	users []string
	page  ConversationPage
}

func (f *fakeConversation) ReadConversation(_ context.Context, uid, _, _ string, _ int) (ConversationPage, error) {
	f.calls++
	f.users = append(f.users, uid)
	return f.page, nil
}
func TestJobConversationAdapterFailClosedAndPreservesPagination(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	j := create(t, s, input("review"))
	if _, e := s.Conversation(ctx, "alice", j.ID, "", 30); !errors.Is(e, ErrUnsupported) {
		t.Fatal(e)
	}
	f := &fakeConversation{page: ConversationPage{Messages: []ConversationMessage{{ID: "m1", Role: "user", Text: strings.Repeat("long", 10000)}}, HasMore: true, NextCursor: "page2", RetentionBoundary: "provider retained history"}}
	s = s.WithConversationReader(f)
	if _, e := s.Conversation(ctx, "bob", j.ID, "", 30); !errors.Is(e, ErrNotFound) || f.calls != 0 {
		t.Fatal("ownership bypass", e)
	}
	p, e := s.Conversation(ctx, "alice", j.ID, "", 30)
	if e != nil || len(p.Messages[0].Text) != 40000 || p.NextCursor != "page2" {
		t.Fatal("provider page truncated", e)
	}
	if _, e = s.Conversation(ctx, "alice", j.ID, "page2", 30); !errors.Is(e, ErrCorrupt) {
		t.Fatal("nonprogressing cursor accepted", e)
	}
}
