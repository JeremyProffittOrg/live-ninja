// Command iot-ingest is the AWS IoT Rule target for
// `liveninja/+/telemetry` (see template.yaml's IoT Topic Rule).
//
// M0 real behavior (per plan.md): parse the telemetry JSON payload
// ({deviceId, ...}) delivered directly by the IoT rule action and PutItem
// a lastSeen snapshot into the table under DEVICE#<id>/TELEM.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

var (
	logger = observ.NewLogger(os.Stdout, config.FromEnv().LogLevel)
	st     *store.Store
)

// handler receives the raw JSON published on `liveninja/<thing>/telemetry`
// — an IoT Rule "Lambda" action invokes the function directly with the
// MQTT message body as the event, so there is no envelope to unwrap.
func handler(ctx context.Context, raw json.RawMessage) error {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		logger.Error("iot-ingest: invalid telemetry payload", slog.String("error", err.Error()))
		return err
	}

	deviceID, _ := payload["deviceId"].(string)
	if deviceID == "" {
		err := errors.New("iot-ingest: telemetry payload missing deviceId")
		logger.Error(err.Error())
		return err
	}

	l := observ.WithRequest(logger, "", "", "m5stack").With(slog.String("deviceId", deviceID))

	if err := st.PutDeviceTelemetry(ctx, deviceID, payload); err != nil {
		l.Error("iot-ingest: put telemetry failed", slog.String("error", err.Error()))
		return err
	}

	l.Info("iot-ingest: telemetry recorded")
	observ.EmitMetric("LiveNinja/IotIngest", "TelemetryRecords", 1, "Count",
		map[string]string{"Surface": "m5stack"})
	return nil
}

func main() {
	ctx := context.Background()
	cfg := config.FromEnv()

	s, err := store.New(ctx, cfg.TableName)
	if err != nil {
		logger.Error("iot-ingest: store init failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	st = s

	lambda.Start(handler)
}
