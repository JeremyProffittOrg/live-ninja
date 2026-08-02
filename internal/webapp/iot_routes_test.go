package webapp

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
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

// iotCredentials calls the real handler with an already-authenticated context
// and returns the decoded body. IOT_DATA_ENDPOINT short-circuits the IoT
// control-plane lookup, which is the same escape hatch internal/sync uses.
func iotCredentials(t *testing.T, deviceID string) map[string]any {
	t.Helper()
	t.Setenv("IOT_DATA_ENDPOINT", "a1b2c3-ats.iot.us-east-1.amazonaws.com")

	fakeKMS, err := testutil.NewFakeKMS()
	require.NoError(t, err)
	deps := &Deps{
		Signer: auth.NewSignerWithClient(fakeKMS, "arn:aws:kms:us-east-1:1:key/test-key"),
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "user-1")
		c.Locals(localSessionID, "session-1")
		c.Locals(localSurface, "web")
		c.Locals(localDeviceID, deviceID)
		return c.Next()
	})
	app.Get("/api/v1/iot/credentials", handleIoTCredentials(deps))

	res, err := app.Test(httptest.NewRequest("GET", "/api/v1/iot/credentials", nil))
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)
	defer res.Body.Close()

	var body map[string]any
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	return body
}

// TestIoTCredentialsCarriesTheSpeakingTopic: the turn-taking lock topic is
// handed out by the server rather than built by the client, so the grant and
// the publish are always the same bytes — a client that drifted would have
// every claim refused, and AWS signals a refused publish by closing the socket,
// which both clients treat as ordinary expiry and reconnect into.
//
// The topic deliberately sits UNDER the presence prefix. That is a rollout
// property, not tidiness: every client subscribes to `liveninja/user/<uid>/#`,
// so a tab still running the pre-deploy module graph receives lock claims too,
// and the old router had no branch for them — a claim parses as JSON, carries
// no actorDeviceId so the self-filter misses it, and reaches the nudge path,
// making the assistant announce a change that never happened. Both old clients
// discard anything containing "/presence/", so this prefix makes the rollout
// silent. See plan.md's gotcha list before moving it back.
func TestIoTCredentialsCarriesTheSpeakingTopic(t *testing.T) {
	body := iotCredentials(t, "dev-tab-s9")

	speaking, _ := body["speakingTopic"].(string)
	assert.Equal(t, "liveninja/user/user-1/presence/speaking", speaking)
	assert.Contains(t, speaking, "/presence/",
		"the lock must stay under the prefix pre-deploy clients ignore")

	// It has to sit under the ONE subscription the authorizer grants. A
	// narrower subscribe just for the lock is refused, and IoT signals a
	// refused SUBSCRIBE by closing the connection, so peers must receive claims
	// through this filter or not at all.
	filter, _ := body["topicFilter"].(string)
	require.True(t, strings.HasSuffix(filter, "#"))
	assert.True(t, strings.HasPrefix(speaking, strings.TrimSuffix(filter, "#")),
		"the lock must live under the granted topic filter %q", filter)
}

// TestIoTCredentialsKeysTheRosterByClientID pins the identity rule the presence
// roster and the speaking lock both depend on: a peer is keyed by the LAST
// segment of its presence topic, and that has to be the very same string as
// clientId. actorDeviceId is a different value — unsanitised and often empty —
// so a roster keyed by it would never match the topics it is meant to describe.
func TestIoTCredentialsKeysTheRosterByClientID(t *testing.T) {
	body := iotCredentials(t, "dev-tab-s9")

	clientID, _ := body["clientId"].(string)
	presence, _ := body["presenceTopic"].(string)
	require.NotEmpty(t, clientID)

	last := presence[strings.LastIndex(presence, "/")+1:]
	assert.Equal(t, clientID, last,
		"the presence topic's last segment IS the roster key; it must equal clientId")
	assert.Equal(t, "liveninja/user/user-1/presence/"+clientID, presence)
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
