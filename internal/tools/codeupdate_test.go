package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/JeremyProffittOrg/live-ninja/internal/codeupdate"
	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
)

// cuGhost replays a fixed repo listing.
type cuGhost struct {
	body   string
	status int
	calls  int
}

func (f *cuGhost) Invoke(_ context.Context, _ *awslambda.InvokeInput, _ ...func(*awslambda.Options)) (*awslambda.InvokeOutput, error) {
	f.calls++
	status := f.status
	if status == 0 {
		status = 200
	}
	p, _ := json.Marshal(map[string]any{"statusCode": status, "body": f.body})
	return &awslambda.InvokeOutput{Payload: p}, nil
}

// cuSQS captures enqueued queue messages.
type cuSQS struct {
	bodies []string
}

func (f *cuSQS) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.bodies = append(f.bodies, aws.ToString(in.MessageBody))
	return &sqs.SendMessageOutput{}, nil
}

const cuRepoListing = `{"repos":[
	{"repo":"JeremyProffittOrg/live-ninja"},
	{"repo":"JeremyProffittOrg/ghost-cli"},
	{"repo":"JeremyProffittOrg/aws-cost-reporting"},
	{"repo":"JeremyProffittOrg/ghost-agent-docs"}
]}`

// cuDDB captures the CODEUPD# rows the store writes. Only PutItem is modelled
// with any care — the rest exist to satisfy codeupdate.DDB.
type cuDDB struct {
	puts []map[string]ddbtypes.AttributeValue
}

func (f *cuDDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.puts = append(f.puts, in.Item)
	return &dynamodb.PutItemOutput{}, nil
}

func (f *cuDDB) GetItem(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return &dynamodb.GetItemOutput{}, nil
}

func (f *cuDDB) UpdateItem(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	return &dynamodb.UpdateItemOutput{}, nil
}

func (f *cuDDB) Query(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	return &dynamodb.QueryOutput{}, nil
}

func cuDeps(g *cuGhost, q *cuSQS) *Deps {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Deps{
		Log:                log,
		SQS:                q,
		Ghost:              ghost.New(ghost.Config{API: g, Function: "ghost-cli-command", Log: log}),
		CodeUpdateQueueURL: "https://sqs/code-update",
	}
}

// cuDepsWithStore is cuDeps plus a real store over a capturing fake, for the
// assertions that care what actually lands on the row.
func cuDepsWithStore(g *cuGhost, q *cuSQS, db *cuDDB) *Deps {
	d := cuDeps(g, q)
	d.CodeUpdate = codeupdate.NewStore(db, "live-ninja", nil)
	return d
}

func cuInvocation() Invocation {
	return Invocation{UserID: "user-1", SessionID: "sess-1"}
}

func startArgs(overrides map[string]any) map[string]any {
	args := map[string]any{
		"repo":         "JeremyProffittOrg/live-ninja",
		"instructions": "tighten the retry logic on the Bedrock client",
		"confirm":      true,
	}
	for k, v := range overrides {
		args[k] = v
	}
	return args
}

func decodeQueued(t *testing.T, q *cuSQS) codeupdate.Request {
	t.Helper()
	if len(q.bodies) == 0 {
		t.Fatal("nothing was enqueued")
	}
	var req codeupdate.Request
	if err := json.Unmarshal([]byte(q.bodies[len(q.bodies)-1]), &req); err != nil {
		t.Fatalf("unmarshal queue message: %v", err)
	}
	return req
}

// ---------------------------------------------------------------------------
// code_update_repos
// ---------------------------------------------------------------------------

func TestCodeUpdateReposDefaultsToTwenty(t *testing.T) {
	g := &cuGhost{body: cuRepoListing}
	out, terr := handleCodeUpdateRepos(context.Background(), cuDeps(g, &cuSQS{}), cuInvocation(), map[string]any{})
	if terr != nil {
		t.Fatalf("handler error: %v", terr)
	}
	repos, _ := out["repos"].([]map[string]any)
	if len(repos) != 4 {
		t.Fatalf("returned %d repos, want all 4 (fewer than the limit)", len(repos))
	}
	if repos[0]["repo"] != "JeremyProffittOrg/live-ninja" {
		t.Errorf("upstream order (most-recently-pushed first) was not preserved: %v", repos[0])
	}
	if out["searched"] != false {
		t.Error("a bare listing must not report itself as a search")
	}
}

// The discovery leg: a spoken name searches the FULL list, not just the recent
// slice.
func TestCodeUpdateReposSearches(t *testing.T) {
	g := &cuGhost{body: cuRepoListing}
	out, terr := handleCodeUpdateRepos(context.Background(), cuDeps(g, &cuSQS{}), cuInvocation(),
		map[string]any{"query": "cost reporting"})
	if terr != nil {
		t.Fatalf("handler error: %v", terr)
	}
	repos, _ := out["repos"].([]map[string]any)
	if len(repos) == 0 {
		t.Fatal("search found nothing")
	}
	if repos[0]["repo"] != "JeremyProffittOrg/aws-cost-reporting" {
		t.Errorf("top result = %v, want aws-cost-reporting", repos[0]["repo"])
	}
	if out["searched"] != true {
		t.Error("a query must report itself as a search")
	}
}

// This MUST exercise the real coercion path. Calling the handler directly with a
// float64 (what raw JSON produces) is what hid a bug where the handler asserted
// float64 while registry.go's validateArgs hands integer params through as a Go
// int — the assertion never matched and `limit` was pinned at 20 forever, with a
// green test sitting right next to it.
func TestCodeUpdateReposRespectsLimitThroughTheRouter(t *testing.T) {
	g := &cuGhost{body: cuRepoListing}
	def := codeUpdateReposDefinition()

	args, terr := validateArgs(def, map[string]any{"limit": float64(2)})
	if terr != nil {
		t.Fatalf("validateArgs rejected a valid limit: %v", terr)
	}
	if _, isInt := args["limit"].(int); !isInt {
		t.Fatalf("validateArgs produced %T for an integer param; the handler's type assertion "+
			"must match whatever this is", args["limit"])
	}

	out, terr := handleCodeUpdateRepos(context.Background(), cuDeps(g, &cuSQS{}), cuInvocation(), args)
	if terr != nil {
		t.Fatalf("handler error: %v", terr)
	}
	if repos, _ := out["repos"].([]map[string]any); len(repos) != 2 {
		t.Fatalf("returned %d repos, want 2 — limit was ignored", len(repos))
	}
}

func TestCodeUpdateReposNotConfigured(t *testing.T) {
	deps := &Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, terr := handleCodeUpdateRepos(context.Background(), deps, cuInvocation(), map[string]any{})
	if terr == nil || terr.Code != CodeNotConfigured {
		t.Fatalf("err = %v, want not_configured", terr)
	}
}

// ---------------------------------------------------------------------------
// code_update_start
// ---------------------------------------------------------------------------

func TestCodeUpdateStartEnqueuesWithDefaults(t *testing.T) {
	g, q := &cuGhost{body: cuRepoListing}, &cuSQS{}
	out, terr := handleCodeUpdateStart(context.Background(), cuDeps(g, q), cuInvocation(), startArgs(nil))
	if terr != nil {
		t.Fatalf("handler error: %v", terr)
	}
	if out["status"] != codeupdate.StatusQueued {
		t.Errorf("status = %v, want queued", out["status"])
	}

	req := decodeQueued(t, q)
	if req.Version != codeupdate.QueueMessageVersion {
		t.Errorf("version = %d, want %d", req.Version, codeupdate.QueueMessageVersion)
	}
	if req.Node != codeupdate.DefaultNode {
		t.Errorf("node = %q, want the office PC default %q", req.Node, codeupdate.DefaultNode)
	}
	if req.CLI != codeupdate.DefaultCLI {
		t.Errorf("cli = %q, want %q", req.CLI, codeupdate.DefaultCLI)
	}
	if req.UserID != "user-1" {
		t.Errorf("userId = %q — it must come from verified claims, not the body", req.UserID)
	}
	if req.RequestID == "" || req.RequestedAt == "" {
		t.Error("the queued request is missing its id or timestamp")
	}
}

// Owner decision 2026-07-31: the CODEUPD# row keeps what was asked for. The row
// is written before the queue message, and it must carry the SAME trimmed text
// the message does — a row that disagrees with the request it describes is worse
// than no row, because it would be believed during an incident.
func TestStartRecordsWhatTheOwnerAskedFor(t *testing.T) {
	q, db := &cuSQS{}, &cuDDB{}
	deps := cuDepsWithStore(&cuGhost{body: cuRepoListing}, q, db)

	if _, err := handleCodeUpdateStart(context.Background(), deps, cuInvocation(),
		startArgs(map[string]any{"instructions": "  tighten the retry logic on the Bedrock client  "})); err != nil {
		t.Fatalf("code_update_start: %v", err)
	}
	if len(db.puts) != 1 {
		t.Fatalf("wrote %d rows, want 1", len(db.puts))
	}
	got, ok := db.puts[0]["instructions"].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		t.Fatal("the row does not carry the owner's instructions")
	}
	want := decodeQueued(t, q).Instructions
	if got.Value != want {
		t.Errorf("row instructions = %q, queued instructions = %q — they must not diverge",
			got.Value, want)
	}
	if got.Value != "tighten the retry logic on the Bedrock client" {
		t.Errorf("instructions = %q, want the trimmed spoken text", got.Value)
	}
	// Bounded by the row's own TTL, which is the whole privacy argument.
	if _, ok := db.puts[0]["ttl"].(*ddbtypes.AttributeValueMemberN); !ok {
		t.Error("the row carrying the owner's words has no ttl")
	}
}

// "use opus to pre-process the prompt, unless told not to": an ABSENT argument
// must read as true, not as Go's zero value.
func TestPreprocessDefaultsOnAndCanBeTurnedOff(t *testing.T) {
	cases := map[string]struct {
		args map[string]any
		want bool
	}{
		"absent":         {startArgs(nil), true},
		"explicit true":  {startArgs(map[string]any{"preprocess": true}), true},
		"explicit false": {startArgs(map[string]any{"preprocess": false}), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g, q := &cuGhost{body: cuRepoListing}, &cuSQS{}
			if _, terr := handleCodeUpdateStart(context.Background(), cuDeps(g, q), cuInvocation(), tc.args); terr != nil {
				t.Fatalf("handler error: %v", terr)
			}
			if got := decodeQueued(t, q).Preprocess; got != tc.want {
				t.Errorf("preprocess = %v, want %v", got, tc.want)
			}
		})
	}
}

// Deploy defaults ON (owner decision 2026-08-01): work the owner already
// confirmed is expected to ship, so an absent argument must read as true rather
// than as the zero value. Holding a change is the opt-OUT, and it must still be
// honoured exactly when it is asked for — a "don't push" that silently deployed
// would be far worse than the reverse.
func TestDeployDefaultsOnAndHonoursAnExplicitOptOut(t *testing.T) {
	cases := map[string]struct {
		args map[string]any
		want bool
	}{
		"absent":         {startArgs(nil), true},
		"explicit false": {startArgs(map[string]any{"deploy": false}), false},
		"explicit true":  {startArgs(map[string]any{"deploy": true}), true},
		// A non-boolean must not be read as an opt-out by accident; it falls
		// back to the default, same as the preprocess flag above.
		"wrong type": {startArgs(map[string]any{"deploy": "no"}), true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g, q := &cuGhost{body: cuRepoListing}, &cuSQS{}
			if _, terr := handleCodeUpdateStart(context.Background(), cuDeps(g, q), cuInvocation(), tc.args); terr != nil {
				t.Fatalf("handler error: %v", terr)
			}
			if got := decodeQueued(t, q).Deploy; got != tc.want {
				t.Errorf("deploy = %v, want %v", got, tc.want)
			}
		})
	}
}

// Starting a coding agent on the owner's machine is not something to infer from
// an ambiguous sentence.
func TestStartRequiresConfirmation(t *testing.T) {
	for _, args := range []map[string]any{
		startArgs(map[string]any{"confirm": false}),
		{"repo": "JeremyProffittOrg/live-ninja", "instructions": "tighten the retry logic"},
	} {
		g, q := &cuGhost{body: cuRepoListing}, &cuSQS{}
		_, terr := handleCodeUpdateStart(context.Background(), cuDeps(g, q), cuInvocation(), args)
		if terr == nil || terr.Code != CodeConfirmationRequired {
			t.Fatalf("err = %v, want confirmation_required", terr)
		}
		if len(q.bodies) != 0 {
			t.Error("an unconfirmed request was enqueued")
		}
	}
}

// A model-invented repo must never reach a launch — and an almost-right name
// should come back as candidates the model can read out.
func TestStartRejectsUnknownRepoWithCandidates(t *testing.T) {
	g, q := &cuGhost{body: cuRepoListing}, &cuSQS{}
	_, terr := handleCodeUpdateStart(context.Background(), cuDeps(g, q), cuInvocation(),
		startArgs(map[string]any{"repo": "JeremyProffittOrg/ghost"}))
	if terr == nil || terr.Code != CodeNotFound {
		t.Fatalf("err = %v, want not_found", terr)
	}
	if !strings.Contains(terr.Message, "ghost-cli") {
		t.Errorf("the error does not offer candidates: %q", terr.Message)
	}
	if len(q.bodies) != 0 {
		t.Error("an unknown repo was enqueued")
	}
}

func TestStartRejectsFabricatedRepo(t *testing.T) {
	g, q := &cuGhost{body: cuRepoListing}, &cuSQS{}
	_, terr := handleCodeUpdateStart(context.Background(), cuDeps(g, q), cuInvocation(),
		startArgs(map[string]any{"repo": "someone-else/private-thing"}))
	if terr == nil || terr.Code != CodeNotFound {
		t.Fatalf("err = %v, want not_found", terr)
	}
	if len(q.bodies) != 0 {
		t.Error("a fabricated repo was enqueued")
	}
}

// ghost-cli accepts grok/opencode/antigravity; this voice surface does not, and
// the extra ones must not slip through.
func TestStartRejectsUnsupportedAgent(t *testing.T) {
	g, q := &cuGhost{body: cuRepoListing}, &cuSQS{}
	_, terr := handleCodeUpdateStart(context.Background(), cuDeps(g, q), cuInvocation(),
		startArgs(map[string]any{"agent": "grok"}))
	if terr == nil || terr.Code != CodeInvalidArgs {
		t.Fatalf("err = %v, want invalid_args", terr)
	}
	if len(q.bodies) != 0 {
		t.Error("an unsupported agent was enqueued")
	}
}

func TestStartAcceptsCodex(t *testing.T) {
	g, q := &cuGhost{body: cuRepoListing}, &cuSQS{}
	if _, terr := handleCodeUpdateStart(context.Background(), cuDeps(g, q), cuInvocation(),
		startArgs(map[string]any{"agent": "codex"})); terr != nil {
		t.Fatalf("handler error: %v", terr)
	}
	if got := decodeQueued(t, q).CLI; got != "codex" {
		t.Errorf("cli = %q, want codex", got)
	}
}

// A denied principal must read as forbidden, not as a generic upstream blip —
// the wording is what tells the owner the allowlist needs seeding.
func TestStartSurfacesAuthorizationFailure(t *testing.T) {
	g, q := &cuGhost{status: 403, body: `{"error":"forbidden"}`}, &cuSQS{}
	_, terr := handleCodeUpdateStart(context.Background(), cuDeps(g, q), cuInvocation(), startArgs(nil))
	if terr == nil || terr.Code != CodeForbidden {
		t.Fatalf("err = %v, want forbidden", terr)
	}
}

func TestStartNotConfiguredWithoutQueue(t *testing.T) {
	g := &cuGhost{body: cuRepoListing}
	deps := cuDeps(g, &cuSQS{})
	deps.CodeUpdateQueueURL = ""
	_, terr := handleCodeUpdateStart(context.Background(), deps, cuInvocation(), startArgs(nil))
	if terr == nil || terr.Code != CodeNotConfigured {
		t.Fatalf("err = %v, want not_configured", terr)
	}
}

// The tool must be marked side-effecting so the router demands an idempotency
// key and guards it with an IDEMP# claim — a duplicate delivery must not start
// two coding sessions.
func TestStartIsSideEffecting(t *testing.T) {
	if !codeUpdateStartDefinition().SideEffecting {
		t.Fatal("code_update_start is not marked SideEffecting; a retry could launch twice")
	}
	for _, def := range []*Definition{codeUpdateReposDefinition(), codeUpdateStatusDefinition()} {
		if def.SideEffecting {
			t.Errorf("%s is marked SideEffecting but only reads", def.Name)
		}
	}
}

// None of the three may be device-local: they all execute server-side.
func TestCodeUpdateToolsAreServerExecuted(t *testing.T) {
	for _, def := range []*Definition{
		codeUpdateReposDefinition(), codeUpdateStartDefinition(), codeUpdateStatusDefinition(),
	} {
		if def.DeviceLocal {
			t.Errorf("%s is marked DeviceLocal", def.Name)
		}
		if len(def.Surfaces) != 0 {
			t.Errorf("%s restricts surfaces; it is a server tool and should be available everywhere", def.Name)
		}
	}
}

// The three tools must actually be in the catalog — a definition nobody
// registers is invisible to every model.
func TestCodeUpdateToolsAreRegistered(t *testing.T) {
	names := map[string]bool{}
	for _, def := range definitions() {
		names[def.Name] = true
	}
	for _, want := range []string{"code_update_repos", "code_update_start", "code_update_status"} {
		if !names[want] {
			t.Errorf("%s is not in definitions()", want)
		}
	}
}
