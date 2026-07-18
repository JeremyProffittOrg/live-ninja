package webapp

// HTTP-surface tests for the M9 deliverables routes: list pagination,
// the presigned-download redirect (302 + no-store), state/ownership
// error mapping, delete, and the not-configured 503 — over a real
// deliv.Service wired to in-memory fakes (S3/presign) and a
// FakeDynamo-backed store, behind the same stub-auth harness the
// settings API tests use.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/deliv"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

type routeFakeS3 struct{ deleted []string }

func (f *routeFakeS3) PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	_, _ = io.Copy(io.Discard, params.Body)
	return &s3.PutObjectOutput{}, nil
}

func (f *routeFakeS3) DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.deleted = append(f.deleted, aws.ToString(params.Key))
	return &s3.DeleteObjectOutput{}, nil
}

type routeFakePresign struct{}

func (routeFakePresign) PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	return &v4.PresignedHTTPRequest{URL: "https://signed.example/" + aws.ToString(params.Key) + "?sig=abc"}, nil
}

// newDeliverablesAPIApp mounts the deliverables handlers as user u1.
func newDeliverablesAPIApp(t *testing.T) (*fiber.App, *deliv.Service, *store.Store, *routeFakeS3) {
	t.Helper()
	st := store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja")
	fakeS3 := &routeFakeS3{}
	svc, err := deliv.New(deliv.Config{
		S3:      fakeS3,
		Presign: routeFakePresign{},
		Store:   st,
		Bucket:  "deliv-bucket",
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("deliv.New: %v", err)
	}
	deps := &Deps{
		Store: st,
		Deliv: svc,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		return c.Next()
	})
	app.Get("/api/v1/deliverables", handleListDeliverables(deps))
	app.Get("/api/v1/deliverables/:id/download", handleDownloadDeliverable(deps))
	app.Delete("/api/v1/deliverables/:id", handleDeleteDeliverable(deps))
	return app, svc, st, fakeS3
}

func TestListDeliverablesEndpoint(t *testing.T) {
	app, svc, _, _ := newDeliverablesAPIApp(t)
	ctx := context.Background()

	// Empty store: empty array (not null), no cursor.
	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/deliverables", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if arr, ok := body["deliverables"].([]any); !ok || len(arr) != 0 {
		t.Errorf("empty list: deliverables = %#v, want []", body["deliverables"])
	}
	if _, has := body["nextCursor"]; has {
		t.Errorf("empty list must not return nextCursor")
	}

	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if _, err := svc.Create(ctx, "u1", name, "text/markdown", []byte("x")); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	// Another user's row must not appear.
	if _, err := svc.Create(ctx, "u2", "theirs.md", "text/markdown", []byte("x")); err != nil {
		t.Fatalf("seed theirs: %v", err)
	}

	resp, body = doJSON(t, app, http.MethodGet, "/api/v1/deliverables?limit=2", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	arr, _ := body["deliverables"].([]any)
	if len(arr) != 2 {
		t.Fatalf("page 1 len = %d, want 2", len(arr))
	}
	row, _ := arr[0].(map[string]any)
	for _, field := range []string{"deliverableId", "name", "kind", "status", "contentType", "sizeBytes", "createdAt"} {
		if _, ok := row[field]; !ok {
			t.Errorf("list row missing field %q", field)
		}
	}
	if _, leaked := row["s3Key"]; leaked {
		t.Errorf("list row must not leak s3Key")
	}
	cursor, _ := body["nextCursor"].(string)
	if cursor == "" {
		t.Fatalf("page 1 must return nextCursor")
	}

	resp, body = doJSON(t, app, http.MethodGet, "/api/v1/deliverables?limit=2&cursor="+cursor, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page 2 status = %d, want 200", resp.StatusCode)
	}
	arr, _ = body["deliverables"].([]any)
	if len(arr) != 1 {
		t.Errorf("page 2 len = %d, want 1 (u2's row must not bleed in)", len(arr))
	}

	// Bad inputs.
	resp, _ = doJSON(t, app, http.MethodGet, "/api/v1/deliverables?limit=0", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("limit=0 status = %d, want 400", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodGet, "/api/v1/deliverables?limit=101", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("limit=101 status = %d, want 400", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodGet, "/api/v1/deliverables?cursor=%21%21garbage", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad cursor status = %d, want 400", resp.StatusCode)
	}
}

func TestDownloadDeliverableRedirects(t *testing.T) {
	app, svc, st, _ := newDeliverablesAPIApp(t)
	ctx := context.Background()

	d, err := svc.Create(ctx, "u1", "report.md", "text/markdown", []byte("hi"))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/deliverables/"+d.DeliverableID+"/download", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "https://signed.example/"+d.S3Key+"?sig=abc" {
		t.Errorf("Location = %q", loc)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (credentialed redirect)", cc)
	}

	// Unknown id → 404; another user's id → identical 404.
	other, err := svc.Create(ctx, "u2", "theirs.md", "text/markdown", []byte("x"))
	if err != nil {
		t.Fatalf("seed other: %v", err)
	}
	for _, id := range []string{"ghost", other.DeliverableID} {
		resp, _ := doJSON(t, app, http.MethodGet, "/api/v1/deliverables/"+id+"/download", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("id %s: status = %d, want 404", id, resp.StatusCode)
		}
	}

	// Pending (not ready) → 409.
	pending := &store.Deliverable{
		DeliverableID: "p1", UserID: "u1", Name: "p.zip",
		ContentType: "application/zip", Kind: store.DeliverableKindZip,
		Status: store.DeliverableStatusPending,
		S3Key:  deliv.UserPrefix("u1") + "p1/p.zip", CreatedAt: "2026-07-17T11:00:00Z",
	}
	if err := st.CreateDeliverable(ctx, pending); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	resp2, body := doJSON(t, app, http.MethodGet, "/api/v1/deliverables/p1/download", nil)
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("pending status = %d, want 409 (%v)", resp2.StatusCode, body)
	}
}

func TestDeleteDeliverableEndpoint(t *testing.T) {
	app, svc, st, fakeS3 := newDeliverablesAPIApp(t)
	ctx := context.Background()

	d, err := svc.Create(ctx, "u1", "a.md", "text/markdown", []byte("x"))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp, _ := doJSON(t, app, http.MethodDelete, "/api/v1/deliverables/"+d.DeliverableID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if len(fakeS3.deleted) != 1 || fakeS3.deleted[0] != d.S3Key {
		t.Errorf("S3 object not deleted: %v", fakeS3.deleted)
	}
	if got, _ := st.GetDeliverable(ctx, "u1", d.DeliverableID); got != nil {
		t.Errorf("index item survived delete")
	}

	// Gone now → 404; another user's id → 404 without deleting anything.
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/deliverables/"+d.DeliverableID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("re-delete status = %d, want 404", resp.StatusCode)
	}
	other, err := svc.Create(ctx, "u2", "theirs.md", "text/markdown", []byte("x"))
	if err != nil {
		t.Fatalf("seed other: %v", err)
	}
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/deliverables/"+other.DeliverableID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-user delete status = %d, want 404", resp.StatusCode)
	}
	if got, _ := st.GetDeliverable(ctx, "u2", other.DeliverableID); got == nil {
		t.Errorf("cross-user delete must not remove the other user's item")
	}
}

func TestDeliverablesRoutesUnconfigured(t *testing.T) {
	deps := &Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))} // Deliv nil
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		return c.Next()
	})
	app.Get("/api/v1/deliverables", handleListDeliverables(deps))
	app.Get("/api/v1/deliverables/:id/download", handleDownloadDeliverable(deps))
	app.Delete("/api/v1/deliverables/:id", handleDeleteDeliverable(deps))

	for _, probe := range []struct{ method, url string }{
		{http.MethodGet, "/api/v1/deliverables"},
		{http.MethodGet, "/api/v1/deliverables/x/download"},
		{http.MethodDelete, "/api/v1/deliverables/x"},
	} {
		resp, _ := doJSON(t, app, probe.method, probe.url, nil)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s status = %d, want 503", probe.method, probe.url, resp.StatusCode)
		}
	}
}
