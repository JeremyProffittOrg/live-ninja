package tools

// Tests for the file_list / file_read / file_create tool surface: list
// pagination pass-through, read caps + binary refusal, create-new vs.
// create-existing (already_exists), schema-gate traversal rejection, and
// the owner invariant that no delete/overwrite tool exists at all.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/deliv"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// ---- fakeDelivService: file-surface methods (struct in deliverable_test.go) ----

func (f *fakeDelivService) List(ctx context.Context, userID string, limit int32, cursor string) ([]store.Deliverable, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	f.listLimit, f.listCursor = limit, cursor
	return f.listItems, f.listNext, nil
}

func (f *fakeDelivService) FindByName(ctx context.Context, userID, name string) (*store.Deliverable, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byName[name], nil
}

func (f *fakeDelivService) ReadContent(ctx context.Context, userID, deliverableID string) (*store.Deliverable, []byte, bool, error) {
	f.readID = deliverableID
	if f.readErr != nil {
		return f.readDeliv, nil, false, f.readErr
	}
	if f.err != nil {
		return nil, nil, false, f.err
	}
	return f.readDeliv, f.readContent, f.readTruncated, nil
}

// ---- file_list ----

func TestFileListPassesPaginationAndMapsFields(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{
		listItems: []store.Deliverable{
			{DeliverableID: "d-1", Name: "a.md", SizeBytes: 10, CreatedAt: "2026-07-18T01:00:00Z",
				ContentType: "text/markdown; charset=utf-8", Status: store.DeliverableStatusReady, Kind: store.DeliverableKindFile},
			{DeliverableID: "d-2", Name: "b.zip", SizeBytes: 999, CreatedAt: "2026-07-17T01:00:00Z",
				ContentType: "application/zip", Status: store.DeliverableStatusPending, Kind: store.DeliverableKindZip},
		},
		listNext: "cursor-2",
	}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), invocation("file_list", map[string]any{
		"limit":  float64(2),
		"cursor": "cursor-1",
	}))
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.EqualValues(t, 2, fake.listLimit)
	assert.Equal(t, "cursor-1", fake.listCursor)
	assert.Equal(t, "cursor-2", res.Output["nextCursor"])
	assert.Equal(t, 2, res.Output["count"])

	files := res.Output["files"].([]map[string]any)
	require.Len(t, files, 2)
	assert.Equal(t, "d-1", files[0]["fileId"])
	assert.Equal(t, "a.md", files[0]["name"])
	assert.Equal(t, int64(10), files[0]["sizeBytes"])
	assert.Equal(t, "2026-07-18T01:00:00Z", files[0]["createdAt"])
	assert.Equal(t, "text/markdown; charset=utf-8", files[0]["contentType"])
}

func TestFileListDefaultsAndBounds(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	// No args → default page size, no cursor, and no nextCursor key when
	// the store reports no further pages.
	res := r.Invoke(context.Background(), invocation("file_list", map[string]any{}))
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.EqualValues(t, defaultFileListLimit, fake.listLimit)
	assert.Empty(t, fake.listCursor)
	_, hasNext := res.Output["nextCursor"]
	assert.False(t, hasNext)

	// The schema gate caps limit at 100 (never a bigger read).
	res = r.Invoke(context.Background(), invocation("file_list", map[string]any{"limit": float64(101)}))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
	res = r.Invoke(context.Background(), invocation("file_list", map[string]any{"limit": float64(0)}))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
}

// ---- file_read ----

func TestFileReadByIDReturnsContent(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{
		readDeliv: &store.Deliverable{DeliverableID: "d-1", Name: "a.md",
			ContentType: "text/markdown; charset=utf-8", SizeBytes: 7},
		readContent: []byte("# Hello"),
	}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), invocation("file_read", map[string]any{"fileId": "d-1"}))
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "d-1", fake.readID)
	assert.Equal(t, "# Hello", res.Output["content"])
	assert.Equal(t, false, res.Output["truncated"])
	_, hasNote := res.Output["note"]
	assert.False(t, hasNote)
}

func TestFileReadByNameResolvesThenReads(t *testing.T) {
	deps := newTestDeps()
	d := &store.Deliverable{DeliverableID: "d-9", Name: "notes.md", ContentType: "text/markdown", SizeBytes: 2}
	fake := &fakeDelivService{
		byName:      map[string]*store.Deliverable{"notes.md": d},
		readDeliv:   d,
		readContent: []byte("hi"),
	}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), invocation("file_read", map[string]any{"name": "notes.md"}))
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "d-9", fake.readID)
	assert.Equal(t, "hi", res.Output["content"])

	// Unknown name → not_found with a pointer at file_list.
	res = r.Invoke(context.Background(), invocation("file_read", map[string]any{"name": "ghost.md"}))
	require.False(t, res.OK)
	assert.Equal(t, CodeNotFound, res.Error.Code)
	assert.Contains(t, res.Error.Message, "file_list")

	// Neither id nor name → invalid_args.
	res = r.Invoke(context.Background(), invocation("file_read", map[string]any{}))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
}

func TestFileReadTruncationCarriesNote(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{
		readDeliv: &store.Deliverable{DeliverableID: "d-1", Name: "big.txt",
			ContentType: "text/plain", SizeBytes: 500_000},
		readContent:   []byte(strings.Repeat("x", deliv.MaxReadBytes)),
		readTruncated: true,
	}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), invocation("file_read", map[string]any{"fileId": "d-1"}))
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, true, res.Output["truncated"])
	assert.Len(t, res.Output["content"], deliv.MaxReadBytes)
	note, _ := res.Output["note"].(string)
	assert.Contains(t, note, "deliverable_deliver", "truncation note must steer to a download link")
}

func TestFileReadRefusesBinaryWithDownloadHint(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{
		readDeliv: &store.Deliverable{DeliverableID: "z-1", Name: "bundle.zip", ContentType: "application/zip"},
		readErr:   deliv.ErrNotText,
	}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), invocation("file_read", map[string]any{"fileId": "z-1"}))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
	assert.Contains(t, res.Error.Message, "application/zip")
	assert.Contains(t, res.Error.Message, "deliverable_deliver")
}

func TestFileReadNameTraversalRejectedAtSchemaGate(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	for _, bad := range []string{
		"../secrets.md", "..", "a/../b.md", "a/b.md", `a\b.md`,
		".hidden", "bad\x00name.md", "sp ace.md", "quote\".md",
	} {
		res := r.Invoke(context.Background(), invocation("file_read", map[string]any{"name": bad}))
		require.False(t, res.OK, "name %q must be rejected", bad)
		assert.Equal(t, CodeInvalidArgs, res.Error.Code, "name %q", bad)
	}
	assert.Empty(t, fake.readID, "no traversal name may reach the service")
}

// ---- file_create ----

func TestFileCreateDefaultsAndAppendsExtension(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	inv := invocation("file_create", map[string]any{
		"name":    "meeting-notes",
		"content": "# Agenda",
	})
	inv.IdempotencyKey = "idk-1"
	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "created", res.Output["status"])
	assert.Equal(t, "d-1", res.Output["fileId"])
	assert.Equal(t, "meeting-notes.md", fake.createName, "default text/markdown extension must be appended")
	assert.Equal(t, "text/markdown; charset=utf-8", fake.createType)
	assert.Equal(t, "# Agenda", string(fake.createContent))

	// Explicit contentType + a name that already has an extension.
	inv2 := invocation("file_create", map[string]any{
		"name": "data.json", "content": `{"a":1}`, "contentType": "application/json",
	})
	inv2.IdempotencyKey = "idk-2"
	res = r.Invoke(context.Background(), inv2)
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "data.json", fake.createName)
	assert.Equal(t, "application/json", fake.createType)
}

func TestFileCreateExistingNameRejected(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{err: deliv.ErrNameTaken}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	inv := invocation("file_create", map[string]any{"name": "notes.md", "content": "x"})
	inv.IdempotencyKey = "idk-1"
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeAlreadyExists, res.Error.Code)
	assert.Contains(t, res.Error.Message, "never overwritten")
	assert.Equal(t, 409, res.StatusCode())
}

func TestFileCreateTraversalAndBadTypeRejected(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	for _, bad := range []string{
		"../../etc/passwd", "..", "dir/inner.md", `dir\inner.md`,
		".hidden.md", "-flag.md", "nul\x01byte", "sp ace.md",
	} {
		inv := invocation("file_create", map[string]any{"name": bad, "content": "x"})
		inv.IdempotencyKey = "idk-" + bad
		res := r.Invoke(context.Background(), inv)
		require.False(t, res.OK, "name %q must be rejected", bad)
		assert.Equal(t, CodeInvalidArgs, res.Error.Code, "name %q", bad)
	}
	assert.Empty(t, fake.createName, "no traversal name may reach the service")

	// contentType outside the closed text-like enum is rejected.
	inv := invocation("file_create", map[string]any{
		"name": "a.bin", "content": "x", "contentType": "application/octet-stream",
	})
	inv.IdempotencyKey = "idk-ct"
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
}

func TestFileCreateRequiresIdempotencyKey(t *testing.T) {
	deps := newTestDeps()
	deps.Deliverables = &fakeDelivService{}
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), invocation("file_create", map[string]any{
		"name": "a.md", "content": "x",
	}))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
}

// ---- owner invariant: no delete/overwrite surface ----

func TestNoFileDeleteOrOverwriteToolExists(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())

	names := make([]string, 0)
	for _, tool := range r.Manifest() {
		names = append(names, tool["name"].(string))
	}

	// The complete file surface is list/read/create — nothing else.
	fileTools := make([]string, 0)
	for _, n := range names {
		if strings.HasPrefix(n, "file_") {
			fileTools = append(fileTools, n)
		}
	}
	assert.ElementsMatch(t, []string{"file_list", "file_read", "file_create"}, fileTools)

	// No tool in the whole catalog can delete or overwrite a document.
	for _, forbidden := range []string{
		"file_delete", "file_remove", "file_update", "file_overwrite", "file_write",
		"deliverable_delete", "deliverable_update", "deliverable_overwrite",
	} {
		res := r.Invoke(context.Background(), invocation(forbidden, map[string]any{}))
		require.False(t, res.OK)
		assert.Equal(t, CodeUnknownTool, res.Error.Code, "tool %q must not exist", forbidden)
	}
}
