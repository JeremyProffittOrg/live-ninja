package webapp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWebClientDeclaresAzureCapability guards the half of the Azure client gate
// that lives in the browser. The broker refuses to hand an Azure credential to
// any client that does not name the matching bootstrap mode in
// X-LN-Capabilities, so if this header stops being sent the gpt-live-azure
// engines silently stop working — every session cascades to openai-realtime
// with a warning and nothing else looks wrong.
func TestWebClientDeclaresAzureCapability(t *testing.T) {
	js := readRepoFile(t, "web/static/js/realtime.mjs")

	require.Contains(t, js, "X-LN-Capabilities",
		"the web client must declare its bootstrap capabilities on the mint")
	require.Contains(t, js, "azure-direct",
		"the web client must name azure-direct, or the broker will never route it to Azure")

	// The capability list must not claim a transport this build has not
	// written. Declaring voice-live-direct without the branch that handles it
	// would make the broker hand over an Entra token the client cannot use.
	capIdx := strings.Index(js, "const CLIENT_CAPABILITIES")
	require.GreaterOrEqual(t, capIdx, 0, "CLIENT_CAPABILITIES must be a named constant")
	line := js[capIdx : capIdx+strings.Index(js[capIdx:], "\n")]
	assert.NotContains(t, line, "voice-live-direct",
		"voice-live-direct is not implemented in this build; declaring it would hand over an unusable credential")
}

// TestWebClientHonoursServerCallsURL guards the other half: the SDP host must
// come from the broker, not from the compiled-in OpenAI constant. Posting an
// Azure ephemeral secret to api.openai.com is the exact failure the callsUrl
// field exists to prevent.
func TestWebClientHonoursServerCallsURL(t *testing.T) {
	js := readRepoFile(t, "web/static/js/realtime.mjs")

	require.Contains(t, js, "minted.callsUrl",
		"the client must read callsUrl from the mint response")

	// The fallback to the compiled-in constant must survive, so this build
	// keeps working against a broker that predates the field.
	assert.Contains(t, js, "OPENAI_CALLS_URL",
		"the compiled-in OpenAI host must remain as the fallback")

	// The SDP POST must not reach for this.callsUrl directly any more — that
	// is the constructor default and ignores what the server said.
	assert.NotContains(t, js, "? this.callsUrl + '?model='",
		"the SDP POST must use the per-session host, not the constructor default")
}
