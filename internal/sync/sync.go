// Package sync implements the M6 settings fan-out to M5Stack devices via
// the AWS IoT named shadow "config" (contracts/shadow.md).
//
// Locked M6 fan-out decisions (autonomous defaults, deviating from the
// plan's original WebSocket/FCM sketch — documented deferrals, not
// omissions):
//   - No FCM: Android push would require a Firebase account (external
//     dependency). Android reconciles on foreground + its wake-service
//     15-minute tick via GET /api/v1/settings?since=<v>.
//   - No WebSocket API: cost/complexity not justified. Web polls
//     GET /api/v1/settings?since=<v> every 30s + on visibilitychange/focus.
//   - The M5 device is the ONLY real-push surface: every successful
//     settings PUT publishes the new document as the shadow `desired`
//     state to every ACTIVE, IoT-provisioned device of the user
//     (higher-version-wins reconciliation per contracts/shadow.md).
//
// The IoT data-plane endpoint is account/region specific: it is resolved
// once per process via iot:DescribeEndpoint (type iot:Data-ATS) and
// cached, with the IOT_DATA_ENDPOINT env var as an override that skips
// the control-plane call entirely (same convention as
// internal/webapp/api_routes.go's tool registry).
package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	stdsync "sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// ShadowName is the named shadow carrying device configuration
// (contracts/shadow.md).
const ShadowName = "config"

// IoTDataAPI is the subset of the IoT data-plane client Publisher uses,
// interface-typed so tests inject a fake.
type IoTDataAPI interface {
	UpdateThingShadow(ctx context.Context, params *iotdataplane.UpdateThingShadowInput, optFns ...func(*iotdataplane.Options)) (*iotdataplane.UpdateThingShadowOutput, error)
}

// EndpointAPI is the subset of the IoT control-plane client used to
// resolve the account's data-plane endpoint (iot:DescribeEndpoint).
type EndpointAPI interface {
	DescribeEndpoint(ctx context.Context, params *iot.DescribeEndpointInput, optFns ...func(*iot.Options)) (*iot.DescribeEndpointOutput, error)
}

// DeviceLister is the store subset PublishDesired needs (satisfied by
// *store.Store).
type DeviceLister interface {
	ListDevices(ctx context.Context, userID string) ([]store.Device, error)
}

// Publisher pushes settings documents to device shadows. Safe for
// concurrent use; the data-plane client is created lazily on first use
// (so users with no IoT-provisioned devices never trigger a
// DescribeEndpoint call) and cached for the process lifetime.
type Publisher struct {
	mu       stdsync.Mutex
	data     IoTDataAPI  // cached data-plane client (or test fake)
	endpoint EndpointAPI // control-plane endpoint resolver (nil in tests)
	awsCfg   aws.Config
	log      *slog.Logger
}

// New builds a production Publisher from the ambient AWS config.
func New(ctx context.Context, log *slog.Logger) (*Publisher, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: load aws config: %w", err)
	}
	return &Publisher{
		endpoint: iot.NewFromConfig(cfg),
		awsCfg:   cfg,
		log:      log,
	}, nil
}

// NewWithClient builds a Publisher around an injected data-plane client
// (test seam — no endpoint resolution happens when data is pre-set).
func NewWithClient(data IoTDataAPI, log *slog.Logger) *Publisher {
	return &Publisher{data: data, log: log}
}

// shared is the process-wide lazily-built Publisher used by the web
// function's settings PUT path (internal/webapp/settings_routes.go) —
// one endpoint resolution + client per warm container.
var (
	sharedOnce stdsync.Once
	shared     *Publisher
	sharedErr  error
)

// SharedPublisher returns the process-wide Publisher, building it on
// first call.
func SharedPublisher(ctx context.Context, log *slog.Logger) (*Publisher, error) {
	sharedOnce.Do(func() {
		shared, sharedErr = New(ctx, log)
	})
	return shared, sharedErr
}

// client returns the cached data-plane client, resolving the IoT
// endpoint on first use (IOT_DATA_ENDPOINT env override, else
// iot:DescribeEndpoint type iot:Data-ATS).
func (p *Publisher) client(ctx context.Context) (IoTDataAPI, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.data != nil {
		return p.data, nil
	}

	endpoint := strings.TrimSpace(os.Getenv("IOT_DATA_ENDPOINT"))
	if endpoint == "" {
		if p.endpoint == nil {
			return nil, errors.New("sync: no IoT data client and no endpoint resolver configured")
		}
		out, err := p.endpoint.DescribeEndpoint(ctx, &iot.DescribeEndpointInput{
			EndpointType: aws.String("iot:Data-ATS"),
		})
		if err != nil {
			return nil, fmt.Errorf("sync: describe iot endpoint: %w", err)
		}
		endpoint = aws.ToString(out.EndpointAddress)
	}
	if endpoint == "" {
		return nil, errors.New("sync: resolved an empty IoT data endpoint")
	}

	p.data = iotdataplane.NewFromConfig(p.awsCfg, func(o *iotdataplane.Options) {
		o.BaseEndpoint = aws.String("https://" + endpoint)
	})
	return p.data, nil
}

// PublishDesired publishes doc (the canonical settings document at
// `version`) as the `config` shadow desired state to every ACTIVE,
// IoT-provisioned device belonging to userID. Devices without a
// ThingName (pre-M5-provisioning) and revoked devices are skipped. A
// per-device publish failure does not stop the fan-out to the remaining
// devices; the joined error is returned for logging (callers on the
// HTTP PUT path treat it as non-fatal — the shadow's persistence plus
// the device's own boot-time `get` make convergence eventual either
// way, per contracts/shadow.md "Failure / offline handling").
func (p *Publisher) PublishDesired(ctx context.Context, devices DeviceLister, userID string, doc map[string]any, version int64) error {
	if userID == "" {
		return errors.New("sync: userID is required")
	}
	all, err := devices.ListDevices(ctx, userID)
	if err != nil {
		return fmt.Errorf("sync: list devices: %w", err)
	}
	targets := make([]store.Device, 0, len(all))
	for _, d := range all {
		if d.Status == store.DeviceStatusActive && d.ThingName != "" {
			targets = append(targets, d)
		}
	}
	return p.PublishToDevices(ctx, targets, doc, version)
}

// PublishToDevices publishes the desired state to an explicit device
// set (shadow-ingest's targeted push-down uses this with a single
// device). Devices are assumed pre-filtered (active + ThingName set).
// A no-op with zero targets — the IoT endpoint is never even resolved.
func (p *Publisher) PublishToDevices(ctx context.Context, targets []store.Device, doc map[string]any, version int64) error {
	if len(targets) == 0 {
		return nil
	}
	client, err := p.client(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, d := range targets {
		payload, err := json.Marshal(map[string]any{
			"state": map[string]any{"desired": BuildDesired(doc, d.DeviceID, version)},
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("sync: marshal desired for %s: %w", d.DeviceID, err))
			continue
		}
		if _, err := client.UpdateThingShadow(ctx, &iotdataplane.UpdateThingShadowInput{
			ThingName:  aws.String(d.ThingName),
			ShadowName: aws.String(ShadowName),
			Payload:    payload,
		}); err != nil {
			errs = append(errs, fmt.Errorf("sync: update shadow for thing %s: %w", d.ThingName, err))
			continue
		}
		if p.log != nil {
			p.log.Info("sync: published desired shadow state",
				slog.String("deviceId", d.DeviceID),
				slog.String("thingName", d.ThingName),
				slog.Int64("settingsVersion", version))
		}
	}
	return errors.Join(errs...)
}

// BuildDesired trims the canonical settings document to the
// M5Stack-relevant desired subset for one device, per contracts/
// shadow.md "Document shape". persona.* and micDeviceId are never
// shadowed; privacy.* is shadowed read-only (on-screen disclosure);
// voiceEngine resolves per-device (devices[deviceId] ?? default); the
// wakeModel url/sha256 pair is always null in desired — the device
// fills it from the wake-word manifest, the shadow only ever carries
// the wake-word ID.
func BuildDesired(doc map[string]any, deviceID string, version int64) map[string]any {
	desired := map[string]any{
		"settingsVersion": version,
		"wakeWord":        stringField(doc, "wakeWord", "hey-live-ninja"),
		// Locked M6 decision: the ESP32 catalog is builtin WakeNet models
		// only for now (custom oWW-on-ESP conversion is honestly flagged
		// unsupported), so the engine pushed to an M5Stack is always
		// "wakenet" regardless of the canonical wakeEngine — per
		// contracts/shadow.md: "M5Stack is always wakenet or an oWW-ESP
		// fallback id, never openwakeword/porcupine".
		"wakeEngine":    "wakenet",
		"sensitivity":   numberField(doc, "sensitivity", 0.5),
		"voice":         stringField(doc, "voice", "cedar"),
		"turnDetection": stringField(doc, "turnDetection", "semantic_vad"),
		"voiceEngine":   resolveVoiceEngine(doc, deviceID),
		"wakeModel":     map[string]any{"url": nil, "sha256": nil},
	}

	if privacy, ok := doc["privacy"].(map[string]any); ok {
		desired["privacy"] = map[string]any{
			"storeAudio":       boolField(privacy, "storeAudio", false),
			"storeTranscripts": boolField(privacy, "storeTranscripts", true),
			"retentionDays":    numberField(privacy, "retentionDays", 30),
		}
	}
	return desired
}

// resolveVoiceEngine resolves the engine for THIS device:
// voiceEngine.devices[deviceId] ?? voiceEngine.default (contracts/
// settings.schema.json).
func resolveVoiceEngine(doc map[string]any, deviceID string) string {
	ve, ok := doc["voiceEngine"].(map[string]any)
	if !ok {
		return "openai-realtime"
	}
	if devices, ok := ve["devices"].(map[string]any); ok && deviceID != "" {
		if pin, ok := devices[deviceID].(string); ok && pin != "" {
			return pin
		}
	}
	return stringField(ve, "default", "openai-realtime")
}

// MergeDeviceReported folds a device-initiated shadow `reported` change
// into a copy of the canonical settings document, returning the merged
// copy and whether anything actually changed. Only the fields an
// M5Stack legitimately writes are folded: wakeWord, wakeEngine,
// sensitivity, voice, turnDetection — each type/bounds-checked with the
// same rules as the HTTP PUT validation; an invalid reported value is
// ignored (the push-down republish will correct the device). privacy.*
// (read-only on device per contracts/shadow.md), voiceEngine (a
// per-device resolved scalar in the shadow — cannot round-trip into the
// canonical map shape), and reported-only bookkeeping fields
// (settingsVersion, deviceReportedAt, wakeModelSha256Applied,
// firmwareVersion) are never folded. Unknown canonical fields are
// preserved verbatim (contracts/README.md rule 2).
func MergeDeviceReported(canonical, reported map[string]any) (map[string]any, bool) {
	merged := make(map[string]any, len(canonical))
	for k, v := range canonical {
		merged[k] = v
	}
	changed := false

	if s, ok := reported["wakeWord"].(string); ok && strings.TrimSpace(s) != "" && len(s) <= 128 {
		if cur, _ := merged["wakeWord"].(string); cur != s {
			merged["wakeWord"] = s
			changed = true
		}
	}
	if s, ok := reported["wakeEngine"].(string); ok &&
		(s == "openwakeword" || s == "porcupine" || s == "wakenet") {
		if cur, _ := merged["wakeEngine"].(string); cur != s {
			merged["wakeEngine"] = s
			changed = true
		}
	}
	if n, ok := docNumber(reported["sensitivity"]); ok && n >= 0 && n <= 1 {
		if cur, curOK := docNumber(merged["sensitivity"]); !curOK || cur != n {
			merged["sensitivity"] = n
			changed = true
		}
	}
	if s, ok := reported["voice"].(string); ok && strings.TrimSpace(s) != "" && len(s) <= 64 {
		if cur, _ := merged["voice"].(string); cur != s {
			merged["voice"] = s
			changed = true
		}
	}
	if s, ok := reported["turnDetection"].(string); ok &&
		(s == "semantic_vad" || s == "server_vad") {
		if cur, _ := merged["turnDetection"].(string); cur != s {
			merged["turnDetection"] = s
			changed = true
		}
	}
	return merged, changed
}

// DocVersion extracts the settings document's integer version (0 when
// absent/malformed), tolerating every numeric shape that reaches a
// map[string]any from encoding/json or attributevalue.
func DocVersion(doc map[string]any) int64 {
	if n, ok := docNumber(doc["version"]); ok {
		return int64(n)
	}
	return 0
}

// ---- small typed accessors over map[string]any ----

func stringField(m map[string]any, key, def string) string {
	if s, ok := m[key].(string); ok && s != "" {
		return s
	}
	return def
}

func boolField(m map[string]any, key string, def bool) bool {
	if b, ok := m[key].(bool); ok {
		return b
	}
	return def
}

func numberField(m map[string]any, key string, def float64) float64 {
	if n, ok := docNumber(m[key]); ok {
		return n
	}
	return def
}

func docNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
