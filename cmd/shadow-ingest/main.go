// Command shadow-ingest is the AWS IoT Rule target for the `config`
// named-shadow accepted stream (contracts/shadow.md, plan.md M6):
//
//	SELECT state, version AS shadowVersion, timestamp, topic(3) AS thingName
//	FROM '$aws/things/+/shadow/name/config/update/accepted'
//
// (topic(3) is the thing name in
// `$aws/things/<thingName>/shadow/name/config/update/accepted`; the rule
// action invokes this function directly with that JSON as the event.)
//
// Reconciliation (higher-version-wins, contracts/shadow.md §"Reconciliation
// rule" + the locked M6 decisions):
//
//   - Events with no `state.reported` (the backend's own `desired`
//     publishes also land on update/accepted) are ignored — this is the
//     loop guard.
//   - Every device-reported state is recorded as DEVICE#<id>/TELEM
//     liveness telemetry (lastSeen + reportedVersion) — PutItem, never
//     Scan (FR-B06).
//   - reported.settingsVersion == canonical version → in sync, done.
//   - reported.settingsVersion <  canonical version → the device is
//     behind (missed delta / clock skew / partition): republish the
//     current canonical document as `desired` to THAT device only
//     (push-down; the shadow ingests it durably even if the device is
//     offline).
//   - reported.settingsVersion == canonical+1 → sanctioned
//     device-initiated change (locked M6 decision extending shadow.md
//     rule 4): fold the device-writable fields into the canonical doc,
//     bump the table with the same optimistic ConditionExpression every
//     other surface uses (losing the race → re-read + push-down), then
//     fan the new version out to all of the user's devices.
//   - reported.settingsVersion >  canonical+1 → protocol anomaly (the
//     version counter is server-owned except the sanctioned +1 bump):
//     log and push the canonical state back down.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	lnsync "github.com/JeremyProffittOrg/live-ninja/internal/sync"
)

// desiredPublisher is the internal/sync Publisher surface the handler
// uses, interface-typed so tests inject a recorder.
type desiredPublisher interface {
	PublishDesired(ctx context.Context, devices lnsync.DeviceLister, userID string, doc map[string]any, version int64) error
	PublishToDevices(ctx context.Context, targets []store.Device, doc map[string]any, version int64) error
}

// shadowAcceptedEvent is the IoT-rule-shaped event: the shadow accepted
// document's `state` plus the rule-injected thing name.
type shadowAcceptedEvent struct {
	ThingName string `json:"thingName"`
	State     struct {
		Desired  map[string]any `json:"desired"`
		Reported map[string]any `json:"reported"`
	} `json:"state"`
}

type app struct {
	st  *store.Store
	pub desiredPublisher
	log *slog.Logger
}

// Handle processes one update/accepted event. Anomalies that retrying
// cannot fix (unknown thing, revoked device, missing reported state)
// return nil so the IoT rule does not poison-pill retry them; only
// genuine AWS-call failures propagate as errors.
func (a *app) Handle(ctx context.Context, raw json.RawMessage) error {
	var ev shadowAcceptedEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		a.log.Error("shadow-ingest: invalid event payload", slog.String("error", err.Error()))
		return err
	}
	if ev.ThingName == "" {
		a.log.Warn("shadow-ingest: event missing thingName (rule SQL must SELECT topic(3) AS thingName)")
		return nil
	}
	// Loop guard: our own desired publishes also fire update/accepted;
	// only device-published reported state is ingested.
	if ev.State.Reported == nil {
		return nil
	}

	l := a.log.With(slog.String("thingName", ev.ThingName))

	device, err := a.resolveDevice(ctx, ev.ThingName)
	if err != nil {
		l.Error("shadow-ingest: resolve device failed", slog.String("error", err.Error()))
		return err
	}
	if device == nil {
		l.Warn("shadow-ingest: no device row for thing; ignoring")
		return nil
	}
	if device.Status != store.DeviceStatusActive {
		l.Warn("shadow-ingest: reported state from non-active device ignored",
			slog.String("deviceId", device.DeviceID), slog.String("status", device.Status))
		return nil
	}
	l = l.With(slog.String("deviceId", device.DeviceID), slog.String("userId", device.UserID))

	reportedVersion := lnsync.DocVersion(map[string]any{"version": ev.State.Reported["settingsVersion"]})

	// Liveness/telemetry confirmation (shadow.md rule 4): lastSeen +
	// reportedVersion snapshot under DEVICE#<id>/TELEM — a key-addressed
	// PutItem, never a settings write.
	telem := map[string]any{"source": "shadow-reported", "reportedVersion": reportedVersion}
	for k, v := range ev.State.Reported {
		telem[k] = v
	}
	if err := a.st.PutDeviceTelemetry(ctx, device.DeviceID, telem); err != nil {
		l.Error("shadow-ingest: put device telemetry failed", slog.String("error", err.Error()))
		return err
	}

	doc, err := a.st.GetSettings(ctx, device.UserID)
	if err != nil {
		l.Error("shadow-ingest: get settings failed", slog.String("error", err.Error()))
		return err
	}
	canonical := lnsync.DocVersion(doc)

	switch {
	case reportedVersion == canonical:
		// Converged.
		observ.EmitMetric("LiveNinja/ShadowIngest", "ReportedInSync", 1, "Count",
			map[string]string{"Surface": "m5stack"})
		return nil

	case reportedVersion == canonical+1:
		return a.deviceInitiatedBump(ctx, l, device, doc, canonical, ev.State.Reported)

	case reportedVersion > canonical+1:
		l.Warn("shadow-ingest: device reported an impossible future version; pushing canonical state down",
			slog.Int64("reportedVersion", reportedVersion), slog.Int64("canonicalVersion", canonical))
		fallthrough

	default: // reportedVersion < canonical — device is behind.
		observ.EmitMetric("LiveNinja/ShadowIngest", "DesiredPushDown", 1, "Count",
			map[string]string{"Surface": "m5stack"})
		if err := a.pub.PublishToDevices(ctx, []store.Device{*device}, doc, canonical); err != nil {
			l.Error("shadow-ingest: desired push-down failed", slog.String("error", err.Error()))
			return err
		}
		l.Info("shadow-ingest: republished desired to lagging device",
			slog.Int64("reportedVersion", reportedVersion), slog.Int64("canonicalVersion", canonical))
		return nil
	}
}

// deviceInitiatedBump handles the sanctioned reported == canonical+1
// case: conditional table bump + full fan-out.
func (a *app) deviceInitiatedBump(ctx context.Context, l *slog.Logger, device *store.Device,
	doc map[string]any, canonical int64, reported map[string]any) error {

	merged, changed := lnsync.MergeDeviceReported(doc, reported)
	if !changed {
		// The device claims a new version but nothing it is allowed to
		// write differs — push the canonical state (and version) back so
		// its counter re-converges rather than minting an empty bump.
		l.Warn("shadow-ingest: version-bump report carried no foldable change; pushing canonical state down")
		return a.pub.PublishToDevices(ctx, []store.Device{*device}, doc, canonical)
	}

	newVersion, err := a.st.PutSettings(ctx, device.UserID, merged, canonical)
	if err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			// Another surface won the race — re-read and push the winner
			// down (higher-version-wins; the device's change is superseded,
			// mirroring the HTTP 409 path for request/response surfaces).
			l.Info("shadow-ingest: device-initiated bump lost the version race; pushing winner down")
			latest, gerr := a.st.GetSettings(ctx, device.UserID)
			if gerr != nil {
				return gerr
			}
			return a.pub.PublishToDevices(ctx, []store.Device{*device}, latest, lnsync.DocVersion(latest))
		}
		l.Error("shadow-ingest: device-initiated settings bump failed", slog.String("error", err.Error()))
		return err
	}
	merged["version"] = newVersion

	observ.EmitMetric("LiveNinja/ShadowIngest", "DeviceInitiatedBump", 1, "Count",
		map[string]string{"Surface": "m5stack"})
	l.Info("shadow-ingest: device-initiated settings change committed",
		slog.Int64("newVersion", newVersion))

	// Fan the new version out to every device (including the originator —
	// its reported state already matches, so its delta is empty).
	if err := a.pub.PublishDesired(ctx, a.st, device.UserID, merged, newVersion); err != nil {
		// Non-fatal: the table is committed; web/Android converge via
		// ?since polling and lagging devices via their next reported cycle.
		l.Warn("shadow-ingest: fan-out after device-initiated bump failed",
			slog.String("error", err.Error()))
	}
	return nil
}

// resolveDevice maps an IoT thing name to its DEVICE#<id>/META row. The
// provisioning convention (auth.ProvisionIoT seam, finalized by M5) is
// thingName == deviceId; a "ln-" prefix is also tolerated so an M5-side
// naming choice of ln-<deviceId> keeps working without a table change.
// Returns (nil, nil) when no row matches.
func (a *app) resolveDevice(ctx context.Context, thingName string) (*store.Device, error) {
	d, err := a.st.GetDevice(ctx, thingName)
	if err != nil || d != nil {
		return d, err
	}
	if trimmed := strings.TrimPrefix(thingName, "ln-"); trimmed != thingName {
		return a.st.GetDevice(ctx, trimmed)
	}
	return nil, nil
}

func main() {
	ctx := context.Background()
	cfg := config.FromEnv()
	logger := observ.NewLogger(os.Stdout, cfg.LogLevel)

	st, err := store.New(ctx, cfg.TableName)
	if err != nil {
		logger.Error("shadow-ingest: store init failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	pub, err := lnsync.New(ctx, logger)
	if err != nil {
		logger.Error("shadow-ingest: publisher init failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	a := &app{st: st, pub: pub, log: logger}
	lambda.Start(a.Handle)
}
