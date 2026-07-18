package sync

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// fakeIoT records UpdateThingShadow calls and can fail a specific thing.
type fakeIoT struct {
	calls     []shadowCall
	failThing string
}

type shadowCall struct {
	thing   string
	shadow  string
	payload map[string]any
}

func (f *fakeIoT) UpdateThingShadow(ctx context.Context, params *iotdataplane.UpdateThingShadowInput, optFns ...func(*iotdataplane.Options)) (*iotdataplane.UpdateThingShadowOutput, error) {
	thing := aws.ToString(params.ThingName)
	if thing == f.failThing && f.failThing != "" {
		return nil, errors.New("iot unavailable")
	}
	var payload map[string]any
	if err := json.Unmarshal(params.Payload, &payload); err != nil {
		return nil, err
	}
	f.calls = append(f.calls, shadowCall{
		thing:   thing,
		shadow:  aws.ToString(params.ShadowName),
		payload: payload,
	})
	return &iotdataplane.UpdateThingShadowOutput{}, nil
}

// fakeLister returns a fixed device set.
type fakeLister struct {
	devices []store.Device
	err     error
}

func (f *fakeLister) ListDevices(ctx context.Context, userID string) ([]store.Device, error) {
	return f.devices, f.err
}

func settingsDoc() map[string]any {
	return map[string]any{
		"version":       int64(7),
		"wakeWord":      "hey-live-ninja",
		"wakeEngine":    "openwakeword",
		"sensitivity":   0.7,
		"persona":       map[string]any{"presetId": "default", "systemInstructions": nil},
		"voice":         "cedar",
		"turnDetection": "semantic_vad",
		"theme":         "system",
		"micDeviceId":   nil,
		"voiceEngine": map[string]any{
			"default": "openai-realtime",
			"devices": map[string]any{"dev-pinned": "nova-sonic"},
		},
		"privacy":       map[string]any{"storeAudio": false, "storeTranscripts": true, "retentionDays": 30},
		"futureUnknown": "kept",
	}
}

func TestBuildDesiredShape(t *testing.T) {
	d := BuildDesired(settingsDoc(), "dev1", 7)

	if d["settingsVersion"] != int64(7) {
		t.Errorf("settingsVersion = %v, want 7", d["settingsVersion"])
	}
	if d["wakeWord"] != "hey-live-ninja" {
		t.Errorf("wakeWord = %v", d["wakeWord"])
	}
	// Locked M6 decision: an M5Stack is always wakenet (shadow.md) — the
	// canonical openwakeword engine must NOT leak into desired.
	if d["wakeEngine"] != "wakenet" {
		t.Errorf("wakeEngine = %v, want wakenet", d["wakeEngine"])
	}
	if d["sensitivity"] != 0.7 {
		t.Errorf("sensitivity = %v", d["sensitivity"])
	}
	if d["voice"] != "cedar" || d["turnDetection"] != "semantic_vad" {
		t.Errorf("voice/turnDetection = %v/%v", d["voice"], d["turnDetection"])
	}
	// Unpinned device resolves the default engine.
	if d["voiceEngine"] != "openai-realtime" {
		t.Errorf("voiceEngine = %v, want openai-realtime", d["voiceEngine"])
	}
	// wakeModel is always the null pair — the device fills it from the
	// manifest (shadow.md).
	wm, ok := d["wakeModel"].(map[string]any)
	if !ok || wm["url"] != nil || wm["sha256"] != nil {
		t.Errorf("wakeModel = %v, want {url:nil, sha256:nil}", d["wakeModel"])
	}
	// Privacy is shadowed read-only for on-screen disclosure.
	pv, ok := d["privacy"].(map[string]any)
	if !ok || pv["storeAudio"] != false || pv["storeTranscripts"] != true {
		t.Errorf("privacy = %v", d["privacy"])
	}
	// Never shadowed: persona, micDeviceId, theme, unknown extras.
	for _, forbidden := range []string{"persona", "micDeviceId", "theme", "futureUnknown", "version"} {
		if _, present := d[forbidden]; present {
			t.Errorf("desired must not carry %q", forbidden)
		}
	}
}

func TestBuildDesiredPerDevicePin(t *testing.T) {
	d := BuildDesired(settingsDoc(), "dev-pinned", 7)
	if d["voiceEngine"] != "nova-sonic" {
		t.Errorf("pinned device voiceEngine = %v, want nova-sonic", d["voiceEngine"])
	}
}

func TestPublishDesiredFiltersAndPublishes(t *testing.T) {
	iotFake := &fakeIoT{}
	p := NewWithClient(iotFake, nil)
	lister := &fakeLister{devices: []store.Device{
		{DeviceID: "dev1", UserID: "u1", ThingName: "dev1", Status: store.DeviceStatusActive},
		{DeviceID: "dev-revoked", UserID: "u1", ThingName: "dev-revoked", Status: store.DeviceStatusRevoked},
		{DeviceID: "dev-nothing", UserID: "u1", ThingName: "", Status: store.DeviceStatusActive},
		{DeviceID: "dev-pinned", UserID: "u1", ThingName: "ln-dev-pinned", Status: store.DeviceStatusActive},
	}}

	if err := p.PublishDesired(context.Background(), lister, "u1", settingsDoc(), 7); err != nil {
		t.Fatalf("PublishDesired: %v", err)
	}
	if len(iotFake.calls) != 2 {
		t.Fatalf("published to %d things, want 2 (revoked + thing-less skipped)", len(iotFake.calls))
	}
	for _, call := range iotFake.calls {
		if call.shadow != "config" {
			t.Errorf("shadow name = %q, want config", call.shadow)
		}
		state, _ := call.payload["state"].(map[string]any)
		desired, _ := state["desired"].(map[string]any)
		if desired == nil {
			t.Fatalf("payload missing state.desired: %v", call.payload)
		}
		if desired["settingsVersion"] != float64(7) { // via JSON round-trip
			t.Errorf("settingsVersion = %v, want 7", desired["settingsVersion"])
		}
	}
	// The pinned device's desired resolves its own engine.
	last := iotFake.calls[1]
	state := last.payload["state"].(map[string]any)
	desired := state["desired"].(map[string]any)
	if last.thing != "ln-dev-pinned" || desired["voiceEngine"] != "nova-sonic" {
		t.Errorf("pinned device publish = thing %q engine %v", last.thing, desired["voiceEngine"])
	}
}

func TestPublishDesiredPartialFailureContinues(t *testing.T) {
	iotFake := &fakeIoT{failThing: "t1"}
	p := NewWithClient(iotFake, nil)
	lister := &fakeLister{devices: []store.Device{
		{DeviceID: "d1", ThingName: "t1", Status: store.DeviceStatusActive},
		{DeviceID: "d2", ThingName: "t2", Status: store.DeviceStatusActive},
	}}

	err := p.PublishDesired(context.Background(), lister, "u1", settingsDoc(), 7)
	if err == nil {
		t.Fatalf("want joined error for the failed thing")
	}
	if len(iotFake.calls) != 1 || iotFake.calls[0].thing != "t2" {
		t.Errorf("fan-out must continue past a failed device; calls = %+v", iotFake.calls)
	}
}

func TestPublishToDevicesNoTargetsIsNoop(t *testing.T) {
	// No data client, no endpoint resolver: zero targets must not even
	// try to resolve the IoT endpoint.
	p := &Publisher{}
	if err := p.PublishToDevices(context.Background(), nil, settingsDoc(), 7); err != nil {
		t.Fatalf("zero-target publish should be a silent no-op, got %v", err)
	}
}

func TestMergeDeviceReported(t *testing.T) {
	canonical := settingsDoc()
	reported := map[string]any{
		"settingsVersion":        float64(8),
		"wakeWord":               "computer",
		"wakeEngine":             "wakenet",
		"sensitivity":            0.4,
		"voice":                  "marin",
		"turnDetection":          "server_vad",
		"firmwareVersion":        "1.4.2",
		"deviceReportedAt":       "2026-07-17T20:31:00Z",
		"wakeModelSha256Applied": "abc",
		"privacy":                map[string]any{"storeAudio": true}, // device may not write privacy
	}

	merged, changed := MergeDeviceReported(canonical, reported)
	if !changed {
		t.Fatalf("expected changed=true")
	}
	if merged["wakeWord"] != "computer" || merged["wakeEngine"] != "wakenet" ||
		merged["voice"] != "marin" || merged["turnDetection"] != "server_vad" {
		t.Errorf("foldable fields not merged: %v", merged)
	}
	if merged["sensitivity"] != 0.4 {
		t.Errorf("sensitivity = %v, want 0.4", merged["sensitivity"])
	}
	// Read-only / bookkeeping fields never fold.
	if pv := merged["privacy"].(map[string]any); pv["storeAudio"] != false {
		t.Errorf("privacy must stay canonical (device is read-only), got %v", pv)
	}
	for _, forbidden := range []string{"firmwareVersion", "deviceReportedAt", "wakeModelSha256Applied", "settingsVersion"} {
		if _, present := merged[forbidden]; present {
			t.Errorf("bookkeeping field %q must not fold into settings", forbidden)
		}
	}
	// Untouched canonical fields (incl. unknown) preserved verbatim.
	if merged["futureUnknown"] != "kept" || merged["theme"] != "system" {
		t.Errorf("canonical fields not preserved: %v", merged)
	}
	// The original canonical map must not be mutated.
	if canonical["voice"] != "cedar" {
		t.Errorf("MergeDeviceReported mutated its input")
	}
}

func TestMergeDeviceReportedRejectsInvalid(t *testing.T) {
	canonical := settingsDoc()
	merged, changed := MergeDeviceReported(canonical, map[string]any{
		"wakeEngine":    "ears",    // not an engine
		"sensitivity":   1.5,       // out of range
		"turnDetection": "psychic", // not a strategy
		"wakeWord":      "   ",     // blank
		"voice":         "",        // empty
	})
	if changed {
		t.Fatalf("all-invalid report must yield changed=false")
	}
	if merged["wakeEngine"] != "openwakeword" || merged["sensitivity"] != 0.7 {
		t.Errorf("invalid values leaked into merge: %v", merged)
	}
}

func TestMergeDeviceReportedNoChange(t *testing.T) {
	canonical := settingsDoc()
	_, changed := MergeDeviceReported(canonical, map[string]any{
		"wakeWord": "hey-live-ninja", "sensitivity": 0.7,
	})
	if changed {
		t.Errorf("identical report must yield changed=false")
	}
}

func TestDocVersion(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{int64(5), 5},
		{float64(5), 5},
		{int(5), 5},
		{json.Number("5"), 5},
		{"5", 0},
		{nil, 0},
	}
	for _, tc := range cases {
		if got := DocVersion(map[string]any{"version": tc.in}); got != tc.want {
			t.Errorf("DocVersion(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
