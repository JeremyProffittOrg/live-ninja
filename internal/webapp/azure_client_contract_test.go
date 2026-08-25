package webapp

import (
	"regexp"
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

// TestShippedAndroidVersionClearsTheMinimum is the guard for a defect that
// shipped silently for a long time: loadCompatVersions declared a minimum
// android version of 1.0.0 and a recommended of 2.1.0 — the worked examples
// from contracts/headers.md — while the app has only ever been 0.x.
//
// It never fired because the app ALSO sent an unparseable X-LN-Client
// ("android/0.2.2-hal+r5"), and VersionMiddleware exempts any header it cannot
// parse. The two defects masked each other exactly. Fixing the header made the
// gate live and 426'd every request from the current build, with
// "This android client version (0.3.0) is no longer supported (minimum 1.0.0)".
//
// So the check that matters is not "is the header well-formed" or "is the
// minimum sane" in isolation — it is that the version the app actually ships
// clears the minimum the server actually enforces.
func TestShippedAndroidVersionClearsTheMinimum(t *testing.T) {
	gradle := readRepoFile(t, "android/app/build.gradle.kts")

	m := regexp.MustCompile(`versionName\s*=\s*"([^"]+)"`).FindStringSubmatch(gradle)
	require.Len(t, m, 2, "could not read versionName from android/app/build.gradle.kts")

	// The wire value is trimmed at the first '-' by ClientId.WIRE_SEMVER,
	// because contracts/headers.md admits no pre-release suffix.
	shipped := strings.SplitN(m[1], "-", 2)[0]
	major, minor, patch := parseSemver(shipped)

	versions := loadCompatVersions()
	minVer := versions.min["android"]
	minMajor, minMinor, minPatch := parseSemver(minVer)

	assert.Falsef(t,
		semverLess(major, minor, patch, minMajor, minMinor, minPatch),
		"the shipped android build (%s) is BELOW the enforced minimum (%s) — "+
			"every request from a current build would be rejected with 426",
		shipped, minVer)

	// The header the app builds from this version must itself parse, or the
	// gate silently stops applying and hides any future mismatch.
	header := "android/" + shipped + "+r1"
	_, ok := parseClientVersion(header)
	assert.Truef(t, ok,
		"the shipped android build produces %q, which contracts/headers.md rejects", header)
}
