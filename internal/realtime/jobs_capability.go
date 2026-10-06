package realtime

import (
	"context"
	"encoding/json"
	"strings"

	"google.golang.org/genai"
)

const JobsReviewCapability = "jobs-review-v1"

// AndroidMediaCapability is sent by Android clients that implement the
// play_media YouTube / YouTube Music handoff. Older Android builds never send
// it, so they are never offered the tool or its instructions.
const AndroidMediaCapability = "android-media-v1"

// playMediaToolName mirrors internal/tools' play_media declaration.
const playMediaToolName = "play_media"

type jobsReviewContextKey struct{}

type androidMediaContextKey struct{}

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

// ClientSupportsAndroidMedia is a compatibility signal only: the Android app
// declares that it can intercept play_media. Any other surface is refused.
func ClientSupportsAndroidMedia(surface string, capabilities []string) bool {
	if surface != "android" {
		return false
	}
	for _, capability := range capabilities {
		if capability == AndroidMediaCapability {
			return true
		}
	}
	return false
}

func WithClientCapabilities(ctx context.Context, surface string, capabilities []string) context.Context {
	ctx = context.WithValue(ctx, jobsReviewContextKey{}, ClientSupportsJobsReview(surface, capabilities))
	return context.WithValue(ctx, androidMediaContextKey{}, ClientSupportsAndroidMedia(surface, capabilities))
}
func jobsReviewEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(jobsReviewContextKey{}).(bool)
	return enabled
}

func androidMediaEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(androidMediaContextKey{}).(bool)
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

// FilterClientTools applies every client-capability gate to a manifest: the
// existing Jobs review gate, then the Android media gate. Works on flat
// realtime/Gemini declarations and wrapped fallback ones alike.
func FilterClientTools(ctx context.Context, manifest []map[string]any) []map[string]any {
	filtered := FilterJobsTools(manifest, jobsReviewEnabled(ctx))
	if androidMediaEnabled(ctx) {
		return filtered
	}
	out := make([]map[string]any, 0, len(filtered))
	for _, entry := range filtered {
		name, _ := entry["name"].(string)
		if function, ok := entry["function"].(map[string]any); ok {
			name, _ = function["name"].(string)
		}
		if name == playMediaToolName {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// FilterClientFunctionDeclarations is FilterClientTools for the SDK-typed
// Gemini declarations locked into a minted token, so the locked constraints
// and the raw setup frame are gated identically. Returns a new slice.
func FilterClientFunctionDeclarations(ctx context.Context, decls []*genai.FunctionDeclaration) []*genai.FunctionDeclaration {
	jobs := jobsReviewEnabled(ctx)
	media := androidMediaEnabled(ctx)
	out := make([]*genai.FunctionDeclaration, 0, len(decls))
	for _, decl := range decls {
		if decl == nil {
			continue
		}
		if !jobs && strings.HasPrefix(decl.Name, "job_") {
			continue
		}
		if !media && decl.Name == playMediaToolName {
			continue
		}
		out = append(out, decl)
	}
	return out
}

// ClientInstructions strips every capability block the client did not declare.
// Each gate is applied independently; no gate may short-circuit another.
func ClientInstructions(ctx context.Context, instructions string) string {
	if !jobsReviewEnabled(ctx) {
		instructions = strings.ReplaceAll(instructions, jobsToolInstructions, "")
	}
	if !androidMediaEnabled(ctx) {
		instructions = strings.ReplaceAll(instructions, androidMediaToolInstructions, "")
	}
	return instructions
}
func ToolManifestJSONForClient(ctx context.Context, surface string, serverExecution bool) json.RawMessage {
	manifest := toolManifestForSurface(surface)
	if serverExecution {
		manifest = toolManifestForServerExecution()
	}
	b, err := json.Marshal(FilterClientTools(ctx, manifest))
	if err != nil {
		panic(err)
	}
	return b
}
