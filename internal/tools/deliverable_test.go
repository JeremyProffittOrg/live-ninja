package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/deliv"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// fakeDelivService implements DeliverableService, recording calls and
// returning scripted results — the tool layer's only job is mapping args
// in and results/sentinels out, so that is all these tests assert.
type fakeDelivService struct {
	createName, createType string
	createContent          []byte
	zipIDs                 []string
	zipName                string
	deliverID, deliverTo   string

	// file_list / file_read scripting (methods in file_test.go).
	listItems     []store.Deliverable
	listNext      string
	listLimit     int32
	listCursor    string
	byName        map[string]*store.Deliverable
	readDeliv     *store.Deliverable
	readContent   []byte
	readTruncated bool
	readID        string
	readErr       error

	err error
}

func (f *fakeDelivService) Create(ctx context.Context, userID, filename, contentType string, content []byte) (*store.Deliverable, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.createName, f.createType, f.createContent = filename, contentType, content
	return &store.Deliverable{
		DeliverableID: "d-1", UserID: userID, Name: filename,
		ContentType: contentType, Kind: store.DeliverableKindFile,
		Status: store.DeliverableStatusReady, SizeBytes: int64(len(content)),
	}, nil
}

func (f *fakeDelivService) Zip(ctx context.Context, userID string, ids []string, zipName string) (*store.Deliverable, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.zipIDs, f.zipName = ids, zipName
	return &store.Deliverable{
		DeliverableID: "z-1", UserID: userID, Name: "bundle.zip",
		Kind: store.DeliverableKindZip, Status: store.DeliverableStatusPending,
		Sources: ids,
	}, nil
}

func (f *fakeDelivService) Deliver(ctx context.Context, userID, deliverableID, emailTo string) (*deliv.DeliverResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.deliverID, f.deliverTo = deliverableID, emailTo
	return &deliv.DeliverResult{
		Deliverable: &store.Deliverable{DeliverableID: deliverableID, Name: "report.md"},
		URL:         "https://signed.example/x",
		EmailedTo:   emailTo,
	}, nil
}

func TestDeliverableCreateAppendsExtensionAndMapsResult(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	inv := invocation("deliverable_create", map[string]any{
		"name":    "trip-plan",
		"format":  "markdown",
		"content": "# Plan",
	})
	inv.IdempotencyKey = "idk-1"
	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "created", res.Output["status"])
	assert.Equal(t, "d-1", res.Output["deliverableId"])
	assert.Equal(t, "trip-plan.md", fake.createName, "extension for the format must be appended")
	assert.Equal(t, "text/markdown; charset=utf-8", fake.createType)
	assert.Equal(t, "# Plan", string(fake.createContent))

	// A name that already carries the extension is left alone.
	inv2 := invocation("deliverable_create", map[string]any{
		"name": "notes.md", "format": "markdown", "content": "x",
	})
	inv2.IdempotencyKey = "idk-2"
	res = r.Invoke(context.Background(), inv2)
	require.True(t, res.OK)
	assert.Equal(t, "notes.md", fake.createName)
}

func TestDeliverableCreateRejectsBadFormat(t *testing.T) {
	deps := newTestDeps()
	deps.Deliverables = &fakeDelivService{}
	r := newTestRegistry(t, deps)

	inv := invocation("deliverable_create", map[string]any{
		"name": "x", "format": "exe", "content": "boom",
	})
	inv.IdempotencyKey = "idk-1"
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
}

func TestDeliverableToolsNotConfigured(t *testing.T) {
	r := newTestRegistry(t, newTestDeps()) // Deliverables nil

	inv := invocation("deliverable_create", map[string]any{
		"name": "x", "format": "text", "content": "y",
	})
	inv.IdempotencyKey = "idk-1"
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeNotConfigured, res.Error.Code)

	res = r.Invoke(context.Background(), invocation("deliverable_deliver", map[string]any{
		"deliverableId": "d-1",
	}))
	require.False(t, res.OK)
	assert.Equal(t, CodeNotConfigured, res.Error.Code)
}

func TestDeliverableZipPassesIDsThrough(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	inv := invocation("deliverable_zip", map[string]any{
		"deliverableIds": []any{"a", "b"},
		"name":           "vacation",
	})
	inv.IdempotencyKey = "idk-1"
	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, []string{"a", "b"}, fake.zipIDs)
	assert.Equal(t, "vacation", fake.zipName)
	assert.Equal(t, store.DeliverableStatusPending, res.Output["status"])
	assert.Equal(t, 2, res.Output["sourceCount"])
}

func TestDeliverableZipRejectsEmptyIDs(t *testing.T) {
	deps := newTestDeps()
	deps.Deliverables = &fakeDelivService{}
	r := newTestRegistry(t, deps)

	inv := invocation("deliverable_zip", map[string]any{"deliverableIds": []any{}})
	inv.IdempotencyKey = "idk-1"
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
}

func TestDeliverableDeliverLinkAndSentinelMapping(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	// Default method: link only, no email resolution needed.
	res := r.Invoke(context.Background(), invocation("deliverable_deliver", map[string]any{
		"deliverableId": "d-1",
	}))
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "delivered", res.Output["status"])
	assert.Equal(t, "https://signed.example/x", res.Output["url"])
	assert.Empty(t, fake.deliverTo)
	_, hasEmailed := res.Output["emailedTo"]
	assert.False(t, hasEmailed)

	// Service sentinels map to client-safe codes.
	fake.err = deliv.ErrNotFound
	res = r.Invoke(context.Background(), invocation("deliverable_deliver", map[string]any{
		"deliverableId": "ghost",
	}))
	require.False(t, res.OK)
	assert.Equal(t, CodeNotFound, res.Error.Code)

	fake.err = deliv.ErrNotReady
	res = r.Invoke(context.Background(), invocation("deliverable_deliver", map[string]any{
		"deliverableId": "d-1",
	}))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
}

func TestDeliverableDeliverEmailUsesOwnInboxOnly(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeDelivService{}
	deps.Deliverables = fake
	r := newTestRegistry(t, deps)

	// The calling user has a stored email — delivery goes there, and the
	// tool accepts no recipient argument at all (own-inbox only).
	require.NoError(t, deps.Store.CreateUser(context.Background(), &store.User{
		UserID: "user-1", AmazonUserID: "amzn-1", Email: "me@example.com", Status: "active",
	}))
	res := r.Invoke(context.Background(), invocation("deliverable_deliver", map[string]any{
		"deliverableId": "d-1",
		"method":        "email",
	}))
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "me@example.com", fake.deliverTo)
	assert.Equal(t, "me@example.com", res.Output["emailedTo"])
}
