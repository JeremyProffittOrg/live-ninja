package realtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

func TestPinToEngine(t *testing.T) {
	cases := []struct {
		name     string
		def      string
		devices  map[string]string
		deviceID string
		want     voiceengine.Engine
	}{
		{
			name: "empty everything falls back to openai-realtime",
			want: voiceengine.EngineOpenAIRealtime,
		},
		{
			name: "account default applies when no device pin",
			def:  "openai-realtime-mini",
			want: voiceengine.EngineOpenAIRealtimeMini,
		},
		{
			name:     "device pin overrides the default",
			def:      "openai-realtime",
			devices:  map[string]string{"dev-1": "nova-sonic"},
			deviceID: "dev-1",
			want:     voiceengine.EngineNovaSonic,
		},
		{
			name:     "unpinned device gets the default, not another device's pin",
			def:      "openai-realtime",
			devices:  map[string]string{"dev-1": "nova-sonic"},
			deviceID: "dev-2",
			want:     voiceengine.EngineOpenAIRealtime,
		},
		{
			name:     "empty deviceID (browser session) always gets the default",
			def:      "openai-realtime-mini",
			devices:  map[string]string{"dev-1": "nova-sonic"},
			deviceID: "",
			want:     voiceengine.EngineOpenAIRealtimeMini,
		},
		{
			name:     "unknown device pin falls through to the default",
			def:      "openai-realtime",
			devices:  map[string]string{"dev-1": "gpt-9-ultra"},
			deviceID: "dev-1",
			want:     voiceengine.EngineOpenAIRealtime,
		},
		{
			name:     "unknown default with unknown device pin falls to hard default",
			def:      "bogus",
			devices:  map[string]string{"dev-1": "also-bogus"},
			deviceID: "dev-1",
			want:     voiceengine.EngineOpenAIRealtime,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PinToEngine(tc.def, tc.devices, tc.deviceID); got != tc.want {
				t.Fatalf("PinToEngine(%q,%v,%q) = %q, want %q", tc.def, tc.devices, tc.deviceID, got, tc.want)
			}
		})
	}
}

// fakeSettingsGetter is a scripted SettingsGetter for ResolveEngine tests.
type fakeSettingsGetter struct {
	item map[string]ddbtypes.AttributeValue
	err  error
	// captured
	gotProjection string
}

func (f *fakeSettingsGetter) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if in.ProjectionExpression != nil {
		f.gotProjection = *in.ProjectionExpression
	}
	if f.err != nil {
		return nil, f.err
	}
	return &dynamodb.GetItemOutput{Item: f.item}, nil
}

// settingsItem builds the marshaled DynamoDB item for a settings doc with the
// given voiceEngine sub-document.
func settingsItem(t *testing.T, def string, devices map[string]string) map[string]ddbtypes.AttributeValue {
	t.Helper()
	doc := map[string]any{
		"pk":    "USER#u1",
		"sk":    "SETTINGS",
		"voice": "cedar",
		"voiceEngine": map[string]any{
			"default": def,
			"devices": devices,
		},
	}
	item, err := attributevalue.MarshalMap(doc)
	if err != nil {
		t.Fatalf("marshal settings item: %v", err)
	}
	return item
}

func TestResolveEngine(t *testing.T) {
	ctx := context.Background()

	t.Run("nova pin for the device", func(t *testing.T) {
		g := &fakeSettingsGetter{item: settingsItem(t, "openai-realtime", map[string]string{"dev-1": "nova-sonic"})}
		got, err := ResolveEngine(ctx, g, "tbl", "u1", "dev-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != voiceengine.EngineNovaSonic {
			t.Fatalf("got %q, want nova-sonic", got)
		}
		if g.gotProjection != "voiceEngine, deviceOverrides" {
			t.Fatalf("expected a projected read of voiceEngine + overrides, got %q", g.gotProjection)
		}
	})

	t.Run("default engine for an unpinned device", func(t *testing.T) {
		g := &fakeSettingsGetter{item: settingsItem(t, "openai-realtime-mini", map[string]string{"dev-1": "nova-sonic"})}
		got, err := ResolveEngine(ctx, g, "tbl", "u1", "dev-2")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != voiceengine.EngineOpenAIRealtimeMini {
			t.Fatalf("got %q, want openai-realtime-mini", got)
		}
	})

	t.Run("section override outranks a legacy pin", func(t *testing.T) {
		item := settingsItem(t, "openai-realtime", map[string]string{"dev-1": "nova-sonic"})
		overrides, err := attributevalue.Marshal(map[string]any{
			"dev-1": map[string]any{
				"sections": map[string]any{
					"voiceEngine": map[string]any{
						"voiceEngine": map[string]any{
							"default": "gemini-flash-live",
							"devices": map[string]any{},
						},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("marshal overrides: %v", err)
		}
		item["deviceOverrides"] = overrides
		g := &fakeSettingsGetter{item: item}
		got, err := ResolveEngine(ctx, g, "tbl", "u1", "dev-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != voiceengine.EngineGeminiFlashLive {
			t.Fatalf("got %q, want gemini-flash-live", got)
		}
	})

	t.Run("no settings document falls back to openai-realtime with no error", func(t *testing.T) {
		g := &fakeSettingsGetter{item: nil}
		got, err := ResolveEngine(ctx, g, "tbl", "u1", "dev-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != voiceengine.EngineOpenAIRealtime {
			t.Fatalf("got %q, want openai-realtime", got)
		}
	})

	t.Run("nil getter is treated as unconfigured default", func(t *testing.T) {
		got, err := ResolveEngine(ctx, nil, "tbl", "u1", "dev-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != voiceengine.EngineOpenAIRealtime {
			t.Fatalf("got %q, want openai-realtime", got)
		}
	})

	t.Run("read error fails open to the default and surfaces the error", func(t *testing.T) {
		g := &fakeSettingsGetter{err: errors.New("dynamo down")}
		got, err := ResolveEngine(ctx, g, "tbl", "u1", "dev-1")
		if err == nil {
			t.Fatalf("expected an error to be surfaced")
		}
		if got != voiceengine.EngineOpenAIRealtime {
			t.Fatalf("must fail open to openai-realtime, got %q", got)
		}
	})
}

func TestBuildBridgeSession(t *testing.T) {
	ctx := context.Background()
	fixedExp := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	var gotUser, gotDevice, gotSurface, gotSession, gotConfigDigest string
	mint := func(_ context.Context, userID, deviceID, surface, sessionID, configDigest string) (string, time.Time, error) {
		gotUser, gotDevice, gotSurface, gotSession = userID, deviceID, surface, sessionID
		gotConfigDigest = configDigest
		return "tok.en.jwt", fixedExp, nil
	}

	bs, err := BuildBridgeSession(ctx, mint, "", "u1", "dev-1", "device", "sess-abc", "sha256-config")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotUser != "u1" || gotDevice != "dev-1" || gotSurface != "device" || gotSession != "sess-abc" {
		t.Fatalf("minter got wrong args: %q %q %q %q", gotUser, gotDevice, gotSurface, gotSession)
	}
	if gotConfigDigest != "sha256-config" {
		t.Fatalf("minter config digest = %q, want sha256-config", gotConfigDigest)
	}
	if bs.Token != "tok.en.jwt" || !bs.ExpiresAt.Equal(fixedExp) {
		t.Fatalf("bridge session token/expiry wrong: %+v", bs)
	}
	// Default base URL applied, session id + token embedded as query params.
	if !strings.HasPrefix(bs.WSURL, DefaultBridgeBaseURL+"/session?") {
		t.Fatalf("wsURL should start with the default bridge base + /session?, got %q", bs.WSURL)
	}
	if !strings.Contains(bs.WSURL, "sid=sess-abc") || !strings.Contains(bs.WSURL, "token=tok.en.jwt") {
		t.Fatalf("wsURL missing sid/token query params: %q", bs.WSURL)
	}
}

func TestBuildBridgeSession_CustomBaseAndErrors(t *testing.T) {
	ctx := context.Background()

	// Nil minter -> configuration error.
	if _, err := BuildBridgeSession(ctx, nil, "", "u1", "d1", "web", "s1", "cfg"); err == nil {
		t.Fatalf("expected error for nil minter")
	}
	if _, err := BuildBridgeSession(ctx, func(_ context.Context, _, _, _, _, _ string) (string, time.Time, error) {
		return "t", time.Now(), nil
	}, "", "u1", "d1", "web", "s1", ""); err == nil {
		t.Fatalf("expected error for empty config digest")
	}

	// Minter error is wrapped.
	failing := func(_ context.Context, _, _, _, _, _ string) (string, time.Time, error) {
		return "", time.Time{}, errors.New("kms down")
	}
	if _, err := BuildBridgeSession(ctx, failing, "wss://custom.example", "u1", "d1", "web", "s1", "cfg"); err == nil {
		t.Fatalf("expected error from failing minter")
	}

	// Custom base URL with a trailing slash is normalized (no double slash).
	ok := func(_ context.Context, _, _, _, _, _ string) (string, time.Time, error) {
		return "t", time.Now(), nil
	}
	bs, err := BuildBridgeSession(ctx, ok, "wss://custom.example/", "u1", "d1", "web", "s1", "cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(bs.WSURL, "wss://custom.example/session?") {
		t.Fatalf("custom base URL not normalized: %q", bs.WSURL)
	}
}

// TestPinToEngineAcceptsAzureEngines pins the four Azure engines added by
// azure-voice-plan.md WS-B M2. It exists because a pin validEngine rejects is
// treated as ABSENT and silently falls through to openai-realtime (R2), so a
// missing case here is not a build error or a test failure anywhere else — it
// is a user whose chosen engine quietly stops being honoured.
func TestPinToEngineAcceptsAzureEngines(t *testing.T) {
	azure := []voiceengine.Engine{
		voiceengine.EngineGPTLiveAzure,
		voiceengine.EngineGPTLiveAzureMini,
		voiceengine.EngineAzureVoiceLive,
		voiceengine.EngineAzureVoiceLiveLite,
	}

	for _, want := range azure {
		if got := PinToEngine(string(want), nil, ""); got != want {
			t.Errorf("PinToEngine(%q, nil, \"\") = %q, want %q — an Azure pin fell through to the default", want, got, want)
		}
		// The deprecated per-device map must accept them too (R4): the two
		// paths are validated separately and can drift.
		devices := map[string]string{"dev-1": string(want)}
		if got := PinToEngine("openai-realtime", devices, "dev-1"); got != want {
			t.Errorf("PinToEngine(default, devices[dev-1]=%q, \"dev-1\") = %q, want %q", want, got, want)
		}
	}

	// Every engine the product ships must survive a round trip through the
	// pin resolver, so adding a constant to voiceengine.All without teaching
	// validEngine about it fails here rather than in production.
	for _, e := range voiceengine.All {
		if got := PinToEngine(string(e), nil, ""); got != e {
			t.Errorf("voiceengine.All contains %q but PinToEngine resolved it to %q", e, got)
		}
	}

	// An unknown pin must still fall through to the platform default.
	if got := PinToEngine("gpt-live-mars", nil, ""); got != voiceengine.EngineOpenAIRealtime {
		t.Errorf("unknown pin resolved to %q, want openai-realtime", got)
	}
}
