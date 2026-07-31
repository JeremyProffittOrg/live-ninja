package codeupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeDDB is an in-memory table keyed by pk|sk, enough for the two rows this
// package writes.
type fakeDDB struct {
	items   map[string]map[string]ddbtypes.AttributeValue
	putErr  error
	failPut string // fail PutItem when the sk contains this
}

func newFakeDDB() *fakeDDB {
	return &fakeDDB{items: map[string]map[string]ddbtypes.AttributeValue{}}
}

func ddbKey(item map[string]ddbtypes.AttributeValue) string {
	pk, _ := item["pk"].(*ddbtypes.AttributeValueMemberS)
	sk, _ := item["sk"].(*ddbtypes.AttributeValueMemberS)
	if pk == nil || sk == nil {
		return ""
	}
	return pk.Value + "|" + sk.Value
}

func (f *fakeDDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if f.putErr != nil {
		return nil, f.putErr
	}
	key := ddbKey(in.Item)
	if f.failPut != "" && strings.Contains(key, f.failPut) {
		return nil, errors.New("dynamodb unavailable")
	}
	// The create-only guard on PutToken is the thing that stops a redelivery from
	// revoking a running session's credential, so the fake has to model it — a
	// fake that ignores ConditionExpression makes the test that checks it
	// meaningless.
	if in.ConditionExpression != nil &&
		strings.Contains(*in.ConditionExpression, "attribute_not_exists(pk)") {
		if _, exists := f.items[key]; exists {
			return nil, &ddbtypes.ConditionalCheckFailedException{}
		}
	}
	f.items[key] = in.Item
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	item := f.items[ddbKey(in.Key)]
	return &dynamodb.GetItemOutput{Item: item}, nil
}

func (f *fakeDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	key := ddbKey(in.Key)
	item, ok := f.items[key]
	if !ok {
		if in.ConditionExpression != nil {
			return nil, &ddbtypes.ConditionalCheckFailedException{}
		}
		item = map[string]ddbtypes.AttributeValue{}
		for k, v := range in.Key {
			item[k] = v
		}
	}
	// Only the two expressions this package issues are modelled.
	if in.ConditionExpression != nil && strings.Contains(*in.ConditionExpression, "postCount <") {
		max := 0
		if v, ok := in.ExpressionAttributeValues[":max"].(*ddbtypes.AttributeValueMemberN); ok {
			max = atoiOrZero(v.Value)
		}
		cur := itemInt(item, "postCount")
		if cur >= max {
			return nil, &ddbtypes.ConditionalCheckFailedException{}
		}
		item["postCount"] = &ddbtypes.AttributeValueMemberN{Value: itoa(cur + 1)}
		f.items[key] = item
		return &dynamodb.UpdateItemOutput{Attributes: item}, nil
	}
	// SetStatus: apply every :value under its matching attribute name.
	for placeholder, av := range in.ExpressionAttributeValues {
		name := strings.TrimPrefix(placeholder, ":")
		switch name {
		case "s":
			name = "status"
		case "e":
			name = "error"
		case "u":
			name = "updatedAt"
		case "rw":
			name = "rewritten"
		}
		item[name] = av
	}
	f.items[key] = item
	return &dynamodb.UpdateItemOutput{Attributes: item}, nil
}

func (f *fakeDDB) Query(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	return &dynamodb.QueryOutput{}, nil
}

func atoiOrZero(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// fakeSQS captures enqueued emails.
type fakeSQS struct {
	messages []emailMessage
	err      error
}

func (f *fakeSQS) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	var m emailMessage
	_ = json.Unmarshal([]byte(aws.ToString(in.MessageBody)), &m)
	f.messages = append(f.messages, m)
	return &sqs.SendMessageOutput{}, nil
}

// fakeGhost replays scripted proxy responses per resource.
type fakeGhost struct {
	launchStatus  int
	launchBody    string
	prepStatus    int
	prepBody      string
	statusReplies []string // consumed in order by /schedule/preprocess-status
	statusIdx     int
	launchBodies  []string // captured request bodies for /schedule
	prepBodies    []string // captured request bodies for /schedule/preprocess
}

func (f *fakeGhost) Invoke(_ context.Context, in *awslambda.InvokeInput, _ ...func(*awslambda.Options)) (*awslambda.InvokeOutput, error) {
	var ev struct {
		Resource string `json:"resource"`
		Body     string `json:"body"`
	}
	_ = json.Unmarshal(in.Payload, &ev)

	reply := func(status int, body string) (*awslambda.InvokeOutput, error) {
		p, _ := json.Marshal(map[string]any{"statusCode": status, "body": body})
		return &awslambda.InvokeOutput{Payload: p}, nil
	}

	switch ev.Resource {
	case "/schedule":
		f.launchBodies = append(f.launchBodies, ev.Body)
		return reply(f.launchStatus, f.launchBody)
	case "/schedule/preprocess":
		f.prepBodies = append(f.prepBodies, ev.Body)
		return reply(f.prepStatus, f.prepBody)
	case "/schedule/preprocess-status":
		if f.statusIdx < len(f.statusReplies) {
			body := f.statusReplies[f.statusIdx]
			f.statusIdx++
			return reply(200, body)
		}
		return reply(200, `{"status":"PENDING"}`)
	}
	return reply(404, `{"error":"no"}`)
}

const okLaunchBody = `{"event_id":"voice-update-o-live-ninja","run":{"run_id":"run-1","node":"officepc","status":"RUNNING"}}`

func newTestDispatcher(g *fakeGhost, db *fakeDDB, q *fakeSQS) *Dispatcher {
	now := time.Unix(1_800_000_000, 0)
	return &Dispatcher{
		Ghost: ghost.New(ghost.Config{API: g, Function: "ghost-cli-command",
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}),
		Store:             NewStore(db, "live-ninja", func() time.Time { return now }),
		SQS:               q,
		EmailQueueURL:     "https://sqs/email",
		OwnerEmail:        "owner@example.com",
		ProgressURL:       "https://live.jeremy.ninja/v1/code-update/progress",
		OutputFile:        DefaultOutputFile,
		PreprocessTimeout: 30 * time.Second,
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		// No real waiting, and a clock that advances so a poll loop terminates.
		Now:   func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error { now = now.Add(preprocessPollInterval); return nil },
	}
}

func testRequest() Request {
	return Request{
		Version:      QueueMessageVersion,
		RequestID:    "019fa317-aaeb-715d-b80b-c2f87e129228",
		UserID:       "user-1",
		Repo:         "o/live-ninja",
		Instructions: "tighten the retry logic on the Bedrock client",
		Node:         "officepc",
		CLI:          "claude",
		Preprocess:   false,
		RequestedAt:  "2027-01-15T10:00:00Z",
	}
}

func launchedPrompt(t *testing.T, g *fakeGhost) string {
	t.Helper()
	if len(g.launchBodies) == 0 {
		t.Fatal("no launch was issued")
	}
	var req ghost.LaunchRequest
	if err := json.Unmarshal([]byte(g.launchBodies[len(g.launchBodies)-1]), &req); err != nil {
		t.Fatalf("unmarshal launch body: %v", err)
	}
	return req.Prompt
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestDispatchLaunchesAndEmails(t *testing.T) {
	g := &fakeGhost{launchStatus: 202, launchBody: okLaunchBody}
	db, q := newFakeDDB(), &fakeSQS{}
	d := newTestDispatcher(g, db, q)

	if err := d.Dispatch(context.Background(), testRequest()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	var sent ghost.LaunchRequest
	if err := json.Unmarshal([]byte(g.launchBodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if !sent.RunNow {
		t.Error("launch did not set run_now")
	}
	if sent.OutputFile != DefaultOutputFile {
		t.Errorf("output_file = %q, want %q — without it the run never completes and no summary email fires",
			sent.OutputFile, DefaultOutputFile)
	}
	if sent.EventID != EventID("o/live-ninja") {
		t.Errorf("event_id = %q, want the stable %q", sent.EventID, EventID("o/live-ninja"))
	}
	if sent.CLI != "claude" || sent.Node != "officepc" {
		t.Errorf("cli/node = %q/%q", sent.CLI, sent.Node)
	}

	if len(q.messages) != 1 {
		t.Fatalf("sent %d emails, want 1", len(q.messages))
	}
	if q.messages[0].To != "owner@example.com" {
		t.Errorf("recipient = %q, want the configured owner", q.messages[0].To)
	}
	// The confirmation email is the record of what a voice command authorized.
	if !strings.Contains(q.messages[0].Text, sent.Prompt) {
		t.Error("confirmation email does not carry the exact prompt that was sent")
	}
	if !strings.Contains(q.messages[0].Text, "run-1") {
		t.Error("confirmation email does not name the run id")
	}
}

// The token row must exist before the prompt embeds the token; otherwise the
// agent gets a credential that can only 401.
func TestDispatchWritesTokenRowBeforeLaunching(t *testing.T) {
	g := &fakeGhost{launchStatus: 202, launchBody: okLaunchBody}
	db, q := newFakeDDB(), &fakeSQS{}
	d := newTestDispatcher(g, db, q)
	req := testRequest()

	if err := d.Dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	row, err := d.Store.GetToken(context.Background(), req.RequestID)
	if err != nil {
		t.Fatalf("token row missing: %v", err)
	}
	if row.TokenHash == "" {
		t.Fatal("token row has no hash")
	}

	// The token in the prompt must verify against the stored hash.
	prompt := launchedPrompt(t, g)
	idx := strings.Index(prompt, TokenPrefix)
	if idx < 0 {
		t.Fatal("prompt carries no run token")
	}
	// The token sits inside a quoted curl header, so strip the closing quote.
	token := strings.Trim(strings.Fields(prompt[idx:])[0], `"'`)
	_, secret, perr := ParseToken(token)
	if perr != nil {
		t.Fatalf("prompt token does not parse: %v", perr)
	}
	if err := VerifySecret(secret, row.TokenHash); err != nil {
		t.Errorf("the token in the prompt does not match the stored row: %v", err)
	}
}

// A DynamoDB failure is transient: the message must go back to the queue rather
// than be lost, and nothing may be launched with a token that has no row.
func TestDispatchTokenRowFailureIsTransientAndLaunchesNothing(t *testing.T) {
	g := &fakeGhost{launchStatus: 202, launchBody: okLaunchBody}
	db, q := newFakeDDB(), &fakeSQS{}
	db.failPut = "TOKEN"
	d := newTestDispatcher(g, db, q)

	if err := d.Dispatch(context.Background(), testRequest()); err == nil {
		t.Fatal("Dispatch returned nil; a store failure must be reported as transient")
	}
	if len(g.launchBodies) != 0 {
		t.Error("a launch was issued despite the token row failing to write")
	}
}

func TestDispatchUsesOpusRewriteWhenRequested(t *testing.T) {
	g := &fakeGhost{
		launchStatus:  202,
		launchBody:    okLaunchBody,
		prepStatus:    202,
		prepBody:      `{"job_id":"job-1","status":"PENDING"}`,
		statusReplies: []string{`{"status":"PENDING"}`, `{"status":"DONE","prompt":"REWRITTEN BRIEF"}`},
	}
	db, q := newFakeDDB(), &fakeSQS{}
	d := newTestDispatcher(g, db, q)
	req := testRequest()
	req.Preprocess = true

	if err := d.Dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	prompt := launchedPrompt(t, g)
	if !strings.Contains(prompt, "REWRITTEN BRIEF") {
		t.Errorf("the rewrite was not used:\n%s", prompt)
	}
	if !strings.Contains(q.messages[0].Text, "rewritten by Opus") {
		t.Error("confirmation email does not report that the rewrite was used")
	}
}

// The rewrite is a nicety; the update is the point. A failure launches the
// owner's own words and SAYS SO — never silently.
func TestPreprocessFailureLaunchesOriginalAndReportsIt(t *testing.T) {
	cases := map[string]*fakeGhost{
		"start rejected": {launchStatus: 202, launchBody: okLaunchBody,
			prepStatus: 429, prepBody: `{"error":"quota"}`},
		"job failed": {launchStatus: 202, launchBody: okLaunchBody,
			prepStatus: 202, prepBody: `{"job_id":"j","status":"PENDING"}`,
			statusReplies: []string{`{"status":"FAILED","error":"boom"}`}},
		"empty prompt": {launchStatus: 202, launchBody: okLaunchBody,
			prepStatus: 202, prepBody: `{"job_id":"j","status":"PENDING"}`,
			statusReplies: []string{`{"status":"DONE","prompt":"   "}`}},
		"never finishes": {launchStatus: 202, launchBody: okLaunchBody,
			prepStatus: 202, prepBody: `{"job_id":"j","status":"PENDING"}`},
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			db, q := newFakeDDB(), &fakeSQS{}
			d := newTestDispatcher(g, db, q)
			req := testRequest()
			req.Preprocess = true

			if err := d.Dispatch(context.Background(), req); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if len(g.launchBodies) != 1 {
				t.Fatalf("issued %d launches, want 1 — the update must still run", len(g.launchBodies))
			}
			if !strings.Contains(launchedPrompt(t, g), "tighten the retry logic") {
				t.Error("the owner's original instructions were lost")
			}
			if len(q.messages) != 1 {
				t.Fatalf("sent %d emails, want 1", len(q.messages))
			}
			if !strings.Contains(q.messages[0].Text, "original wording") {
				t.Errorf("the email does not tell the owner their original wording was used:\n%s",
					q.messages[0].Text)
			}
		})
	}
}

// Only the owner's instructions may reach Bedrock — never the run token, never
// the directives. This is the assembly-order property, asserted end to end.
func TestPreprocessNeverReceivesTheToken(t *testing.T) {
	g := &fakeGhost{
		launchStatus: 202, launchBody: okLaunchBody,
		prepStatus: 202, prepBody: `{"job_id":"j","status":"PENDING"}`,
		statusReplies: []string{`{"status":"DONE","prompt":"REWRITTEN"}`},
	}
	db, q := newFakeDDB(), &fakeSQS{}
	d := newTestDispatcher(g, db, q)
	req := testRequest()
	req.Preprocess = true

	if err := d.Dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(g.prepBodies) == 0 {
		t.Fatal("no preprocess request was made")
	}
	var pr ghost.PreprocessRequest
	if err := json.Unmarshal([]byte(g.prepBodies[0]), &pr); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{TokenPrefix, "code-update/progress", "Authorization: Bearer", "DO NOT PUSH"} {
		if strings.Contains(pr.Prompt, forbidden) {
			t.Errorf("the text sent to Opus contains %q:\n%s", forbidden, pr.Prompt)
		}
	}
	if !strings.Contains(pr.Prompt, "tighten the retry logic") {
		t.Error("the text sent to Opus lost the owner's instructions")
	}
}

// A rewrite that comes back already carrying ghost-cli's output directive (which
// its preprocessor always appends) must not produce two of them, and must not
// strand the operating rules after the first.
func TestRewriteCarryingTheDirectiveIsCanonicalized(t *testing.T) {
	directive := OutputDirective(DefaultOutputFile)
	body, _ := json.Marshal(map[string]string{
		"status": "DONE",
		"prompt": "REWRITTEN BRIEF\n\n" + directive,
	})
	g := &fakeGhost{
		launchStatus: 202, launchBody: okLaunchBody,
		prepStatus: 202, prepBody: `{"job_id":"j","status":"PENDING"}`,
		statusReplies: []string{string(body)},
	}
	db, q := newFakeDDB(), &fakeSQS{}
	d := newTestDispatcher(g, db, q)
	req := testRequest()
	req.Preprocess = true

	if err := d.Dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	prompt := launchedPrompt(t, g)
	if n := strings.Count(prompt, directive); n != 1 {
		t.Errorf("directive appears %d times, want 1:\n%s", n, prompt)
	}
	if !strings.HasSuffix(prompt, directive) {
		t.Errorf("directive is not the final paragraph:\n%s", prompt)
	}
}

// A denied principal will be denied again on redelivery, so it is permanent:
// email the owner and drop the message.
func TestLaunchDeniedIsPermanentAndReported(t *testing.T) {
	g := &fakeGhost{launchStatus: 403, launchBody: `{"error":"forbidden"}`}
	db, q := newFakeDDB(), &fakeSQS{}
	d := newTestDispatcher(g, db, q)

	if err := d.Dispatch(context.Background(), testRequest()); err != nil {
		t.Fatalf("a denial must not be retried: %v", err)
	}
	if len(q.messages) != 1 {
		t.Fatalf("sent %d emails, want 1", len(q.messages))
	}
	if !strings.Contains(q.messages[0].Subject, "did NOT start") {
		t.Errorf("subject = %q", q.messages[0].Subject)
	}
	if !strings.Contains(q.messages[0].Text, "allowlist") {
		t.Errorf("the failure email does not name the fix:\n%s", q.messages[0].Text)
	}
	rec, err := d.Store.Get(context.Background(), "user-1", testRequest().RequestID)
	if err == nil && rec.Status != StatusFailed {
		t.Errorf("record status = %q, want %q", rec.Status, StatusFailed)
	}
}

// A 409 means something is already running on that repo — a real answer, and
// permanent for this message.
func TestLaunchConflictIsPermanentAndExplained(t *testing.T) {
	g := &fakeGhost{launchStatus: 409, launchBody: `{"error":"a run is already in progress"}`}
	db, q := newFakeDDB(), &fakeSQS{}
	d := newTestDispatcher(g, db, q)

	if err := d.Dispatch(context.Background(), testRequest()); err != nil {
		t.Fatalf("a conflict must not be retried: %v", err)
	}
	if !strings.Contains(q.messages[0].Text, "already running") {
		t.Errorf("the failure email does not explain the conflict:\n%s", q.messages[0].Text)
	}
}

// A 5xx is worth one more attempt.
func TestLaunchUpstreamErrorIsTransient(t *testing.T) {
	g := &fakeGhost{launchStatus: 500, launchBody: `{"error":"boom"}`}
	db, q := newFakeDDB(), &fakeSQS{}
	d := newTestDispatcher(g, db, q)

	if err := d.Dispatch(context.Background(), testRequest()); err == nil {
		t.Fatal("a 5xx must be reported as transient")
	}
}

// A message from a future deploy is dropped, not looped: a version we cannot
// read will never become readable.
func TestUnknownMessageVersionIsDropped(t *testing.T) {
	g := &fakeGhost{launchStatus: 202, launchBody: okLaunchBody}
	db, q := newFakeDDB(), &fakeSQS{}
	d := newTestDispatcher(g, db, q)
	req := testRequest()
	req.Version = 99

	if err := d.Dispatch(context.Background(), req); err != nil {
		t.Fatalf("an unreadable version must not be retried: %v", err)
	}
	if len(g.launchBodies) != 0 {
		t.Error("a launch was issued for an unknown message version")
	}
}

// Missing configuration is transient — a redeploy fixes it, and the owner's
// request should survive to be retried rather than vanish.
func TestUnconfiguredDispatcherIsTransient(t *testing.T) {
	d := &Dispatcher{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := d.Dispatch(context.Background(), testRequest()); err == nil {
		t.Fatal("an unconfigured dispatcher must report a transient failure")
	}
}

// The deploy gate has to survive the whole pipeline, not just prompt assembly.
func TestDeployGateReachesTheLaunchedPrompt(t *testing.T) {
	for _, deploy := range []bool{false, true} {
		g := &fakeGhost{launchStatus: 202, launchBody: okLaunchBody}
		db, q := newFakeDDB(), &fakeSQS{}
		d := newTestDispatcher(g, db, q)
		req := testRequest()
		req.Deploy = deploy

		if err := d.Dispatch(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		prompt := strings.ToLower(launchedPrompt(t, g))
		forbids := strings.Contains(prompt, "do not push")
		if deploy && forbids {
			t.Error("an authorized deploy still forbade pushing")
		}
		if !deploy && !forbids {
			t.Error("an unauthorized run was not told to avoid pushing")
		}
		if !strings.Contains(q.messages[0].Text, "Deploy:") {
			t.Error("the confirmation email does not state the deploy decision")
		}
	}
}

// Losing the notification must not undo a launch that already happened.
func TestEmailFailureDoesNotFailTheDispatch(t *testing.T) {
	g := &fakeGhost{launchStatus: 202, launchBody: okLaunchBody}
	db := newFakeDDB()
	q := &fakeSQS{err: errors.New("sqs down")}
	d := newTestDispatcher(g, db, q)

	if err := d.Dispatch(context.Background(), testRequest()); err != nil {
		t.Fatalf("a failed notification must not fail the dispatch: %v", err)
	}
	if len(g.launchBodies) != 1 {
		t.Error("the launch did not happen")
	}
}

// A duplicate SQS delivery must NOT re-mint. Overwriting the token row silently
// revokes the credential the still-running session is holding (every later
// progress post 401s) and hands it a second full quota of emails.
func TestDuplicateDeliveryDoesNotReMintOrRelaunch(t *testing.T) {
	g := &fakeGhost{launchStatus: 202, launchBody: okLaunchBody}
	db, q := newFakeDDB(), &fakeSQS{}
	d := newTestDispatcher(g, db, q)
	req := testRequest()

	if err := d.Dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	first, err := d.Store.GetToken(context.Background(), req.RequestID)
	if err != nil {
		t.Fatal(err)
	}

	// Same message, delivered again.
	if err := d.Dispatch(context.Background(), req); err != nil {
		t.Fatalf("a redelivery must not be reported as transient: %v", err)
	}
	second, err := d.Store.GetToken(context.Background(), req.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if second.TokenHash != first.TokenHash {
		t.Error("the redelivery re-minted the token, revoking the running session's credential")
	}
	if len(g.launchBodies) != 1 {
		t.Errorf("issued %d launches, want 1 — the redelivery started a second session",
			len(g.launchBodies))
	}
	if len(q.messages) != 1 {
		t.Errorf("sent %d emails, want 1", len(q.messages))
	}
}

// A rewrite that is nothing but ghost-cli's own output directive carries no
// task. Launching it would start a session with rules and nothing to do.
func TestEmptyRewriteFallsBackToTheOwnersWords(t *testing.T) {
	directive := OutputDirective(DefaultOutputFile)
	for _, prompt := range []string{"", "   ", directive, "\n\n" + directive + "\n"} {
		body, _ := json.Marshal(map[string]string{"status": "DONE", "prompt": prompt})
		g := &fakeGhost{
			launchStatus: 202, launchBody: okLaunchBody,
			prepStatus: 202, prepBody: `{"job_id":"j","status":"PENDING"}`,
			statusReplies: []string{string(body)},
		}
		db, q := newFakeDDB(), &fakeSQS{}
		d := newTestDispatcher(g, db, q)
		r := testRequest()
		r.Preprocess = true

		if err := d.Dispatch(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(launchedPrompt(t, g), "tighten the retry logic") {
			t.Errorf("an empty rewrite %q was launched instead of the owner's words", prompt)
		}
		if !strings.Contains(q.messages[0].Text, "original wording") {
			t.Error("the email does not report the fallback")
		}
	}
}
