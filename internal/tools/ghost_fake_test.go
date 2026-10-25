package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// ghostNow is the fixed clock every ghost_* test runs against.
var ghostNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// ghostEvent is the internal-invoke event as the fake saw it.
type ghostEvent struct {
	Task          string            `json:"ghost_task"`
	Resource      string            `json:"resource"`
	Method        string            `json:"method"`
	Body          string            `json:"body"`
	Query         map[string]string `json:"query"`
	CorrelationID string            `json:"correlation_id"`
}

// ghostResponder answers one routed request.
type ghostResponder func(ev ghostEvent) (status int, body string)

// ghostFake routes internal-invoke events by "METHOD resource" and records
// every call, in order. An unrouted call answers 404 the way ghost-cli's
// router would for a route the allowlist does not carry.
type ghostFake struct {
	mu     sync.Mutex
	routes map[string]ghostResponder
	// fnErrors answers a route with a Lambda FunctionError carrying that
	// errorMessage, the way ghost-cli refuses a resource outside its
	// internal-invoke allowlist before its router runs.
	fnErrors map[string]string
	calls    []ghostEvent
}

func newGhostFake() *ghostFake {
	return &ghostFake{routes: map[string]ghostResponder{}, fnErrors: map[string]string{}}
}

// fnError makes one route fail with a Lambda FunctionError whose payload is
// {"errorMessage": msg}.
func (f *ghostFake) fnError(method, resource, msg string) *ghostFake {
	f.fnErrors[method+" "+resource] = msg
	return f
}

// ghostAllowlistRefusal is ghost-cli's InternalInvokeError text for a route
// its allowlist does not carry (lambda/command/internal_invoke.go).
func ghostAllowlistRefusal(method, resource string) string {
	return "internal invoke: " + method + " " + resource + " is not reachable by internal invoke"
}

func (f *ghostFake) on(method, resource string, r ghostResponder) *ghostFake {
	f.routes[method+" "+resource] = r
	return f
}

func (f *ghostFake) static(method, resource string, status int, body string) *ghostFake {
	return f.on(method, resource, func(ghostEvent) (int, string) { return status, body })
}

func (f *ghostFake) Invoke(_ context.Context, in *awslambda.InvokeInput, _ ...func(*awslambda.Options)) (*awslambda.InvokeOutput, error) {
	var ev ghostEvent
	if err := json.Unmarshal(in.Payload, &ev); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, ev)
	r, ok := f.routes[ev.Method+" "+ev.Resource]
	fnMsg, fnErr := f.fnErrors[ev.Method+" "+ev.Resource]
	f.mu.Unlock()
	if fnErr {
		p, _ := json.Marshal(map[string]any{"errorMessage": fnMsg, "errorType": "InternalInvokeError"})
		return &awslambda.InvokeOutput{FunctionError: aws.String("Unhandled"), Payload: p}, nil
	}
	status, body := 404, `{"error":"not found"}`
	if ok {
		status, body = r(ev)
	}
	p, _ := json.Marshal(map[string]any{"statusCode": status, "body": body})
	return &awslambda.InvokeOutput{Payload: p}, nil
}

// callsTo returns the recorded events for one route.
func (f *ghostFake) callsTo(method, resource string) []ghostEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ghostEvent
	for _, ev := range f.calls {
		if ev.Method == method && ev.Resource == resource {
			out = append(out, ev)
		}
	}
	return out
}

// ghostDeps builds a registry Deps over the fake fleet and a capturing
// FakeDynamo (so IDEMP# claims and releases can be asserted).
func ghostDeps(f *ghostFake) (*Deps, *testutil.FakeDynamo) {
	d, fake := newTestDepsWithFake()
	d.Now = func() time.Time { return ghostNow }
	d.Ghost = ghost.New(ghost.Config{
		API: f, Function: "ghost-cli-command",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return ghostNow },
	})
	return d, fake
}

// ghostOwnerInvocation is an owner call with an idempotency key, the shape
// the side-effecting ghost tools require.
func ghostOwnerInvocation(tool, key string, args map[string]any) Invocation {
	inv := invocation(tool, args)
	inv.Role = store.RoleOwner
	inv.IdempotencyKey = key
	return inv
}

// ghostPropose runs the unconfirmed call of a manage tool and returns the
// server-issued proposalId it must be confirmed with (ghost_confirm.go).
func ghostPropose(t *testing.T, r *Registry, tool, key string, args map[string]any) string {
	t.Helper()
	res := r.Invoke(context.Background(), ghostOwnerInvocation(tool, key, args))
	terr := requireToolError(t, res, CodeConfirmationRequired)
	id, _ := terr.Details["proposalId"].(string)
	require.Regexp(t, `^[a-f0-9]{32}$`, id, "a proposal carries its server-issued id")
	require.Equal(t, true, terr.Details["confirmRequired"])
	require.NotEmpty(t, terr.Details["proposalExpires"])
	return id
}

// ghostConfirmArgs is args plus confirm=true and the proposalId: the confirmed
// call's shape.
func ghostConfirmArgs(args map[string]any, proposalID string) map[string]any {
	out := make(map[string]any, len(args)+2)
	for k, v := range args {
		out[k] = v
	}
	out["confirm"] = true
	out["proposalId"] = proposalID
	return out
}

// ghostProposalRow reads the GHOSTPROP# row for one proposal, nil once consumed.
func ghostProposalRow(fake *testutil.FakeDynamo, proposalID string) map[string]ddbtypes.AttributeValue {
	return fake.RawItem("GHOSTPROP#user-1#"+proposalID, "GHOSTPROP")
}

// Fixtures ---------------------------------------------------------------

const ghostSessionA = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
const ghostSessionB = "11111111-2222-3333-4444-555555555555"
const ghostSessionC = "99999999-8888-7777-6666-555555555555"

func ghostNodesBody(nodes ...map[string]any) string {
	if nodes == nil {
		nodes = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"nodes": nodes})
	return string(b)
}

func ghostNode(id, state string, connected bool, version string) map[string]any {
	return map[string]any{"node_id": id, "state": state, "agent_version": version,
		"last_seen": ghostNow.Add(-time.Minute).Format(time.RFC3339), "connected": connected}
}

func ghostSessionsBody(node, next string, sessions ...map[string]any) string {
	if sessions == nil {
		sessions = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "node_id": node, "sessions": sessions,
		"next_cursor": next, "coverage": "retained_only"})
	return string(b)
}

// ghostPagedSessions answers GET /history/sessions for one node the way
// upstream does: the listing is split across pages in ascending upload order,
// so the newest sessions are on the LAST page. The opaque cursor is "p<i>".
func ghostPagedSessions(t *testing.T, node string, pages ...[]map[string]any) ghostResponder {
	return func(ev ghostEvent) (int, string) {
		require.Equal(t, node, ev.Query["node_id"])
		i := 0
		if c := ev.Query["cursor"]; c != "" {
			_, err := fmt.Sscanf(c, "p%d", &i)
			require.NoError(t, err, "cursor %q was not one upstream issued", c)
		}
		require.Less(t, i, len(pages), "cursor walked past the last upstream page")
		next := ""
		if i+1 < len(pages) {
			next = fmt.Sprintf("p%d", i+1)
		}
		return 200, ghostSessionsBody(node, next, pages[i]...)
	}
}

// ghostEndlessSessions never stops paging: every page carries one old session
// and a next cursor, so only the walk bound can end a scan.
func ghostEndlessSessions(node string) ghostResponder {
	return func(ev ghostEvent) (int, string) {
		i := 0
		if c := ev.Query["cursor"]; c != "" {
			_, _ = fmt.Sscanf(c, "p%d", &i)
		}
		old := ghostSession(fmt.Sprintf("00000000-0000-0000-0000-%012d", i), 60*24*time.Hour)
		return 200, ghostSessionsBody(node, fmt.Sprintf("p%d", i+1), old)
	}
}

func ghostSession(id string, updatedAgo time.Duration) map[string]any {
	s := map[string]any{"session_id": id, "date": "2026-10-19"}
	if updatedAgo >= 0 {
		s["updated_at"] = ghostNow.Add(-updatedAgo).Format(time.RFC3339Nano)
	}
	return s
}

// ghostEventsBody renders one /history/events page. Events need id, sequence,
// kind, text, part, more; the client validates strictly increasing sequences.
func ghostEventsBody(node, session, next string, events []map[string]any, gaps ...string) string {
	if events == nil {
		events = []map[string]any{}
	}
	if gaps == nil {
		gaps = []string{}
	}
	page := map[string]any{"version": 1, "node_id": node, "session_id": session, "events": events,
		"resume_cursor": "resume-" + session, "coverage": "retained_only", "gaps": gaps}
	if next != "" {
		page["next_cursor"] = next
	}
	b, _ := json.Marshal(page)
	return string(b)
}

func ghostHistoryEvent(i int, kind, text string) map[string]any {
	seq := fmt.Sprintf("batch/0001/%05d", i)
	ev := map[string]any{"id": seq, "sequence": seq, "kind": kind, "text": text, "part": 0, "more": false,
		"timestamp": ghostNow.Add(-time.Duration(1000-i) * time.Second).Format(time.RFC3339)}
	if kind == "tool_call" || kind == "tool_result" {
		ev["call_id"] = fmt.Sprintf("call-%d", i)
		ev["name"] = "Bash"
	}
	return ev
}

// requireToolError asserts a registry result failed with the given code.
func requireToolError(t *testing.T, res *Result, code string) *ToolError {
	t.Helper()
	require.False(t, res.OK, "expected %s, got ok output %v", code, res.Output)
	require.NotNil(t, res.Error)
	require.Equalf(t, code, res.Error.Code, "message: %s", res.Error.Message)
	return res.Error
}

// body decodes a recorded POST body.
func (e ghostEvent) bodyMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(e.Body), &m), "body %q", e.Body)
	return m
}

// itemString reads one string attribute off a captured DynamoDB item, or ""
// when it is absent or not a string.
func itemString(item map[string]ddbtypes.AttributeValue, key string) string {
	if v, ok := item[key].(*ddbtypes.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}
