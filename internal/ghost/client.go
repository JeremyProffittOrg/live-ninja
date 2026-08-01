// Package ghost is Live Ninja's client for the ghost-cli fleet API — the
// service that owns the Windows nodes, their coding-agent sessions, and the
// GitHub launcher credential.
//
// It does not speak HTTP. ghost-cli's command Lambda accepts an internal-invoke
// event (lambda/command/internal_invoke.go) alongside its API Gateway events:
// the event is synthesized server-side into the same proxy request the REST
// router serves, so the capability matrix and the hash-chained audit log run
// exactly as they do for a browser. Only authentication differs — it is the IAM
// grant on this function's role rather than a session cookie or an API key.
//
// Three consequences worth holding on to:
//
//   - This client cannot name its own principal. ghost-cli pins it from its own
//     configuration, so every call is attributed to (and bounded by) the
//     `live-ninja` allowlist entry, whatever this code sends.
//   - Only ghost-cli's allowlisted resources are reachable. Asking for anything
//     else fails at the far end, not here, and that is deliberate — the closed
//     set lives with the thing it protects.
//   - There is no credential to rotate, leak, or log.
package ghost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// internalInvokeTask is ghost-cli's event discriminator. It is a CROSS-REPO
// CONTRACT with lambda/command/internal_invoke.go: change it there and this
// client silently starts being interpreted as an API Gateway request, which
// fails closed (no principal ⇒ 401) but confusingly. Pinned by a test on both
// sides.
const internalInvokeTask = "internal_api"

// repoCacheTTL bounds how long a container reuses a repo listing. A voice
// conversation is over in well under this, so one listing serves a whole
// interaction; a repo created mid-conversation is the case this trades away,
// and the discovery query re-lists on a miss anyway.
const repoCacheTTL = 5 * time.Minute

// Errors callers switch on. They carry no upstream body: ghost-cli's error
// bodies are already client-safe, but they are not shaped for a voice model, so
// the tool layer renders its own wording from these.
var (
	// ErrNotAuthorized is a 401/403 — normally the `live-ninja` principal is
	// missing from ghost-cli's signed launch allowlist, or lacks launch on the
	// requested node.
	ErrNotAuthorized = errors.New("ghost: not authorized")
	// ErrNotFound is a 404.
	ErrNotFound = errors.New("ghost: not found")
	// ErrConflict is a 409 — most often "a run is already in progress for this
	// event", which is a real answer rather than a failure.
	ErrConflict = errors.New("ghost: conflict")
	// ErrQuota is a 429 — the Opus preprocessing quota (10 jobs / 10 min).
	ErrQuota = errors.New("ghost: quota exceeded")
	// ErrUnavailable is a 503 — a ghost-cli route that is deployed but not
	// configured (no publisher, no job table).
	ErrUnavailable = errors.New("ghost: upstream not configured")
	// ErrUpstream is anything else, including a transport failure.
	ErrUpstream = errors.New("ghost: upstream error")
	// ErrNotConfigured means THIS side has no function name wired.
	ErrNotConfigured = errors.New("ghost: client is not configured")
)

// InvokeAPI is the one Lambda control-plane operation this package needs.
// A *lambda.Client satisfies it; tests inject a fake.
type InvokeAPI interface {
	Invoke(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

// Client talks to ghost-cli's command Lambda.
type Client struct {
	api      InvokeAPI
	function string
	log      *slog.Logger

	mu      sync.Mutex
	repos   []Repo
	reposAt time.Time
	nowFunc func() time.Time
}

// Config wires a Client. Function is the ghost-cli command function's name or
// ARN (env GHOST_COMMAND_FUNCTION_ARN); empty makes every call return
// ErrNotConfigured rather than pretending the fleet is unreachable.
type Config struct {
	API      InvokeAPI
	Function string
	Log      *slog.Logger
	Now      func() time.Time
}

func New(cfg Config) *Client {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		api:      cfg.API,
		function: strings.TrimSpace(cfg.Function),
		log:      log,
		nowFunc:  now,
	}
}

// Ready reports whether the client can reach ghost-cli at all.
func (c *Client) Ready() bool { return c != nil && c.api != nil && c.function != "" }

// invokeEvent is the wire shape ghost-cli's DecodeInternalInvokeEvent reads. It
// deliberately carries no principal: see the package comment.
type invokeEvent struct {
	Task          string            `json:"ghost_task"`
	Resource      string            `json:"resource"`
	Method        string            `json:"method"`
	Body          string            `json:"body,omitempty"`
	Query         map[string]string `json:"query,omitempty"`
	CorrelationID string            `json:"correlation_id,omitempty"`
}

// proxyResponse is the API-Gateway-shaped result ghost-cli's router returns.
type proxyResponse struct {
	StatusCode int    `json:"statusCode"`
	Body       string `json:"body"`
}

// call performs one internal invoke and returns the response body, mapping the
// status onto a typed error.
//
// It NEVER logs `body` or the response body: a schedule create carries the
// owner's prompt, and a preprocess request carries their raw instructions.
func (c *Client) call(ctx context.Context, method, resource, body string, query map[string]string, corrID string) (string, error) {
	if !c.Ready() {
		return "", ErrNotConfigured
	}
	payload, err := json.Marshal(invokeEvent{
		Task:          internalInvokeTask,
		Resource:      resource,
		Method:        method,
		Body:          body,
		Query:         query,
		CorrelationID: corrID,
	})
	if err != nil {
		return "", fmt.Errorf("%w: marshal event: %v", ErrUpstream, err)
	}

	out, err := c.api.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(c.function),
		InvocationType: lambdatypes.InvocationTypeRequestResponse,
		Payload:        payload,
	})
	if err != nil {
		c.log.Error("ghost: invoke failed",
			slog.String("resource", resource), slog.String("error", err.Error()))
		return "", fmt.Errorf("%w: invoke: %v", ErrUpstream, err)
	}
	// A handled Lambda error (a rejected internal-invoke event, a panic) sets
	// FunctionError; the payload is then an error object, not our response.
	if fe := aws.ToString(out.FunctionError); fe != "" {
		c.log.Error("ghost: function error",
			slog.String("resource", resource), slog.String("function_error", fe))
		return "", fmt.Errorf("%w: function error %s", ErrUpstream, fe)
	}

	var resp proxyResponse
	if err := json.Unmarshal(out.Payload, &resp); err != nil {
		return "", fmt.Errorf("%w: decode response: %v", ErrUpstream, err)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.Body, nil
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		c.log.Warn("ghost: not authorized", slog.String("resource", resource))
		return "", ErrNotAuthorized
	case resp.StatusCode == 404:
		return "", ErrNotFound
	case resp.StatusCode == 409:
		return resp.Body, ErrConflict
	case resp.StatusCode == 429:
		return "", ErrQuota
	case resp.StatusCode == 503:
		return "", ErrUnavailable
	default:
		c.log.Error("ghost: unexpected status",
			slog.String("resource", resource), slog.Int("status", resp.StatusCode))
		return "", fmt.Errorf("%w: status %d", ErrUpstream, resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Repos
// ---------------------------------------------------------------------------

// Repo is one launchable repository.
type Repo struct {
	// Repo is the full "owner/name".
	Repo string `json:"repo"`
	// Owner and Name are the split halves, precomputed because the matcher and
	// the voice rendering both want the name alone.
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

type launchReposResponse struct {
	Repos []struct {
		Repo string `json:"repo"`
	} `json:"repos"`
}

// ListRepos returns every repository ghost-cli's launcher credential can reach,
// most-recently-pushed first (ghost-cli sorts; this preserves that order, which
// is what makes "the latest 20" meaningful). Cached per container for
// repoCacheTTL.
func (c *Client) ListRepos(ctx context.Context) ([]Repo, error) {
	c.mu.Lock()
	if c.repos != nil && c.nowFunc().Sub(c.reposAt) < repoCacheTTL {
		cached := c.repos
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	body, err := c.call(ctx, "GET", "/launch/repos", "", nil, "")
	if err != nil {
		return nil, err
	}
	var parsed launchReposResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return nil, fmt.Errorf("%w: decode repos: %v", ErrUpstream, err)
	}

	repos := make([]Repo, 0, len(parsed.Repos))
	for _, r := range parsed.Repos {
		full := strings.TrimSpace(r.Repo)
		owner, name, ok := strings.Cut(full, "/")
		if !ok || owner == "" || name == "" {
			continue
		}
		repos = append(repos, Repo{Repo: full, Owner: owner, Name: name})
	}

	c.mu.Lock()
	c.repos = repos
	c.reposAt = c.nowFunc()
	c.mu.Unlock()
	return repos, nil
}

// ---------------------------------------------------------------------------
// Prompt preprocessing (Opus)
// ---------------------------------------------------------------------------

// PreprocessRequest mirrors ghost-cli's schedulePromptRequest.
type PreprocessRequest struct {
	Prompt     string `json:"prompt"`
	CLI        string `json:"cli"`
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	OutputFile string `json:"output_file"`
	Node       string `json:"node"`
	Repo       string `json:"repo"`
}

// Preprocess starts an Opus rewrite and returns the job id to poll. The call
// itself is fast — ghost-cli records a job and re-invokes itself, because the
// rewrite outruns API Gateway's ~29 s ceiling.
func (c *Client) Preprocess(ctx context.Context, req PreprocessRequest, corrID string) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("%w: marshal preprocess request: %v", ErrUpstream, err)
	}
	resp, err := c.call(ctx, "POST", "/schedule/preprocess", string(body), nil, corrID)
	if err != nil {
		return "", err
	}
	var parsed struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
		return "", fmt.Errorf("%w: decode preprocess response: %v", ErrUpstream, err)
	}
	if parsed.JobID == "" {
		return "", fmt.Errorf("%w: preprocess returned no job id", ErrUpstream)
	}
	return parsed.JobID, nil
}

// Preprocess job statuses. These are a CROSS-REPO CONTRACT: they are the
// literal values ghost-cli stores on a job row and serves from
// GET /schedule/preprocess-status (lambda/command/schedule_prompt_job.go:60-63,
// read out by lambda/command/schedule_prompt_status.go). They are LOWERCASE, and
// the failure value is "error" — "failed" never existed on the wire.
//
// This is pinned by TestPreprocessStatusesMatchGhostCLI because the failure is
// invisible: a status this client cannot recognise does not raise anything, it
// just makes every poll look unfinished, so a rewrite that arrived in thirty
// seconds is discarded four minutes later as a timeout that never happened.
const (
	PreprocessPending = "pending"
	PreprocessDone    = "done"
	PreprocessError   = "error"
)

// PreprocessIs reports whether a status value received from ghost-cli names the
// given state. It folds case deliberately: if either side of this contract ever
// changes the casing of its literals, the mismatch must degrade to a slow path
// that still works, never to a silent one that discards good work.
func PreprocessIs(status, want string) bool {
	return strings.EqualFold(strings.TrimSpace(status), want)
}

// PreprocessStatus is one poll of a rewrite job.
type PreprocessStatus struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Prompt string `json:"prompt"`
	Error  string `json:"error"`
}

// PreprocessStatus polls a rewrite job.
func (c *Client) PreprocessStatus(ctx context.Context, jobID, corrID string) (PreprocessStatus, error) {
	resp, err := c.call(ctx, "GET", "/schedule/preprocess-status", "",
		map[string]string{"job_id": jobID}, corrID)
	if err != nil {
		return PreprocessStatus{}, err
	}
	var parsed PreprocessStatus
	if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
		return PreprocessStatus{}, fmt.Errorf("%w: decode preprocess status: %v", ErrUpstream, err)
	}
	return parsed, nil
}

// ---------------------------------------------------------------------------
// Launch (create-and-fire)
// ---------------------------------------------------------------------------

// LaunchRequest is a ghost-cli schedule create with run_now set: it stores a
// disabled, re-runnable event and dispatches it immediately.
type LaunchRequest struct {
	EventID    string `json:"event_id,omitempty"`
	Node       string `json:"node"`
	Repo       string `json:"repo"`
	CLI        string `json:"cli"`
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Prompt     string `json:"prompt"`
	OutputFile string `json:"output_file"`
	RunNow     bool   `json:"run_now"`

	// Deploy tells ghost-cli whether this run may push. It is the cloud half of
	// the pre-push hook: ghost-cli maps deploy==false to no_push:true in the
	// LAUNCH params, and the agent's launcher.ApplyNoPush installs a real
	// pre-push hook in the workspace, so a held run cannot push even if the
	// prompt's DO NOT PUSH sentence never arrived (which is exactly what the
	// v1.1.52 prompt-transport defect did).
	//
	// Sending this is SAFE AHEAD OF ghost-cli reading it, and that is verified,
	// not assumed: lambda/command/schedule.go decodes the create body with a
	// plain json.Unmarshal, so an unknown key is ignored. The strict
	// DisallowUnknownFields decoder is on the AGENT's command envelope, one hop
	// further on — which is why the cloud must never emit no_push to a node
	// older than v1.1.53, and why this field alone cannot cause that.
	//
	// NOT omitempty, deliberately. This is a security-relevant boolean whose
	// dangerous value is `false`; omitempty would drop exactly the "do not
	// push" case off the wire and leave ghost-cli to infer a default. The one
	// value that must always be transmitted is the one omitempty deletes.
	Deploy bool `json:"deploy"`
}

// LaunchResult is the dispatched run.
type LaunchResult struct {
	EventID string `json:"event_id"`
	Run     struct {
		RunID     string `json:"run_id"`
		Node      string `json:"node"`
		Status    string `json:"status"`
		Ts        string `json:"ts"`
		CommandID string `json:"command_id"`
	} `json:"run"`
}

// Launch creates the event and fires it now.
func (c *Client) Launch(ctx context.Context, req LaunchRequest, corrID string) (LaunchResult, error) {
	req.RunNow = true
	body, err := json.Marshal(req)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("%w: marshal launch request: %v", ErrUpstream, err)
	}
	resp, err := c.call(ctx, "POST", "/schedule", string(body), nil, corrID)
	if err != nil {
		return LaunchResult{}, err
	}
	var parsed LaunchResult
	if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
		return LaunchResult{}, fmt.Errorf("%w: decode launch response: %v", ErrUpstream, err)
	}
	if parsed.Run.RunID == "" {
		return LaunchResult{}, fmt.Errorf("%w: launch returned no run id", ErrUpstream)
	}
	return parsed, nil
}

// ---------------------------------------------------------------------------
// Run status
// ---------------------------------------------------------------------------

// Run is one recorded run of an event.
type Run struct {
	RunID   string `json:"run_id"`
	Status  string `json:"status"`
	Ts      string `json:"ts"`
	Node    string `json:"node"`
	Trigger string `json:"trigger"`
	Summary string `json:"summary"`
}

// Event is a stored schedule event with its recent runs.
type Event struct {
	EventID       string `json:"event_id"`
	Node          string `json:"node"`
	Repo          string `json:"repo"`
	CLI           string `json:"cli"`
	LastRunID     string `json:"last_run_id"`
	LastRunStatus string `json:"last_run_status"`
	LastRunTs     string `json:"last_run_ts"`
	Runs          []Run  `json:"runs"`
}

// FindRun returns the run with the given id, searching the event list. ghost-cli
// has no by-run-id read, and the event list is small (a Scan of a tiny table it
// already caches), so this is the cheap path rather than a new upstream route.
func (c *Client) FindRun(ctx context.Context, eventID, runID, corrID string) (Event, Run, error) {
	resp, err := c.call(ctx, "GET", "/schedule", "", nil, corrID)
	if err != nil {
		return Event{}, Run{}, err
	}
	var parsed struct {
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
		return Event{}, Run{}, fmt.Errorf("%w: decode events: %v", ErrUpstream, err)
	}
	for _, ev := range parsed.Events {
		if ev.EventID != eventID {
			continue
		}
		for _, r := range ev.Runs {
			if r.RunID == runID {
				return ev, r, nil
			}
		}
		return ev, Run{}, ErrNotFound
	}
	return Event{}, Run{}, ErrNotFound
}

// ---------------------------------------------------------------------------
// Nodes
// ---------------------------------------------------------------------------

// Node is one fleet machine as GET /nodes reports it.
type Node struct {
	NodeID string `json:"node_id"`
	Status string `json:"status"`
}

// Nodes lists the fleet. Used to tell "that machine is offline" apart from
// "that machine does not exist" before a launch is queued.
func (c *Client) Nodes(ctx context.Context, corrID string) ([]Node, error) {
	resp, err := c.call(ctx, "GET", "/nodes", "", nil, corrID)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Nodes []Node `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
		return nil, fmt.Errorf("%w: decode nodes: %v", ErrUpstream, err)
	}
	return parsed.Nodes, nil
}
