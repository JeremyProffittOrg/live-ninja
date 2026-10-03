package tools

import (
	"bytes"
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
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/stretchr/testify/require"
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
	lookup map[string]ddbtypes.AttributeValue
	puts   []map[string]ddbtypes.AttributeValue
}

func (f *cuDDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.puts = append(f.puts, in.Item)
	return &dynamodb.PutItemOutput{}, nil
}

func (f *cuDDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	// Only answer for the requested user partition and exact record.
	if f.lookup != nil {
		for _, key := range []string{"pk", "sk"} {
			if in.Key[key].(*ddbtypes.AttributeValueMemberS).Value != f.lookup[key].(*ddbtypes.AttributeValueMemberS).Value {
				return &dynamodb.GetItemOutput{}, nil
			}
		}
	}
	return &dynamodb.GetItemOutput{Item: f.lookup}, nil
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

// The model cannot authorize itself to start a coding agent, including when
// every integration is configured and it supplies a valid owner's context.
func TestCodeUpdateProposalCannotLaunchWithAnyModelConfirmation(t *testing.T) {
	for confirmName, confirm := range map[string]any{"omitted": nil, "true": true, "false": false} {
		for deployName, deploy := range map[string]any{"omitted": nil, "true": true, "false": false} {
			t.Run("confirm-"+confirmName+"/deploy-"+deployName, func(t *testing.T) {
				deps, fake := newTestDepsWithFake()
				g, q, db := &cuGhost{body: cuRepoListing}, &cuSQS{}, &cuDDB{}
				remote := cuDepsWithStore(g, q, db)
				deps.Ghost, deps.SQS, deps.CodeUpdate = remote.Ghost, remote.SQS, remote.CodeUpdate
				deps.CodeUpdateQueueURL = remote.CodeUpdateQueueURL
				args := startArgs(nil)
				delete(args, "confirm")
				if confirm != nil {
					args["confirm"] = confirm
				}
				if deploy != nil {
					args["deploy"] = deploy
				}
				inv := invocation("code_update_start", args)
				inv.Role = store.RoleOwner
				inv.IdempotencyKey = "same-proposal"
				registry := newTestRegistry(t, deps)
				for range 2 {
					res := registry.Invoke(context.Background(), inv)
					require.False(t, res.OK)
					require.False(t, res.Duplicate)
					require.Equal(t, CodeConfirmationRequired, res.Error.Code)
					require.Contains(t, res.Error.Message, "No coding job was queued or started")
					require.Contains(t, res.Error.Message, "not connected yet")
					require.Equal(t, false, res.Error.Details["executionAvailable"])
					require.Equal(t, "not_connected", res.Error.Details["launchApproval"])
					proposed := res.Error.Details["proposed"].(map[string]any)
					require.Equal(t, false, proposed["deploy"])
					require.NotContains(t, res.Error.Details, "requestId")
					require.NotContains(t, res.Error.Details, "runId")
				}
				require.Zero(t, g.calls, "proposal must not contact the fleet")
				require.Empty(t, q.bodies, "proposal must not enqueue execution")
				require.Empty(t, db.puts, "proposal must not create a coding record")
				require.Nil(t, fake.RawItem("IDEMP#user-1#same-proposal", "IDEMP"))
			})
		}
	}
}

func TestCodeUpdateProposalCarriesExactReviewFieldsWithoutDiagnosticLeak(t *testing.T) {
	deps := newTestDeps()
	var diagnostics bytes.Buffer
	deps.Log = slog.New(slog.NewTextHandler(&diagnostics, nil))
	inv := invocation("code_update_start", startArgs(map[string]any{
		"repo": "  some-owner/unverified-repo  ", "node": "  REVIEWPC  ",
		"instructions": "  Private change request.\nPreserve this second line.  ",
		"agent":        "codex", "model": " proposed-model ", "effort": " high ", "preprocess": false,
	}))
	inv.Role = store.RoleOwner
	res := newTestRegistry(t, deps).Invoke(context.Background(), inv)
	require.False(t, res.OK)
	require.Equal(t, CodeConfirmationRequired, res.Error.Code)
	require.Equal(t, false, res.Error.Details["repositoryVerified"])
	require.Equal(t, false, res.Error.Details["nodeVerified"])
	require.Equal(t, map[string]any{
		"repo": "some-owner/unverified-repo", "node": "REVIEWPC",
		"instructions": "Private change request.\nPreserve this second line.",
		"agent":        "codex", "model": "proposed-model", "effort": "high", "preprocess": false, "deploy": false,
	}, res.Error.Details["proposed"])
	require.NotContains(t, res.Error.Error(), "Private change request")
	require.NotContains(t, diagnostics.String(), "Private change request")
}

func TestCodeUpdateProposalDefaultsWithoutAnyIntegrationAccess(t *testing.T) {
	_, terr := handleCodeUpdateStart(context.Background(), nil, cuInvocation(), startArgs(nil))
	require.NotNil(t, terr)
	require.Equal(t, CodeConfirmationRequired, terr.Code)
	proposed := terr.Details["proposed"].(map[string]any)
	require.Equal(t, codeupdate.DefaultNode, proposed["node"])
	require.Equal(t, codeupdate.DefaultCLI, proposed["agent"])
	require.Equal(t, true, proposed["preprocess"])
	require.Equal(t, false, proposed["deploy"])
}

func TestCodeUpdateProposalRejectsInvalidArgumentsWithoutIntegrationAccess(t *testing.T) {
	for name, overrides := range map[string]map[string]any{
		"unsupported CLI":   {"agent": "grok"},
		"invalid repo":      {"repo": "unqualified-repository"},
		"blank brief":       {"instructions": strings.Repeat(" ", 20)},
		"malformed deploy":  {"deploy": "yes"},
		"invented approval": {"approvalToken": "model-created-value"},
	} {
		t.Run(name, func(t *testing.T) {
			deps := newTestDeps()
			inv := invocation("code_update_start", startArgs(overrides))
			inv.Role = store.RoleOwner
			res := newTestRegistry(t, deps).Invoke(context.Background(), inv)
			require.False(t, res.OK)
			require.Equal(t, CodeInvalidArgs, res.Error.Code)
		})
	}
}

// The registry must enforce ownership before any integration access or claim.
func TestCodeUpdateToolsRequireOwner(t *testing.T) {
	for _, def := range []*Definition{codeUpdateReposDefinition(), codeUpdateStartDefinition(), codeUpdateStatusDefinition()} {
		t.Run(def.Name, func(t *testing.T) {
			require.True(t, def.OwnerOnly)
			for _, role := range []string{store.RoleMember, "", "admin"} {
				deps, fake := newTestDepsWithFake()
				g, q := &cuGhost{body: cuRepoListing}, &cuSQS{}
				deps.Ghost = cuDeps(g, q).Ghost
				deps.SQS = q
				deps.CodeUpdateQueueURL = "https://sqs/code-update"
				args := map[string]any{}
				if def.Name == "code_update_start" {
					args = startArgs(nil)
				}
				inv := invocation(def.Name, args)
				inv.Role = role
				inv.IdempotencyKey = "denied"
				res := newTestRegistry(t, deps).Invoke(context.Background(), inv)
				require.False(t, res.OK)
				require.Equal(t, CodeForbidden, res.Error.Code)
				require.Zero(t, g.calls)
				require.Empty(t, q.bodies)
				require.Nil(t, fake.RawItem("IDEMP#user-1#denied", "IDEMP"))
			}
		})
	}
}

func TestCodeUpdateOwnerCanStillDiscoverRepositories(t *testing.T) {
	deps := newTestDeps()
	g, q := &cuGhost{body: cuRepoListing}, &cuSQS{}
	deps.Ghost = cuDeps(g, q).Ghost
	inv := invocation("code_update_repos", map[string]any{})
	inv.Role = store.RoleOwner
	res := newTestRegistry(t, deps).Invoke(context.Background(), inv)
	require.True(t, res.OK, "%+v", res.Error)
	require.Len(t, res.Output["repos"], 4)
	require.Equal(t, 1, g.calls)
}

func TestCodeUpdateToolsDoNotClaimMutationIdempotencyKeys(t *testing.T) {
	for _, def := range []*Definition{codeUpdateReposDefinition(), codeUpdateStartDefinition(), codeUpdateStatusDefinition()} {
		require.False(t, def.SideEffecting, "%s only reads or proposes", def.Name)
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

func TestCodeUpdateStatusPreservesLegacyDeploymentAndDoesNotInferCompletion(t *testing.T) {
	ctx := context.Background()
	db := &cuDDB{}
	deps := newTestDeps()
	deps.CodeUpdate = codeupdate.NewStore(db, "test", nil)
	require.NoError(t, deps.CodeUpdate.Put(ctx, codeupdate.Record{
		RequestID: "legacy-run", UserID: "user-1", Repo: "JeremyProffittOrg/live-ninja",
		Status: codeupdate.StatusLaunched, Deploy: true, RunID: "run-1",
	}))
	db.lookup = db.puts[0]
	out, terr := handleCodeUpdateStatus(ctx, deps, cuInvocation(), map[string]any{"requestId": "legacy-run"})
	require.Nil(t, terr)
	require.Equal(t, true, out["deploy"], "old runs must not be relabeled as review-only")
	require.Equal(t, codeupdate.StatusLaunched, out["status"])
	require.NotContains(t, out, "runStatus", "missing provider outcomes must not imply success")
	inv := cuInvocation()
	inv.UserID = "someone-else"
	_, terr = handleCodeUpdateStatus(ctx, deps, inv, map[string]any{"requestId": "legacy-run"})
	require.NotNil(t, terr)
	require.Equal(t, CodeNotFound, terr.Code)
}
