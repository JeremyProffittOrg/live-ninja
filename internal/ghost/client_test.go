package ghost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

// fakeInvoke records every invocation and replays a queued response.
type fakeInvoke struct {
	events    []invokeEvent
	responses []proxyResponse
	fnErrors  []string
	err       error
	calls     int
}

func (f *fakeInvoke) Invoke(_ context.Context, in *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	f.calls++
	var ev invokeEvent
	_ = json.Unmarshal(in.Payload, &ev)
	f.events = append(f.events, ev)
	if f.err != nil {
		return nil, f.err
	}
	idx := len(f.events) - 1
	out := &lambda.InvokeOutput{}
	if idx < len(f.fnErrors) && f.fnErrors[idx] != "" {
		out.FunctionError = aws.String(f.fnErrors[idx])
		out.Payload = []byte(`{"errorMessage":"boom"}`)
		return out, nil
	}
	resp := proxyResponse{StatusCode: 200, Body: "{}"}
	if idx < len(f.responses) {
		resp = f.responses[idx]
	}
	out.Payload, _ = json.Marshal(resp)
	return out, nil
}

func testClient(f *fakeInvoke) *Client {
	return New(Config{
		API:      f,
		Function: "ghost-cli-command",
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return time.Unix(1_800_000_000, 0) },
	})
}

func ok(body string) proxyResponse { return proxyResponse{StatusCode: 200, Body: body} }

// The task discriminator is a CROSS-REPO CONTRACT with ghost-cli's
// internalInvokeTask (lambda/command/internal_invoke.go). It is pinned to a
// LITERAL here rather than compared against our own constant, because the
// failure it guards is silent in the worst way: a mismatch makes ghost-cli fall
// through to its API-Gateway path, where the event has no authorizer context, so
// every call 401s and the symptom reads as "live-ninja is not on the allowlist"
// — sending an operator to re-seed an allowlist that was never the problem.
func TestInternalInvokeTaskMatchesGhostCLI(t *testing.T) {
	const ghostCLIValue = "internal_api" // ghost-cli: internalInvokeTask
	if internalInvokeTask != ghostCLIValue {
		t.Fatalf("task discriminator = %q, want %q (ghost-cli lambda/command/internal_invoke.go)",
			internalInvokeTask, ghostCLIValue)
	}
	// And it must ride under the exact JSON key ghost-cli decodes.
	raw, _ := json.Marshal(invokeEvent{Task: internalInvokeTask, Resource: "/schedule", Method: "POST"})
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["ghost_task"] != ghostCLIValue {
		t.Errorf("discriminator rides under %v, want the ghost_task key", fields)
	}
}

// Every resource this client asks for must be on ghost-cli's closed
// internal-invoke allowlist. A route added here but not there fails at the far
// end with an opaque Lambda function error.
func TestOnlyAllowlistedResourcesAreUsed(t *testing.T) {
	// ghost-cli: internalInvokeResources (lambda/command/internal_invoke.go).
	// The METHOD is part of the key there — /schedule's handler dispatches on the
	// verb, so PUT and DELETE are deliberately NOT reachable.
	allowed := map[string]bool{
		"GET /launch/repos":               true,
		"GET /nodes":                      true,
		"GET /schedule":                   true,
		"POST /schedule":                  true,
		"POST /schedule/preprocess":       true,
		"GET /schedule/preprocess-status": true,
	}
	f := &fakeInvoke{responses: []proxyResponse{
		ok(`{"repos":[]}`),
		{StatusCode: 202, Body: `{"job_id":"j"}`},
		ok(`{"status":"done","prompt":"x"}`),
		{StatusCode: 202, Body: `{"event_id":"e","run":{"run_id":"r"}}`},
		ok(`{"events":[]}`),
		ok(`{"nodes":[]}`),
	}}
	c := testClient(f)
	ctx := context.Background()
	_, _ = c.ListRepos(ctx)
	_, _ = c.Preprocess(ctx, PreprocessRequest{}, "")
	_, _ = c.PreprocessStatus(ctx, "j", "")
	_, _ = c.Launch(ctx, LaunchRequest{}, "")
	_, _, _ = c.FindRun(ctx, "e", "r", "")
	_, _ = c.Nodes(ctx, "")

	if len(f.events) != 6 {
		t.Fatalf("made %d calls, want 6 (one per client method)", len(f.events))
	}
	for _, ev := range f.events {
		if key := ev.Method + " " + ev.Resource; !allowed[key] {
			t.Errorf("%q is not on ghost-cli's internal-invoke allowlist", key)
		}
	}
}

// The client must never be able to name its own principal, its own role, or a
// credential scope: ghost-cli pins all three. This asserts the wire event
// carries nothing of the sort, so a bug here could not widen authority even if
// ghost-cli's pinning regressed.
func TestInvokeEventCarriesNoIdentity(t *testing.T) {
	f := &fakeInvoke{responses: []proxyResponse{ok(`{"repos":[]}`)}}
	if _, err := testClient(f).ListRepos(context.Background()); err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	raw, _ := json.Marshal(invokeEvent{Task: internalInvokeTask, Resource: "/launch/repos", Method: "GET"})
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"principal", "role", "scope", "nodes", "authorizer"} {
		if _, present := fields[forbidden]; present {
			t.Errorf("invoke event carries an identity field %q", forbidden)
		}
	}
	if f.events[0].Task != internalInvokeTask {
		t.Errorf("task = %q, want %q", f.events[0].Task, internalInvokeTask)
	}
}

func TestListReposParsesAndSplits(t *testing.T) {
	f := &fakeInvoke{responses: []proxyResponse{ok(
		`{"repos":[{"repo":"o/live-ninja"},{"repo":"o/ghost-cli"},{"repo":"malformed"},{"repo":""}]}`)}}
	repos, err := testClient(f).ListRepos(context.Background())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2 (malformed entries dropped): %+v", len(repos), repos)
	}
	if repos[0].Owner != "o" || repos[0].Name != "live-ninja" {
		t.Errorf("split wrong: %+v", repos[0])
	}
	// Upstream order (most-recently-pushed first) must survive — the matcher's
	// tie-break depends on it.
	if repos[0].Repo != "o/live-ninja" || repos[1].Repo != "o/ghost-cli" {
		t.Errorf("order not preserved: %+v", repos)
	}
}

func TestListReposCachesWithinTTL(t *testing.T) {
	f := &fakeInvoke{responses: []proxyResponse{ok(`{"repos":[{"repo":"o/a"}]}`), ok(`{"repos":[]}`)}}
	c := testClient(f)
	for range 3 {
		if _, err := c.ListRepos(context.Background()); err != nil {
			t.Fatalf("ListRepos: %v", err)
		}
	}
	if f.calls != 1 {
		t.Errorf("invoked %d times, want 1 (cached)", f.calls)
	}
}

func TestListReposRefreshesAfterTTL(t *testing.T) {
	f := &fakeInvoke{responses: []proxyResponse{ok(`{"repos":[{"repo":"o/a"}]}`), ok(`{"repos":[{"repo":"o/b"}]}`)}}
	now := time.Unix(1_800_000_000, 0)
	c := New(Config{API: f, Function: "fn", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return now }})

	if _, err := c.ListRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(repoCacheTTL + time.Second)
	repos, err := c.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Errorf("invoked %d times, want 2 after the TTL lapsed", f.calls)
	}
	if len(repos) != 1 || repos[0].Repo != "o/b" {
		t.Errorf("stale result after refresh: %+v", repos)
	}
}

// Status mapping is what the tool layer switches on to tell the owner something
// useful ("you're not on the allowlist" vs "a run is already going").
func TestStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{401, ErrNotAuthorized},
		{403, ErrNotAuthorized},
		{404, ErrNotFound},
		{409, ErrConflict},
		{429, ErrQuota},
		{503, ErrUnavailable},
		{500, ErrUpstream},
		{418, ErrUpstream},
	}
	for _, tc := range cases {
		f := &fakeInvoke{responses: []proxyResponse{{StatusCode: tc.status, Body: `{"error":"x"}`}}}
		_, err := testClient(f).ListRepos(context.Background())
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d → %v, want %v", tc.status, err, tc.want)
		}
	}
}

// A handled Lambda error means the far end rejected the event itself (a
// resource that is not on its allowlist, say). That is not a 200 with a body,
// and must not be parsed as one.
func TestFunctionErrorIsNotParsedAsAResponse(t *testing.T) {
	f := &fakeInvoke{fnErrors: []string{"Unhandled"}}
	_, err := testClient(f).ListRepos(context.Background())
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v, want ErrUpstream", err)
	}
}

func TestTransportErrorIsUpstream(t *testing.T) {
	f := &fakeInvoke{err: errors.New("network down")}
	_, err := testClient(f).ListRepos(context.Background())
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v, want ErrUpstream", err)
	}
}

// An unconfigured client must say so rather than look like an unreachable fleet.
func TestUnconfiguredClient(t *testing.T) {
	c := New(Config{API: &fakeInvoke{}, Function: "  "})
	if c.Ready() {
		t.Fatal("client with an empty function name reports Ready")
	}
	if _, err := c.ListRepos(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}

// The preprocess job statuses are a CROSS-REPO CONTRACT with ghost-cli, and the
// third one this codebase has (with the output directive and the invoke
// discriminator). Like those, it is pinned to LITERALS rather than derived,
// because the failure mode is silence: these were "PENDING"/"DONE"/"FAILED"
// here while ghost-cli has always written "pending"/"done"/"error", so no case
// could ever match. A finished rewrite was collected by nothing, the poll ran to
// its 240 s ceiling, and the owner was told the rewrite "did not finish in time"
// — of a job that had finished in thirty seconds. Every preprocess:true request
// burned four minutes of worker time, one unit of the Opus quota and one Bedrock
// call, and threw the result away.
//
// The uppercase values were not invented: ghost-cli has a SECOND status
// vocabulary, for RUN rows, and that one really is uppercase — RUNNING /
// COMPLETED / FAILED (lambda/command/schedule_run.go:27-30), which this client
// also consumes. Two vocabularies in one API is the trap; the only defence is
// pinning each one where it is read.
func TestPreprocessStatusesMatchGhostCLI(t *testing.T) {
	// ghost-cli: lambda/command/schedule_prompt_job.go:60-63
	//   preprocessStatusPending = "pending"
	//   preprocessStatusDone    = "done"
	//   preprocessStatusError   = "error"
	// Served verbatim as the "status" field by lambda/command/schedule_prompt_status.go,
	// which normalises every other row state to one of these three.
	for _, tc := range []struct{ got, want string }{
		{PreprocessPending, "pending"},
		{PreprocessDone, "done"},
		{PreprocessError, "error"},
	} {
		if tc.got != tc.want {
			t.Errorf("status literal = %q, want %q (ghost-cli lambda/command/schedule_prompt_job.go:60-63)",
				tc.got, tc.want)
		}
	}
}

// The comparison folds case so that a future casing change on either side of the
// contract costs a slow path, not a discarded rewrite. It must not fold anything
// else — a status that merely contains "done" is not done.
func TestPreprocessIsFoldsCaseOnly(t *testing.T) {
	for _, status := range []string{"done", "DONE", "Done", " done "} {
		if !PreprocessIs(status, PreprocessDone) {
			t.Errorf("PreprocessIs(%q, done) = false, want true", status)
		}
	}
	for _, status := range []string{"", "don", "done-ish", "not done", "pending"} {
		if PreprocessIs(status, PreprocessDone) {
			t.Errorf("PreprocessIs(%q, done) = true, want false", status)
		}
	}
}

func TestPreprocessReturnsJobID(t *testing.T) {
	f := &fakeInvoke{responses: []proxyResponse{{StatusCode: 202, Body: `{"job_id":"job-7","status":"pending"}`}}}
	jobID, err := testClient(f).Preprocess(context.Background(),
		PreprocessRequest{Prompt: "tighten the retry", CLI: "claude", Node: "officepc",
			Repo: "o/live-ninja", OutputFile: "update-report.md"}, "corr-1")
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if jobID != "job-7" {
		t.Errorf("job id = %q, want job-7", jobID)
	}
	ev := f.events[0]
	if ev.Resource != "/schedule/preprocess" || ev.Method != "POST" {
		t.Errorf("wrong route: %s %s", ev.Method, ev.Resource)
	}
	if ev.CorrelationID != "corr-1" {
		t.Errorf("correlation id = %q, want corr-1", ev.CorrelationID)
	}
}

func TestPreprocessWithoutJobIDIsAnError(t *testing.T) {
	f := &fakeInvoke{responses: []proxyResponse{{StatusCode: 202, Body: `{"status":"pending"}`}}}
	if _, err := testClient(f).Preprocess(context.Background(), PreprocessRequest{}, ""); !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v, want ErrUpstream", err)
	}
}

func TestPreprocessStatusPassesJobIDAsQuery(t *testing.T) {
	f := &fakeInvoke{responses: []proxyResponse{ok(`{"job_id":"job-7","status":"done","prompt":"rewritten"}`)}}
	st, err := testClient(f).PreprocessStatus(context.Background(), "job-7", "")
	if err != nil {
		t.Fatalf("PreprocessStatus: %v", err)
	}
	if st.Status != PreprocessDone || st.Prompt != "rewritten" {
		t.Errorf("status = %+v", st)
	}
	if f.events[0].Query["job_id"] != "job-7" {
		t.Errorf("query = %v, want job_id=job-7", f.events[0].Query)
	}
}

// Launch must always send run_now — a request that reached ghost-cli without it
// would be rejected for having no schedule source, and the caller cannot forget.
func TestLaunchAlwaysSetsRunNow(t *testing.T) {
	f := &fakeInvoke{responses: []proxyResponse{{StatusCode: 202,
		Body: `{"event_id":"ev-1","run":{"run_id":"run-1","node":"officepc","status":"RUNNING"}}`}}}
	res, err := testClient(f).Launch(context.Background(), LaunchRequest{
		EventID: "ev-1", Node: "officepc", Repo: "o/live-ninja", CLI: "claude",
		Prompt: "do it", OutputFile: "update-report.md", RunNow: false,
	}, "corr-2")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.Run.RunID != "run-1" {
		t.Errorf("run id = %q", res.Run.RunID)
	}
	var sent LaunchRequest
	if err := json.Unmarshal([]byte(f.events[0].Body), &sent); err != nil {
		t.Fatal(err)
	}
	if !sent.RunNow {
		t.Error("Launch sent run_now=false; the caller must not be able to turn it off")
	}
	if f.events[0].Resource != "/schedule" {
		t.Errorf("resource = %q, want /schedule", f.events[0].Resource)
	}
}

// A 409 is a real answer ("something is already running"), so the body comes
// back alongside the typed error rather than being discarded.
func TestLaunchConflictSurfacesTypedError(t *testing.T) {
	f := &fakeInvoke{responses: []proxyResponse{{StatusCode: 409,
		Body: `{"error":"a run is already in progress for this event"}`}}}
	_, err := testClient(f).Launch(context.Background(), LaunchRequest{}, "")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

func TestLaunchWithoutRunIDIsAnError(t *testing.T) {
	f := &fakeInvoke{responses: []proxyResponse{{StatusCode: 202, Body: `{"event_id":"ev-1"}`}}}
	if _, err := testClient(f).Launch(context.Background(), LaunchRequest{}, ""); !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v, want ErrUpstream", err)
	}
}

func TestFindRun(t *testing.T) {
	body := `{"events":[
		{"event_id":"ev-1","node":"officepc","repo":"o/live-ninja","runs":[
			{"run_id":"run-1","status":"COMPLETED","summary":"did the thing"}]}]}`
	f := &fakeInvoke{responses: []proxyResponse{ok(body), ok(body), ok(body)}}
	c := testClient(f)

	ev, run, err := c.FindRun(context.Background(), "ev-1", "run-1", "")
	if err != nil {
		t.Fatalf("FindRun: %v", err)
	}
	if ev.Repo != "o/live-ninja" || run.Status != "COMPLETED" || run.Summary != "did the thing" {
		t.Errorf("ev=%+v run=%+v", ev, run)
	}

	if _, _, err := c.FindRun(context.Background(), "ev-1", "run-missing", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing run → %v, want ErrNotFound", err)
	}
	if _, _, err := c.FindRun(context.Background(), "ev-missing", "run-1", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing event → %v, want ErrNotFound", err)
	}
}

func TestNodes(t *testing.T) {
	f := &fakeInvoke{responses: []proxyResponse{ok(`{"nodes":[{"node_id":"officepc","status":"ONLINE"}]}`)}}
	nodes, err := testClient(f).Nodes(context.Background(), "")
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].NodeID != "officepc" || nodes[0].Status != "ONLINE" {
		t.Errorf("nodes = %+v", nodes)
	}
}
