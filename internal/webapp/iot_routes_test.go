package webapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
)

// TestIoTTokenUsesTheNarrowAudience is the point of this whole route. The
// browser holds this token in JavaScript, so it is the credential most likely
// to leak — and it must not be an API credential. cmd/authorizer and
// cmd/iot-authorizer each carry a test refusing the other's audience; this one
// pins that the two constants are actually different, which is the assumption
// both of those rest on.
func TestIoTTokenUsesTheNarrowAudience(t *testing.T) {
	assert.NotEqual(t, auth.Audience, auth.AudienceIoT,
		"the MQTT token must not share an audience with API access tokens")
	assert.NotEmpty(t, auth.AudienceIoT)
}

// TestSanitizeClientIDMirrorsTheAuthorizer: the client id is interpolated into
// the Connect resource ARN of the policy cmd/iot-authorizer returns. Handing
// out an id that authorizer would reject produces a client that can never
// connect, with no useful error — so the two allowlists must agree.
func TestSanitizeClientIDMirrorsTheAuthorizer(t *testing.T) {
	for _, ok := range []string{"dev-1", "sess_ABC.9", "a:b", strings.Repeat("a", 128)} {
		assert.Equal(t, ok, sanitizeClientID(ok), "%q should be accepted", ok)
	}
	for _, bad := range []string{
		"", "   ", "web-*", `web","Resource":"*`, "has space", "emoji-🙂",
		strings.Repeat("a", 129),
	} {
		assert.Empty(t, sanitizeClientID(bad), "%q must be rejected", bad)
	}
}

// TestIoTCredentialsRouteIsAuthenticated: it mints a credential, so it must
// never be reachable without a session. Asserted at the source level because
// the guard is a single call that is easy to drop in a refactor and produces
// no failure anywhere else if it goes.
func TestIoTCredentialsRouteIsAuthenticated(t *testing.T) {
	src := readRepoFile(t, "internal/webapp/iot_routes.go")
	assert.Contains(t, src, `app.Group("/api/v1", RequireAuth())`,
		"the credential route must be mounted behind RequireAuth")

	// And it must not appear in the authorizer's public allowlist.
	authSrc := readRepoFile(t, "cmd/authorizer/main.go")
	assert.NotContains(t, authSrc, "/api/v1/iot",
		"the credential route must never be a public route")
}

// readRepoFile reads a path relative to the repository root.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	require.NoError(t, err, "reading %s", rel)
	return string(b)
}

// TestIoTAuthorizerNameMatchesTheTemplate: clients name the authorizer
// explicitly on the connect URL. If this string and template.yaml's
// AuthorizerName drift, every browser connection is refused by AWS before it
// reaches any of our code.
func TestIoTAuthorizerNameMatchesTheTemplate(t *testing.T) {
	tmpl := readRepoFile(t, "template.yaml")
	require.Contains(t, tmpl, "AuthorizerName: "+iotAuthorizerName,
		"the name the client sends must match the deployed authorizer")
}
