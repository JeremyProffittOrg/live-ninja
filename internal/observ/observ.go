// Package observ provides the structured-logging and metrics helpers
// shared by every Live Ninja function: a slog JSON logger enriched with
// the standard requestId/userId/surface fields, and a CloudWatch EMF
// (embedded metric format) emitter that writes metric documents to
// stdout — Lambda ships stdout to CloudWatch Logs automatically, and the
// EMF processor there turns these lines into real metrics with zero
// extra IAM permissions or API calls.
package observ

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

// NewLogger returns a slog.Logger that writes structured JSON to w. level
// is parsed case-insensitively ("debug", "info", "warn", "error"); an
// empty or unparseable value defaults to info. No log line ever includes
// raw PII fields — callers pass identifiers (requestId/userId/deviceId),
// never message content.
func NewLogger(w io.Writer, level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler)
}

// WithRequest returns a logger enriched with the three fields every Live
// Ninja log line carries: requestId (correlates a single request/
// invocation across services), userId (blank pre-auth or for system-
// triggered functions), and surface (web/android/m5stack/authorizer/
// system).
func WithRequest(logger *slog.Logger, requestID, userID, surface string) *slog.Logger {
	return logger.With(
		slog.String("requestId", requestID),
		slog.String("userId", userID),
		slog.String("surface", surface),
	)
}

// ---- CloudWatch EMF metrics ----

type emfMetricDef struct {
	Name string `json:"Name"`
	Unit string `json:"Unit,omitempty"`
}

type emfMetadata struct {
	Timestamp         int64                    `json:"Timestamp"`
	CloudWatchMetrics []emfMetricsDirectiveDef `json:"CloudWatchMetrics"`
}

type emfMetricsDirectiveDef struct {
	Namespace  string         `json:"Namespace"`
	Dimensions [][]string     `json:"Dimensions"`
	Metrics    []emfMetricDef `json:"Metrics"`
}

// EmitMetric writes a single-datapoint CloudWatch EMF JSON document to
// stdout under the given namespace/metric name, with dimensions attached
// as a single dimension set. unit defaults to "None" when empty (valid
// CloudWatch units: "Count", "Milliseconds", "Bytes", "None", ...).
//
// This is the only metrics-emission path used anywhere in Live Ninja
// (see plan.md M2 "SessionsBrokered"/"EphemeralTokenMintLatency" and M0's
// usage-rollup "UsageRollupRuns") — no CloudWatch PutMetricData calls, no
// extra IAM.
func EmitMetric(namespace, metricName string, value float64, unit string, dimensions map[string]string) {
	emit(os.Stdout, namespace, metricName, value, unit, dimensions)
}

func emit(w io.Writer, namespace, metricName string, value float64, unit string, dimensions map[string]string) {
	if unit == "" {
		unit = "None"
	}

	dimNames := make([]string, 0, len(dimensions))
	doc := make(map[string]any, len(dimensions)+2)
	for k, v := range dimensions {
		dimNames = append(dimNames, k)
		doc[k] = v
	}
	doc[metricName] = value
	doc["_aws"] = emfMetadata{
		Timestamp: time.Now().UnixMilli(),
		CloudWatchMetrics: []emfMetricsDirectiveDef{
			{
				Namespace:  namespace,
				Dimensions: [][]string{dimNames},
				Metrics:    []emfMetricDef{{Name: metricName, Unit: unit}},
			},
		},
	}

	b, err := json.Marshal(doc)
	if err != nil {
		fmt.Fprintf(w, "{\"error\":\"observ: emf marshal failed: %s\"}\n", err)
		return
	}
	fmt.Fprintln(w, string(b))
}
