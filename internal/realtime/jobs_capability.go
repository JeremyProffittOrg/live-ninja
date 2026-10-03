package realtime

import (
	"context"
	"encoding/json"
	"strings"
)

const JobsReviewCapability = "jobs-review-v1"

type jobsReviewContextKey struct{}

// This is a compatibility signal, never an identity or permission grant.
func ClientSupportsJobsReview(surface string, capabilities []string) bool {
	if surface != "web" && surface != "android" {
		return false
	}
	for _, capability := range capabilities {
		if capability == JobsReviewCapability {
			return true
		}
	}
	return false
}
func WithClientCapabilities(ctx context.Context, surface string, capabilities []string) context.Context {
	return context.WithValue(ctx, jobsReviewContextKey{}, ClientSupportsJobsReview(surface, capabilities))
}
func jobsReviewEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(jobsReviewContextKey{}).(bool)
	return enabled
}

// FilterJobsTools handles flat realtime declarations and wrapped fallback ones.
// The catalog remains immutable; callers receive a separate filtered slice.
func FilterJobsTools(manifest []map[string]any, enabled bool) []map[string]any {
	out := make([]map[string]any, 0, len(manifest))
	for _, entry := range manifest {
		name, _ := entry["name"].(string)
		if function, ok := entry["function"].(map[string]any); ok {
			name, _ = function["name"].(string)
		}
		if !enabled && strings.HasPrefix(name, "job_") {
			continue
		}
		out = append(out, entry)
	}
	return out
}
func ClientInstructions(ctx context.Context, instructions string) string {
	if !jobsReviewEnabled(ctx) {
		return strings.ReplaceAll(instructions, jobsToolInstructions, "")
	}
	return instructions
}
func ToolManifestJSONForClient(ctx context.Context, surface string, serverExecution bool) json.RawMessage {
	manifest := toolManifestForSurface(surface)
	if serverExecution {
		manifest = toolManifestForServerExecution()
	}
	b, err := json.Marshal(FilterJobsTools(manifest, jobsReviewEnabled(ctx)))
	if err != nil {
		panic(err)
	}
	return b
}
