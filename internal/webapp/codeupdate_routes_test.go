package webapp

// HTTP-surface tests for the public code-update progress endpoint.
//
// This is the only pre-auth, credential-bearing route in the feature, so the
// tests are weighted toward the things that would make it a hole: telling
// failure classes apart, letting the body steer the recipient, and slipping past
// the post cap.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/codeupdate"
)

// progressDDB is a minimal in-memory table supporting the token row's GetItem
// and its conditional post-count claim.
type progressDDB struct {
	mu     sync.Mutex
	items  map[string]map[string]ddbtypes.AttributeValue
	getErr error
}

func newProgressDDB() *progressDDB {
	return &progressDDB{items: map[string]map[string]ddbtypes.AttributeValue{}}
}

func progressKey(m map[string]ddbtypes.AttributeValue) string {
	pk, _ := m["pk"].(*ddbtypes.AttributeValueMemberS)
	sk, _ := m["sk"].(*ddbtypes.AttributeValueMemberS)
	if pk == nil || sk == nil {
		return ""
	}
	return pk.Value + "|" + sk.Value
}

func (f *progressDDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[progressKey(in.Item)] = in.Item
	return &dynamodb.PutItemOutput{}, nil
}

func (f *progressDDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &dynamodb.GetItemOutput{Item: f.items[progressKey(in.Key)]}, nil
}

func (f *progressDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[progressKey(in.Key)]
	if !ok {
		return nil, &ddbtypes.ConditionalCheckFailedException{}
	}
	maxAV, _ := in.ExpressionAttributeValues[":max"].(*ddbtypes.AttributeValueMemberN)
	countAV, _ := item["postCount"].(*ddbtypes.AttributeValueMemberN)
	cur, max := 0, 0
	if countAV != nil {
		cur = mustAtoi(countAV.Value)
	}
	if maxAV != nil {
		max = mustAtoi(maxAV.Value)
	}
	if cur >= max {
		return nil, &ddbtypes.ConditionalCheckFailedException{}
	}
	item["postCount"] = &ddbtypes.AttributeValueMemberN{Value: mustItoa(cur + 1)}
	f.items[progressKey(in.Key)] = item
	return &dynamodb.UpdateItemOutput{Attributes: item}, nil
}

func (f *progressDDB) Query(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	return &dynamodb.QueryOutput{}, nil
}

func mustAtoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func mustItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

type progressSQS struct {
	mu       sync.Mutex
	messages []emailMessage
}

func (f *progressSQS) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m emailMessage
	_ = json.Unmarshal([]byte(aws.ToString(in.MessageBody)), &m)
	f.messages = append(f.messages, m)
	return &sqs.SendMessageOutput{}, nil
}

func (f *progressSQS) sent() []emailMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]emailMessage, len(f.messages))
	copy(out, f.messages)
	return out
}

// progressHarness builds the app with a seeded token row and returns the app,
// the plaintext token, and the email sink.
func progressHarness(t *testing.T) (*fiber.App, string, *progressSQS, *progressDDB) {
	t.Helper()
	const requestID = "019fa317-aaeb-715d-b80b-c2f87e129228"

	token, hash, err := codeupdate.NewToken(requestID)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	db := newProgressDDB()
	q := &progressSQS{}
	now := time.Unix(1_800_000_000, 0)
	store := codeupdate.NewStore(db, "live-ninja", func() time.Time { return now })

	if err := store.PutToken(context.Background(), codeupdate.TokenRow{
		RequestID: requestID,
		UserID:    "user-1",
		Repo:      "o/live-ninja",
		TokenHash: hash,
	}); err != nil {
		t.Fatalf("PutToken: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := &Deps{
		Log:        log,
		CodeUpdate: store,
		CodeUpdateDispatcher: &codeupdate.Dispatcher{
			SQS:           q,
			EmailQueueURL: "https://sqs/email",
			OwnerEmail:    "owner@example.com",
			Log:           log,
		},
	}
	app := fiber.New()
	RegisterCodeUpdateRoutes(app, deps)
	return app, token, q, db
}

func postProgress(t *testing.T, app *fiber.App, auth, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/code-update/progress", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}

func TestProgressAcceptsAValidReport(t *testing.T) {
	app, token, q, _ := progressHarness(t)
	resp := postProgress(t, app, "Bearer "+token,
		`{"status":"working","summary":"read the retry path, patching it now"}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	sent := q.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d emails, want 1", len(sent))
	}
	if sent[0].To != "owner@example.com" {
		t.Errorf("recipient = %q, want the configured owner", sent[0].To)
	}
	if !strings.Contains(sent[0].Subject, "o/live-ninja") {
		t.Errorf("subject = %q, want it to name the repo", sent[0].Subject)
	}
	if !strings.Contains(sent[0].Text, "patching it now") {
		t.Errorf("email lost the summary: %q", sent[0].Text)
	}
}

// A bare token (no "Bearer ") is accepted: an agent transcribing a curl from a
// prompt may drop the prefix, and the shape check still bounds it.
func TestProgressAcceptsABareToken(t *testing.T) {
	app, token, _, _ := progressHarness(t)
	resp := postProgress(t, app, token, `{"status":"done","summary":"finished"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// Every credential-class failure must be an INDISTINGUISHABLE 401, or the
// endpoint becomes an oracle for which run ids exist.
func TestProgressCredentialFailuresAreIndistinguishable(t *testing.T) {
	app, token, q, _ := progressHarness(t)
	_, secret, _ := codeupdate.ParseToken(token)

	cases := map[string]string{
		"no header":          "",
		"empty bearer":       "Bearer ",
		"wrong secret":       "Bearer cu_019fa317-aaeb-715d-b80b-c2f87e129228_" + strings.Repeat("0", 64),
		"unknown request":    "Bearer cu_019fa317-0000-7000-8000-000000000000_" + secret,
		"a ghost api key":    "Bearer gk_" + strings.Repeat("a", 32) + "_" + secret,
		"a jwt":              "Bearer eyJhbGciOiJSUzI1NiJ9.body.sig",
		"garbage":            "Bearer not-a-token",
		"session cookie-ish": "Bearer sess_abcdef",
	}
	var bodies []string
	for name, auth := range cases {
		t.Run(name, func(t *testing.T) {
			resp := postProgress(t, app, auth, `{"status":"working","summary":"x"}`)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			b, _ := io.ReadAll(resp.Body)
			bodies = append(bodies, string(b))
		})
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Errorf("401 bodies differ, leaking which failure occurred:\n%s\nvs\n%s", bodies[0], bodies[i])
		}
	}
	if n := len(q.sent()); n != 0 {
		t.Errorf("sent %d emails for rejected credentials, want 0", n)
	}
}

// A store failure must also read as 401 rather than 500 — a 500 would tell an
// attacker their token shape reached the lookup.
func TestProgressStoreFailureIsUnauthorized(t *testing.T) {
	app, token, _, db := progressHarness(t)
	db.getErr = errors.New("dynamodb down")
	resp := postProgress(t, app, "Bearer "+token, `{"status":"working","summary":"x"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestProgressRejectsBadBodies(t *testing.T) {
	app, token, q, _ := progressHarness(t)
	for name, body := range map[string]string{
		"not json":       `{`,
		"unknown status": `{"status":"vibing","summary":"x"}`,
		"empty status":   `{"summary":"x"}`,
		"empty summary":  `{"status":"working","summary":"   "}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := postProgress(t, app, "Bearer "+token, body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
	if n := len(q.sent()); n != 0 {
		t.Errorf("sent %d emails for invalid bodies, want 0", n)
	}
}

// A verbose agent should not lose its report — truncate, do not reject.
func TestProgressTruncatesRatherThanRejects(t *testing.T) {
	app, token, q, _ := progressHarness(t)
	long := strings.Repeat("a", maxProgressSummary+5000)
	body, _ := json.Marshal(map[string]string{"status": "working", "summary": long})

	resp := postProgress(t, app, "Bearer "+token, string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	sent := q.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d emails, want 1", len(sent))
	}
	if strings.Count(sent[0].Text, "a") > maxProgressSummary+100 {
		t.Error("summary was not truncated")
	}
}

// The cap is what keeps a looping agent from flooding the inbox.
func TestProgressEnforcesThePostCap(t *testing.T) {
	app, token, q, _ := progressHarness(t)
	for i := range codeupdate.MaxProgressPosts {
		resp := postProgress(t, app, "Bearer "+token, `{"status":"working","summary":"x"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("post %d: status = %d, want 200", i+1, resp.StatusCode)
		}
	}
	resp := postProgress(t, app, "Bearer "+token, `{"status":"working","summary":"x"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("post %d: status = %d, want 429", codeupdate.MaxProgressPosts+1, resp.StatusCode)
	}
	if n := len(q.sent()); n != codeupdate.MaxProgressPosts {
		t.Errorf("sent %d emails, want exactly the cap %d", n, codeupdate.MaxProgressPosts)
	}
}

// Nothing in the request body may steer where mail goes: a leaked token buys the
// ability to email the OWNER and nothing else.
func TestProgressRecipientIsNeverCallerControlled(t *testing.T) {
	app, token, q, _ := progressHarness(t)
	resp := postProgress(t, app, "Bearer "+token, `{
		"status":"working","summary":"x",
		"to":"attacker@example.com","recipient":"attacker@example.com","email":"attacker@example.com"
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, m := range q.sent() {
		if m.To != "owner@example.com" {
			t.Errorf("recipient = %q, want owner@example.com", m.To)
		}
	}
}

// Unconfigured must be a 503, not a 401 — an operator debugging a deploy needs
// to tell "not wired up" from "bad token".
func TestProgressUnconfiguredIs503(t *testing.T) {
	app := fiber.New()
	RegisterCodeUpdateRoutes(app, &Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	req := httptest.NewRequest(http.MethodPost, "/v1/code-update/progress",
		bytes.NewReader([]byte(`{"status":"working","summary":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
