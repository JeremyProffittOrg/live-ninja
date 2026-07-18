package webapp

// Tests for the M7 privacy/account surface (account_routes.go): the
// consent ledger routes, the typed-confirmation + re-auth + async-invoke
// behavior of DELETE /api/v1/account, and the export-as-deliverable route
// (including credential-attribute stripping and part chunking).

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/deliv"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// accountFakeLambda records async purge invokes.
type accountFakeLambda struct {
	calls []*lambda.InvokeInput
	err   error
}

func (f *accountFakeLambda) Invoke(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	f.calls = append(f.calls, params)
	if f.err != nil {
		return nil, f.err
	}
	return &lambda.InvokeOutput{StatusCode: 202}, nil
}

// exportCaptureS3 keeps uploaded object bodies so tests can inspect the
// export JSON that deliv.Create wrote.
type exportCaptureS3 struct{ objects map[string][]byte }

func (f *exportCaptureS3) PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	b, _ := io.ReadAll(params.Body)
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[aws.ToString(params.Key)] = b
	return &s3.PutObjectOutput{}, nil
}

func (f *exportCaptureS3) DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	delete(f.objects, aws.ToString(params.Key))
	return &s3.DeleteObjectOutput{}, nil
}

// newAccountApp mounts the account routes as authenticated user u1 on the
// web surface, over a FakeDynamo store + capture-S3 deliv service.
func newAccountApp(t *testing.T) (*fiber.App, *store.Store, *accountFakeLambda, *exportCaptureS3) {
	t.Helper()
	st := store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja")
	fakeS3 := &exportCaptureS3{}
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
	fakeLambda := &accountFakeLambda{}
	deps := &Deps{
		Store:  st,
		Deliv:  svc,
		Lambda: fakeLambda,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		c.Locals(localSurface, "web")
		return c.Next()
	})
	RegisterAccountRoutes(app, deps)
	return app, st, fakeLambda, fakeS3
}

func seedMember(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.CreateUser(context.Background(), &store.User{
		UserID: "u1", AmazonUserID: "amzn1.account.u1",
		Email: "u1@example.com", Role: store.RoleMember, Status: store.UserStatusActive,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

// ---- consent ----

func TestConsentRecordAndList(t *testing.T) {
	app, _, _, _ := newAccountApp(t)

	// Missing version -> 400.
	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/consent", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no version: status = %d, want 400 (%v)", resp.StatusCode, body)
	}

	// Bad client ts -> 400.
	resp, _ = doJSON(t, app, http.MethodPost, "/api/v1/consent", map[string]any{
		"version": "v1", "ts": "yesterday",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad ts: status = %d, want 400", resp.StatusCode)
	}

	// Valid record: surface comes from the auth context, never the body.
	resp, body = doJSON(t, app, http.MethodPost, "/api/v1/consent", map[string]any{
		"version": "2026-07-privacy-v1", "ts": "2026-07-17T12:00:00Z",
		"surface": "device", // attacker-supplied; must be ignored
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("record: status = %d, want 201 (%v)", resp.StatusCode, body)
	}
	if body["surface"] != "web" {
		t.Errorf("surface = %v, want web (from auth context)", body["surface"])
	}

	resp, body = doJSON(t, app, http.MethodGet, "/api/v1/consent", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status = %d, want 200", resp.StatusCode)
	}
	consents, ok := body["consents"].([]any)
	if !ok || len(consents) != 1 {
		t.Fatalf("consents = %#v, want 1 entry", body["consents"])
	}
	first := consents[0].(map[string]any)
	if first["version"] != "2026-07-privacy-v1" || first["surface"] != "web" {
		t.Errorf("stored consent = %#v", first)
	}
	if first["clientTs"] != "2026-07-17T12:00:00Z" {
		t.Errorf("clientTs = %v", first["clientTs"])
	}
}

// ---- delete account ----

func TestDeleteAccountRequiresTypedConfirmation(t *testing.T) {
	app, st, fakeLambda, _ := newAccountApp(t)
	seedMember(t, st)
	t.Setenv("ACCOUNT_PURGE_FUNCTION_NAME", "live-ninja-account-purge")

	for _, body := range []any{nil, map[string]any{}, map[string]any{"confirm": "delete"}} {
		resp, out := doJSON(t, app, http.MethodDelete, "/api/v1/account", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("confirm %v: status = %d, want 400 (%v)", body, resp.StatusCode, out)
		}
	}
	if len(fakeLambda.calls) != 0 {
		t.Fatalf("purge invoked without confirmation")
	}
	u, _ := st.GetUser(context.Background(), "u1")
	if u.Status != store.UserStatusActive {
		t.Fatalf("status = %s, want active (untouched)", u.Status)
	}
}

func TestDeleteAccountOwnerRefused(t *testing.T) {
	app, st, fakeLambda, _ := newAccountApp(t)
	if err := st.CreateUser(context.Background(), &store.User{
		UserID: "u1", AmazonUserID: "amzn1.account.u1",
		Email: "owner@example.com", Role: store.RoleOwner, Status: store.UserStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACCOUNT_PURGE_FUNCTION_NAME", "live-ninja-account-purge")

	resp, body := doJSON(t, app, http.MethodDelete, "/api/v1/account", map[string]any{"confirm": "DELETE"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%v)", resp.StatusCode, body)
	}
	if body["error"] != "owner_undeletable" {
		t.Errorf("error = %v", body["error"])
	}
	if len(fakeLambda.calls) != 0 {
		t.Fatalf("purge invoked for owner")
	}
}

func TestDeleteAccountMarksDeletingAndInvokesPurge(t *testing.T) {
	app, st, fakeLambda, _ := newAccountApp(t)
	seedMember(t, st)
	t.Setenv("ACCOUNT_PURGE_FUNCTION_NAME", "live-ninja-account-purge")

	resp, body := doJSON(t, app, http.MethodDelete, "/api/v1/account", map[string]any{"confirm": "DELETE"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%v)", resp.StatusCode, body)
	}
	if body["status"] != "deleting" {
		t.Errorf("status body = %v", body["status"])
	}

	u, err := st.GetUser(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != store.UserStatusDeleting {
		t.Errorf("user status = %s, want deleting", u.Status)
	}
	if u.TokensValidAfter == 0 {
		t.Errorf("tokensValidAfter not bumped — outstanding JWTs would stay live")
	}

	if len(fakeLambda.calls) != 1 {
		t.Fatalf("purge invokes = %d, want 1", len(fakeLambda.calls))
	}
	call := fakeLambda.calls[0]
	if aws.ToString(call.FunctionName) != "live-ninja-account-purge" {
		t.Errorf("function = %s", aws.ToString(call.FunctionName))
	}
	if call.InvocationType != lambdatypes.InvocationTypeEvent {
		t.Errorf("invocation type = %s, want Event (async)", call.InvocationType)
	}
	var ev map[string]any
	if err := json.Unmarshal(call.Payload, &ev); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if ev["userId"] != "u1" || ev["email"] != "u1@example.com" || ev["amazonUserId"] != "amzn1.account.u1" {
		t.Errorf("purge event = %#v", ev)
	}

	// Second DELETE while already deleting: idempotent 202, no re-invoke.
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/account", map[string]any{"confirm": "DELETE"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("repeat: status = %d, want 202", resp.StatusCode)
	}
	if len(fakeLambda.calls) != 1 {
		t.Fatalf("repeat re-invoked the purge (calls = %d)", len(fakeLambda.calls))
	}
}

func TestDeleteAccountNotConfigured(t *testing.T) {
	app, st, _, _ := newAccountApp(t)
	seedMember(t, st)
	t.Setenv("ACCOUNT_PURGE_FUNCTION_NAME", "")

	resp, body := doJSON(t, app, http.MethodDelete, "/api/v1/account", map[string]any{"confirm": "DELETE"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%v)", resp.StatusCode, body)
	}
	// Fail closed without side effects: the account must not be left
	// half-deleting when the worker isn't wired.
	u, _ := st.GetUser(context.Background(), "u1")
	if u.Status != store.UserStatusActive {
		t.Errorf("status = %s, want active", u.Status)
	}
}

// ---- export ----

func TestAccountExportStripsCredentialMaterial(t *testing.T) {
	app, st, _, fakeS3 := newAccountApp(t)
	seedMember(t, st)
	ctx := context.Background()

	// A session row (credential hashes must never appear in an export) and
	// a transcript row (must appear).
	if err := st.CreateSession(ctx, &store.Session{
		SessionID: "sess-1", UserID: "u1", FamilyID: "fam-1",
		Surface: "web", RefreshHash: "deadbeef", PrevHash: "cafef00d",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ConditionalPut(ctx, "USER#u1", "LOG#sess-1#000001",
		map[string]any{"role": "user", "text": "hello world"}, 0); err != nil {
		t.Fatal(err)
	}

	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/account/export", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%v)", resp.StatusCode, body)
	}
	delivs, ok := body["deliverables"].([]any)
	if !ok || len(delivs) != 1 {
		t.Fatalf("deliverables = %#v, want exactly 1", body["deliverables"])
	}

	if len(fakeS3.objects) != 1 {
		t.Fatalf("s3 objects = %d, want 1", len(fakeS3.objects))
	}
	var content string
	for _, b := range fakeS3.objects {
		content = string(b)
	}
	if strings.Contains(content, "deadbeef") || strings.Contains(content, "cafef00d") {
		t.Errorf("export leaks refresh-token hashes:\n%s", content)
	}
	if !strings.Contains(content, "hello world") {
		t.Errorf("export missing transcript content")
	}
	if !strings.Contains(content, `"sk":"PROFILE"`) {
		t.Errorf("export missing profile row")
	}

	var doc struct {
		Part  int              `json:"part"`
		Of    int              `json:"of"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("export is not valid JSON: %v", err)
	}
	if doc.Part != 1 || doc.Of != 1 || len(doc.Items) != 3 {
		t.Errorf("envelope = part %d of %d with %d items, want 1/1/3", doc.Part, doc.Of, len(doc.Items))
	}
}

func TestMarshalExportPartsChunks(t *testing.T) {
	// ~40 items x ~50KB ≈ 2MB -> must split into 3 parts under the 900KB
	// target, preserving every item exactly once.
	big := strings.Repeat("x", 50<<10)
	items := make([]map[string]any, 40)
	for i := range items {
		items[i] = map[string]any{"sk": "LOG#" + strings.Repeat("a", i%5), "text": big, "n": i}
	}
	parts, err := marshalExportParts("u1", time.Now().UTC(), items)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) < 2 {
		t.Fatalf("parts = %d, want >= 2", len(parts))
	}
	total := 0
	for i, p := range parts {
		if len(p) > 1<<20 {
			t.Errorf("part %d is %d bytes — exceeds the deliverable cap", i+1, len(p))
		}
		var doc struct {
			Part  int              `json:"part"`
			Of    int              `json:"of"`
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(p, &doc); err != nil {
			t.Fatalf("part %d invalid JSON: %v", i+1, err)
		}
		if doc.Part != i+1 || doc.Of != len(parts) {
			t.Errorf("part %d envelope = %d/%d", i+1, doc.Part, doc.Of)
		}
		total += len(doc.Items)
	}
	if total != len(items) {
		t.Errorf("items across parts = %d, want %d", total, len(items))
	}
}

func TestAccountExportEmptyPartitionStillExports(t *testing.T) {
	app, _, _, fakeS3 := newAccountApp(t)
	// No profile seeded at all: the export is still a valid (empty) document.
	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/account/export", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%v)", resp.StatusCode, body)
	}
	if len(fakeS3.objects) != 1 {
		t.Fatalf("s3 objects = %d, want 1", len(fakeS3.objects))
	}
}
