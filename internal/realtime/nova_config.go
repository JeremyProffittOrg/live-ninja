package realtime

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

// BuildNovaSessionConfig creates the server-owned Nova bootstrap sent through
// the client to the bridge. Nova executes every tool call in the backend, so
// its tool list is deliberately limited to the server-executable catalog.
func BuildNovaSessionConfig(systemPrompt string) voiceengine.Config {
	manifest := toolManifestForServerExecution()
	specs := make([]voiceengine.ToolSpec, 0, len(manifest))
	for _, entry := range manifest {
		name, nameOK := entry["name"].(string)
		description, descriptionOK := entry["description"].(string)
		if !nameOK || !descriptionOK {
			panic("realtime: malformed static server tool manifest")
		}
		schema, err := json.Marshal(entry["parameters"])
		if err != nil {
			panic(fmt.Sprintf("realtime: marshal Nova tool %s schema: %v", name, err))
		}
		specs = append(specs, voiceengine.ToolSpec{
			Name:        name,
			Description: description,
			InputSchema: schema,
		})
	}
	return voiceengine.Config{
		SystemPrompt: systemPrompt,
		Tools:        specs,
	}
}

// NovaConfigDigest returns the URL-safe SHA-256 digest stamped into the signed
// bridge token. The bridge recomputes it from the parsed first frame before
// opening Bedrock, preventing a client from changing persona or tool policy.
func NovaConfigDigest(config voiceengine.Config) string {
	raw, err := json.Marshal(config)
	if err != nil {
		panic(fmt.Sprintf("realtime: marshal Nova session config: %v", err))
	}
	// Normalize through an interface tree so object-key ordering and
	// insignificant whitespace introduced by browser/Android JSON libraries
	// cannot break an otherwise identical signed config.
	var semantic any
	if err := json.Unmarshal(raw, &semantic); err != nil {
		panic(fmt.Sprintf("realtime: normalize Nova session config: %v", err))
	}
	canonical, err := json.Marshal(semantic)
	if err != nil {
		panic(fmt.Sprintf("realtime: canonicalize Nova session config: %v", err))
	}
	sum := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
