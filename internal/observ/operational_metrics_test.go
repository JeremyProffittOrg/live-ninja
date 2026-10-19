package observ

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestReviewedOperationalMetricsKeepLogsAndAuditMetrics(t *testing.T) {
	for _, tc := range []struct {
		ns, name string
		emit     bool
	}{
		{"LiveNinja/Versioning", "ClientVersions", false},
		{"LiveNinja/Tools", "ToolInvocations", true},
		{"LiveNinja/Memory", "AgentCoreEvents", true},
		{"LiveNinja/RealtimeBroker", "SessionsBrokered", true},
		{"LiveNinja/Versioning", "FutureMetric", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PUBLISH_OPERATIONAL_CLOUDWATCH_METRICS", "")
			var out bytes.Buffer
			emit(&out, tc.ns, tc.name, 1, "Count", map[string]string{"surface": "android"})
			var doc map[string]any
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			_, ok := doc["_aws"]
			if ok != tc.emit || doc[tc.name] != float64(1) || doc["surface"] != "android" {
				t.Fatalf("unexpected publication/log behavior: %v", doc)
			}
		})
	}
}
func TestOperationalMetricsExplicitOptIn(t *testing.T) {
	t.Setenv("PUBLISH_OPERATIONAL_CLOUDWATCH_METRICS", "true")
	var out bytes.Buffer
	emit(&out, "LiveNinja/Versioning", "ClientVersions", 1, "Count", nil)
	if !bytes.Contains(out.Bytes(), []byte("_aws")) {
		t.Fatal("explicit opt-in failed")
	}
}
