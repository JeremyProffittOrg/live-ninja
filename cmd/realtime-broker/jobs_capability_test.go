package main

import (
	"context"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/stretchr/testify/require"
)

type capabilityMinter struct{ fakeRealtimeMint }

func (m *capabilityMinter) Mint(ctx context.Context, _, _, _, _, surface string) (*realtime.MintResult, error) {
	return &realtime.MintResult{ToolManifest: realtime.ToolManifestJSONForClient(ctx, surface, false)}, nil
}

type capabilityFallback struct {
	fakeFallback
	manifest string
}

func (f *capabilityFallback) TurnWithToolsForSurface(ctx context.Context, _, surface string, _ []realtime.ChatMessage, _ string) (*realtime.TurnResult, error) {
	f.manifest = string(realtime.ToolManifestJSONForClient(ctx, surface, true))
	return &realtime.TurnResult{Text: "ok"}, nil
}

func TestBrokerPropagatesJobsCapabilityToMintAndFallback(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "legacy"
		var caps []string
		if enabled {
			name, caps = "updated", []string{realtime.JobsReviewCapability}
		}
		t.Run(name, func(t *testing.T) {
			ddb := testutil.NewFakeDynamo()
			b := newGeminiTestBroker(ddb, nil)
			b.minter = &capabilityMinter{}
			resp, err := b.Handle(context.Background(), Request{UserID: "alice", Surface: "web", Capabilities: caps})
			require.NoError(t, err)
			require.Empty(t, resp.Error, resp.Message)
			fb := &capabilityFallback{}
			b.fallback = fb
			req := turnRequest(t, map[string]any{"messages": []map[string]string{{"role": "user", "content": "hello"}}})
			req.Capabilities = caps
			result, err := b.Handle(context.Background(), req)
			require.NoError(t, err)
			require.Empty(t, result.Error, result.Message)
			for _, manifest := range []string{string(resp.ToolManifest), fb.manifest} {
				if enabled {
					require.Contains(t, manifest, `"job_list"`)
				} else {
					require.NotContains(t, manifest, `"job_list"`)
				}
				require.Contains(t, manifest, `"get_weather"`)
			}
		})
	}
}
