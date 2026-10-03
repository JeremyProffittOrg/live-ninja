package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
)

type jobsAccessCounter struct {
	jobs.Store
	reads, writes int
}

func (s *jobsAccessCounter) Get(ctx context.Context, uid, id string) (*jobs.Record, error) {
	s.reads++
	return s.Store.Get(ctx, uid, id)
}
func (s *jobsAccessCounter) List(ctx context.Context, uid string, limit int, cursor string) ([]jobs.Record, string, error) {
	s.reads++
	return s.Store.List(ctx, uid, limit, cursor)
}
func (s *jobsAccessCounter) CompareAndSwap(ctx context.Context, record *jobs.Record, version int64) error {
	s.writes++
	return s.Store.CompareAndSwap(ctx, record, version)
}

func jobsCapabilityApp(t *testing.T, surface, scope string, fake *fakeBrokerLambda) (*fiber.App, *jobsAccessCounter, *int, string) {
	t.Helper()
	fs, err := jobs.NewFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = fs.Close() })
	counted := &jobsAccessCounter{Store: fs}
	svc := jobs.NewService(counted)
	job, err := svc.Create(context.Background(), "alice", jobs.Input{Title: "Private saved review", Kind: "review", Instructions: "Private original text"}, "fixture-create")
	require.NoError(t, err)
	counted.reads, counted.writes = 0, 0
	dyn := testutil.NewFakeDynamo()
	st := store.NewWithClient(dyn, "test-table")
	authCalls := new(int)
	registry, err := tools.NewRegistry(&tools.Deps{Store: st, DDB: dyn, TableName: "test-table", Log: testLogger(), Now: time.Now, Jobs: svc, Reauthorize: func(context.Context, string) error { *authCalls++; return nil }})
	require.NoError(t, err)
	deps := &Deps{Store: st, Log: testLogger(), BrokerFn: "fake-broker", Lambda: fake}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "alice")
		c.Locals(localSessionID, "verified-original-session")
		c.Locals(localSurface, surface)
		c.Locals(localScope, scope)
		c.Locals(localRole, store.RoleOwner)
		return c.Next()
	})
	app.Post("/invoke", handleToolsInvoke(deps, registry))
	app.Post("/fallback", handleFallbackTurn(deps, registry))
	return app, counted, authCalls, job.ID
}

func capabilityJSON(t *testing.T, app *fiber.App, path, caps string, value any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if caps != "" {
		req.Header.Set("X-LN-Capabilities", caps)
	}
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return resp.StatusCode, out
}

func TestLegacyJobsToolsDeniedBeforeAnyReadOrProposal(t *testing.T) {
	app, counted, authCalls, _ := jobsCapabilityApp(t, "android", "", nil)
	for _, name := range []string{"job_list", "job_status", "job_create", "job_start", "job_pause", "job_resume", "job_cancel", "job_retry", "job_command", "job_future_tool"} {
		t.Run(name, func(t *testing.T) {
			status, out := capabilityJSON(t, app, "/invoke", "azure-direct,voice-live-direct", map[string]any{"tool": name, "callId": "cached-call", "args": map[string]any{"confirm": true}})
			require.Equal(t, http.StatusUpgradeRequired, status)
			require.Equal(t, "client_upgrade_required", out["error"].(map[string]any)["code"])
			require.NotContains(t, out, "output")
			require.NotContains(t, out["error"], "details")
			require.Zero(t, counted.reads)
			require.Zero(t, counted.writes)
			require.Zero(t, *authCalls, "must reject before registry or proposal execution")
		})
	}
}

func TestJobsCapabilityRequiresExactTokenAndTrustedSurface(t *testing.T) {
	for _, tc := range []struct{ name, surface, scope, caps string }{
		{"wrong token", "web", "", "jobs-review-v10"},
		{"wrong case", "web", "", "JOBS-REVIEW-V1"},
		{"oversize", "web", "", strings.Repeat("x", 2048) + ",jobs-review-v1"},
		{"device", "m5stack", "", "jobs-review-v1"},
		{"scoped bridge", "web", "nova", "jobs-review-v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, counted, _, _ := jobsCapabilityApp(t, tc.surface, tc.scope, nil)
			status, out := capabilityJSON(t, app, "/invoke", tc.caps, map[string]any{"tool": "job_list"})
			require.Equal(t, http.StatusUpgradeRequired, status, out)
			require.Zero(t, counted.reads)
			require.Zero(t, counted.writes)
		})
	}
}

func TestCapableJobsClientsReadAndProposeThroughRegistry(t *testing.T) {
	for _, surface := range []string{"web", "android"} {
		t.Run(surface, func(t *testing.T) {
			app, counted, authCalls, id := jobsCapabilityApp(t, surface, "", nil)
			const caps = "azure-direct, jobs-review-v1,voice-live-direct"
			status, out := capabilityJSON(t, app, "/invoke", caps, map[string]any{"tool": "job_list", "callId": "list"})
			require.Equal(t, http.StatusOK, status, out)
			require.Len(t, out["output"].(map[string]any)["jobs"], 1)
			status, out = capabilityJSON(t, app, "/invoke", caps, map[string]any{"tool": "job_start", "callId": "start", "args": map[string]any{"jobId": id}})
			require.Equal(t, http.StatusBadRequest, status, out)
			require.Equal(t, "confirmation_required", out["error"].(map[string]any)["code"])
			require.GreaterOrEqual(t, counted.reads, 2)
			require.Zero(t, counted.writes, "a voice proposal must not execute")
			require.Equal(t, 2, *authCalls, "compatibility does not replace authorization")
		})
	}
}

func TestJobsFallbackCapabilityParityAndCachedCallDenial(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "legacy"
		caps := "azure-direct"
		if enabled {
			name, caps = "updated", "azure-direct,jobs-review-v1"
		}
		t.Run(name, func(t *testing.T) {
			fake := &fakeBrokerLambda{}
			app, counted, authCalls, id := jobsCapabilityApp(t, "web", "", fake)
			args, _ := json.Marshal(map[string]string{"jobId": id})
			fake.responses = []map[string]any{{"toolCalls": []map[string]any{{"id": "read", "name": "job_list", "arguments": "{}"}, {"id": "proposal", "name": "job_start", "arguments": string(args)}}}, {"text": "Tool results received."}}
			status, out := capabilityJSON(t, app, "/fallback", caps, map[string]any{"text": "Show my jobs then propose starting the review"})
			require.Equal(t, http.StatusOK, status, out)
			calls := respToolCalls(t, out)
			require.Len(t, calls, 2)
			require.Len(t, fake.requests, 2)
			for _, req := range fake.requests {
				require.Equal(t, parseClientCapabilities(caps), req.Capabilities)
			}
			if enabled {
				require.Equal(t, true, calls[0]["ok"])
				require.Equal(t, "confirmation_required", calls[1]["error"].(map[string]any)["code"])
				require.GreaterOrEqual(t, counted.reads, 2)
				require.Equal(t, 2, *authCalls)
			} else {
				for _, call := range calls {
					require.Equal(t, "client_upgrade_required", call["error"].(map[string]any)["code"])
					require.NotContains(t, call, "output")
					require.NotContains(t, call["error"], "details")
				}
				require.Zero(t, counted.reads)
				require.Zero(t, *authCalls)
			}
			require.Zero(t, counted.writes)
		})
	}
}
