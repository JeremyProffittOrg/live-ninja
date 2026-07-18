package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	lnsync "github.com/JeremyProffittOrg/live-ninja/internal/sync"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// fakePublisher records fan-out calls without any IoT plumbing.
type fakePublisher struct {
	fanOuts   []publishRecord // PublishDesired (all-device fan-out)
	pushDowns []publishRecord // PublishToDevices (targeted push-down)
}

type publishRecord struct {
	userID  string
	things  []string
	version int64
	doc     map[string]any
}

func (f *fakePublisher) PublishDesired(ctx context.Context, devices lnsync.DeviceLister, userID string, doc map[string]any, version int64) error {
	f.fanOuts = append(f.fanOuts, publishRecord{userID: userID, version: version, doc: doc})
	return nil
}

func (f *fakePublisher) PublishToDevices(ctx context.Context, targets []store.Device, doc map[string]any, version int64) error {
	things := make([]string, 0, len(targets))
	for _, d := range targets {
		things = append(things, d.ThingName)
	}
	f.pushDowns = append(f.pushDowns, publishRecord{things: things, version: version, doc: doc})
	return nil
}

// newTestApp seeds a FakeDynamo with one active device (thingName ==
// deviceId, the provisioning convention) and the user's settings at
// version 2 (voice cedar), returning the wired handler app.
func newTestApp(t *testing.T) (*app, *testutil.FakeDynamo, *fakePublisher) {
	t.Helper()
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja")
	ctx := context.Background()

	if err := st.CreateDevice(ctx, &store.Device{
		DeviceID:  "dev1",
		UserID:    "u1",
		Name:      "Kitchen Tab5",
		ThingName: "dev1",
		FamilyID:  "fam1",
	}); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	doc := store.DefaultSettings()
	delete(doc, "version")
	if _, err := st.PutSettings(ctx, "u1", doc, 1); err != nil { // stored version 2
		t.Fatalf("seed settings: %v", err)
	}

	pub := &fakePublisher{}
	return &app{
		st:  st,
		pub: pub,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, fake, pub
}

func event(t *testing.T, thing string, reported map[string]any) json.RawMessage {
	t.Helper()
	ev := map[string]any{"thingName": thing}
	state := map[string]any{}
	if reported != nil {
		state["reported"] = reported
	}
	ev["state"] = state
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return raw
}

func TestIgnoresDesiredOnlyEcho(t *testing.T) {
	a, fake, pub := newTestApp(t)

	// The backend's own desired publish echoes on update/accepted with no
	// reported state — must be ignored (loop guard), no telemetry write.
	raw, _ := json.Marshal(map[string]any{
		"thingName": "dev1",
		"state":     map[string]any{"desired": map[string]any{"settingsVersion": 3}},
	})
	if err := a.Handle(context.Background(), raw); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(pub.fanOuts)+len(pub.pushDowns) != 0 {
		t.Errorf("desired echo must not trigger any publish")
	}
	if fake.RawItem("DEVICE#dev1", "TELEM") != nil {
		t.Errorf("desired echo must not write telemetry")
	}
}

func TestInSyncReportRecordsTelemetryOnly(t *testing.T) {
	a, fake, pub := newTestApp(t)

	err := a.Handle(context.Background(), event(t, "dev1", map[string]any{
		"settingsVersion": 2,
		"firmwareVersion": "1.4.2",
	}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(pub.fanOuts)+len(pub.pushDowns) != 0 {
		t.Errorf("in-sync report must not publish anything")
	}
	if fake.RawItem("DEVICE#dev1", "TELEM") == nil {
		t.Errorf("reported state must be recorded as DEVICE#/TELEM liveness telemetry")
	}
}

func TestLaggingDeviceGetsPushDown(t *testing.T) {
	a, _, pub := newTestApp(t)

	err := a.Handle(context.Background(), event(t, "dev1", map[string]any{
		"settingsVersion": 1, // behind the canonical 2
	}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(pub.pushDowns) != 1 {
		t.Fatalf("want exactly one targeted push-down, got %d", len(pub.pushDowns))
	}
	pd := pub.pushDowns[0]
	if len(pd.things) != 1 || pd.things[0] != "dev1" || pd.version != 2 {
		t.Errorf("push-down = things %v version %d, want [dev1] 2", pd.things, pd.version)
	}
	if len(pub.fanOuts) != 0 {
		t.Errorf("push-down must be targeted, not a full fan-out")
	}
}

func TestFutureVersionAnomalyGetsPushDown(t *testing.T) {
	a, _, pub := newTestApp(t)

	err := a.Handle(context.Background(), event(t, "dev1", map[string]any{
		"settingsVersion": 9, // > canonical+1: server-owned counter violated
	}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(pub.pushDowns) != 1 || pub.pushDowns[0].version != 2 {
		t.Errorf("future-version anomaly must push canonical state down, got %+v", pub.pushDowns)
	}
}

func TestDeviceInitiatedBumpCommitsAndFansOut(t *testing.T) {
	a, _, pub := newTestApp(t)
	ctx := context.Background()

	err := a.Handle(ctx, event(t, "dev1", map[string]any{
		"settingsVersion": 3, // canonical+1: sanctioned device-initiated change
		"voice":           "marin",
		"sensitivity":     0.9,
	}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	doc, err := a.st.GetSettings(ctx, "u1")
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if lnsync.DocVersion(doc) != 3 {
		t.Errorf("canonical version = %d, want 3", lnsync.DocVersion(doc))
	}
	if doc["voice"] != "marin" {
		t.Errorf("voice = %v, want marin", doc["voice"])
	}
	if n, _ := doc["sensitivity"].(float64); n != 0.9 {
		t.Errorf("sensitivity = %v, want 0.9", doc["sensitivity"])
	}
	// Unrelated canonical fields survive the fold.
	if doc["wakeWord"] != "hey-live-ninja" || doc["theme"] != "system" {
		t.Errorf("canonical fields lost in device bump: %v", doc)
	}

	if len(pub.fanOuts) != 1 {
		t.Fatalf("want one full fan-out after commit, got %d", len(pub.fanOuts))
	}
	fo := pub.fanOuts[0]
	if fo.userID != "u1" || fo.version != 3 || fo.doc["voice"] != "marin" {
		t.Errorf("fan-out = %+v, want u1/v3/marin", fo)
	}
}

func TestDeviceBumpWithNoFoldableChangePushesDown(t *testing.T) {
	a, _, pub := newTestApp(t)
	ctx := context.Background()

	err := a.Handle(ctx, event(t, "dev1", map[string]any{
		"settingsVersion": 3,       // claims a bump...
		"voice":           "cedar", // ...but nothing actually differs
		"firmwareVersion": "1.4.2", // bookkeeping only
	}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	doc, _ := a.st.GetSettings(ctx, "u1")
	if lnsync.DocVersion(doc) != 2 {
		t.Errorf("empty bump must not mint a version, canonical = %d", lnsync.DocVersion(doc))
	}
	if len(pub.pushDowns) != 1 || pub.pushDowns[0].version != 2 {
		t.Errorf("empty bump must push canonical state down, got %+v", pub.pushDowns)
	}
	if len(pub.fanOuts) != 0 {
		t.Errorf("empty bump must not fan out")
	}
}

// racingDDB wraps FakeDynamo so that the first PutItem targeting the
// user's SETTINGS item is preceded by a simulated concurrent writer
// committing version 3 (voice sage) — the handler's conditional write
// (expecting version 2) then fails exactly like a real lost
// optimistic-concurrency race.
type racingDDB struct {
	*testutil.FakeDynamo
	raced bool
}

func (r *racingDDB) PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	pk, _ := params.Item["pk"].(*types.AttributeValueMemberS)
	sk, _ := params.Item["sk"].(*types.AttributeValueMemberS)
	if !r.raced && pk != nil && sk != nil && pk.Value == "USER#u1" && sk.Value == "SETTINGS" {
		// Only race an EXISTING settings item — the test's own seed write
		// (first-ever settings PutItem, no stored item yet) passes through.
		if winner := r.RawItem("USER#u1", "SETTINGS"); winner != nil {
			r.raced = true
			winner["version"] = &types.AttributeValueMemberN{Value: "3"}
			winner["voice"] = &types.AttributeValueMemberS{Value: "sage"}
			r.SeedItem(winner)
		}
	}
	return r.FakeDynamo.PutItem(ctx, params, optFns...)
}

func TestDeviceInitiatedBumpLostRacePushesWinnerDown(t *testing.T) {
	fake := testutil.NewFakeDynamo()
	racing := &racingDDB{FakeDynamo: fake}
	st := store.NewWithClient(racing, "live-ninja")
	ctx := context.Background()

	if err := st.CreateDevice(ctx, &store.Device{
		DeviceID: "dev1", UserID: "u1", Name: "Kitchen Tab5",
		ThingName: "dev1", FamilyID: "fam1",
	}); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	doc := store.DefaultSettings()
	delete(doc, "version")
	if _, err := st.PutSettings(ctx, "u1", doc, 1); err != nil { // stored version 2
		t.Fatalf("seed settings: %v", err)
	}
	pub := &fakePublisher{}
	a := &app{st: st, pub: pub, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Device claims canonical+1 with a real foldable change (voice marin),
	// but another surface commits version 3 (voice sage) first.
	err := a.Handle(ctx, event(t, "dev1", map[string]any{
		"settingsVersion": 3,
		"voice":           "marin",
	}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got, _ := a.st.GetSettings(ctx, "u1")
	if lnsync.DocVersion(got) != 3 || got["voice"] != "sage" {
		t.Errorf("winner must stand: version=%d voice=%v, want 3/sage",
			lnsync.DocVersion(got), got["voice"])
	}
	if len(pub.fanOuts) != 0 {
		t.Errorf("lost race must not trigger a full fan-out")
	}
	if len(pub.pushDowns) != 1 {
		t.Fatalf("want one targeted push-down of the winner, got %d", len(pub.pushDowns))
	}
	pd := pub.pushDowns[0]
	if len(pd.things) != 1 || pd.things[0] != "dev1" || pd.version != 3 || pd.doc["voice"] != "sage" {
		t.Errorf("push-down = things %v version %d voice %v, want [dev1] 3 sage",
			pd.things, pd.version, pd.doc["voice"])
	}
}

func TestUnknownThingIsIgnoredNotRetried(t *testing.T) {
	a, _, pub := newTestApp(t)

	err := a.Handle(context.Background(), event(t, "never-provisioned", map[string]any{
		"settingsVersion": 5,
	}))
	if err != nil {
		t.Fatalf("unknown thing must not error (would poison-pill IoT retries): %v", err)
	}
	if len(pub.fanOuts)+len(pub.pushDowns) != 0 {
		t.Errorf("unknown thing must not publish")
	}
}

func TestLnPrefixedThingNameResolves(t *testing.T) {
	a, _, pub := newTestApp(t)

	// M5 provisioning may name things ln-<deviceId>; the resolver strips it.
	err := a.Handle(context.Background(), event(t, "ln-dev1", map[string]any{
		"settingsVersion": 1,
	}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(pub.pushDowns) != 1 {
		t.Errorf("ln- prefixed thing must resolve to dev1 and get its push-down")
	}
}

func TestRevokedDeviceIgnored(t *testing.T) {
	a, _, pub := newTestApp(t)
	ctx := context.Background()

	if err := a.st.RevokeDevice(ctx, "dev1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	err := a.Handle(ctx, event(t, "dev1", map[string]any{
		"settingsVersion": 3,
		"voice":           "marin",
	}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	doc, _ := a.st.GetSettings(ctx, "u1")
	if lnsync.DocVersion(doc) != 2 || len(pub.fanOuts)+len(pub.pushDowns) != 0 {
		t.Errorf("revoked device must not influence settings or trigger publishes")
	}
}
