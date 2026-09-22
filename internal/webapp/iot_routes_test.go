package webapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
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
// and returns the decoded body. No surface and no opt-in header: that is the
// default path, which stays on live-ninja-iot. IOT_DATA_ENDPOINT short-circuits
// the IoT control-plane lookup, which is the same escape hatch internal/sync uses.
func iotCredentials(t *testing.T, deviceID string) map[string]any {
	t.Helper()
	return iotCredentialsWithSecrets(t, deviceID, nil)
}

func iotCredentialsWithSecrets(t *testing.T, deviceID string, secrets *config.Loader) map[string]any {
	t.Helper()
	return iotCredentialsFor(t, deviceID, secrets, "", nil)
}

func iotCredentialsFor(t *testing.T, deviceID string, secrets *config.Loader, surface string, headers map[string]string) map[string]any {
	t.Helper()
	t.Setenv("IOT_DATA_ENDPOINT", "a1b2c3-ats.iot.us-east-1.amazonaws.com")

	fakeKMS, err := testutil.NewFakeKMS()
	require.NoError(t, err)
	deps := &Deps{
		Signer:  auth.NewSignerWithClient(fakeKMS, "arn:aws:kms:us-east-1:1:key/test-key"),
		Secrets: secrets,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "user-1")
		c.Locals(localSessionID, "session-1")
		c.Locals(localSurface, surface)
		c.Locals(localDeviceID, deviceID)
		return c.Next()
	})
	app.Get("/api/v1/iot/credentials", handleIoTCredentials(deps))

	req := httptest.NewRequest("GET", "/api/v1/iot/credentials", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := app.Test(req)
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
	require.Contains(t, tmpl, "AuthorizerName: "+iotSignedAuthorizerName)
	require.Contains(t, tmpl, "SigningDisabled: false")
	require.Contains(t, tmpl, "SigningDisabled: !If [IotAuthorizerSigningEnabled, false, true]",
		"SigningDisabled on live-ninja-iot must stay conditional; AWS IoT cannot change it in place")
	require.Contains(t, tmpl, "{{resolve:ssm:/live-ninja/prod/iot/authorizer_signing_public_key}}")
}

func TestIoTCredentialsDefaultNamesTheUnsignedAuthorizer(t *testing.T) {
	body := iotCredentials(t, "dev-tab-s9")
	assert.Equal(t, iotAuthorizerName, body["authorizerName"])
	assert.Equal(t, false, body["signingRequired"])
}

func TestIoTCredentialsSignsTheTokenAndKeepsSigningOff(t *testing.T) {
	key := iotTestSigningKey(t)
	body := iotCredentialsFor(t, "dev-tab-s9", config.NewLoaderWithClient(nil), "device", nil)
	assert.Equal(t, iotAuthorizerName, body["authorizerName"])
	assert.Equal(t, false, body["signingRequired"],
		"device and the default path stay on live-ninja-iot; Tab5 does not send a signature")
	assertIoTTokenSignature(t, key, body)
}

func TestIoTCredentialsWebUsesTheSignedAuthorizer(t *testing.T) {
	key := iotTestSigningKey(t)
	body := iotCredentialsFor(t, "dev-browser", config.NewLoaderWithClient(nil), "web", nil)
	assert.Equal(t, iotSignedAuthorizerName, body["authorizerName"])
	assert.Equal(t, true, body["signingRequired"])
	assertIoTTokenSignature(t, key, body)
}

func TestIoTCredentialsAndroidStaysUnsignedWithoutTheOptIn(t *testing.T) {
	body := iotCredentialsFor(t, "dev-phone", nil, "android", nil)
	assert.Equal(t, iotAuthorizerName, body["authorizerName"])
	assert.Equal(t, false, body["signingRequired"])
}

func TestIoTCredentialsAndroidOptInUsesTheSignedAuthorizer(t *testing.T) {
	key := iotTestSigningKey(t)
	body := iotCredentialsFor(t, "dev-phone", config.NewLoaderWithClient(nil), "android", map[string]string{
		iotSigningOptInHeader: "1",
	})
	assert.Equal(t, iotSignedAuthorizerName, body["authorizerName"])
	assert.Equal(t, true, body["signingRequired"])
	assertIoTTokenSignature(t, key, body)
}

func iotTestSigningKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	t.Setenv(config.EnvOverrideIoTAuthorizerSigningPrivateKey, string(pemBytes))
	return key
}

func assertIoTTokenSignature(t *testing.T, key *rsa.PrivateKey, body map[string]any) {
	t.Helper()
	sigB64, _ := body["tokenSignature"].(string)
	require.NotEmpty(t, sigB64)
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	require.NoError(t, err)
	token, _ := body["token"].(string)
	require.NotEmpty(t, token)
	sum := sha256.Sum256([]byte(token))
	require.NoError(t, rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig))
	assert.NotContains(t, sigB64, "PRIVATE")
}

func TestIoTCredentialsOmitsABadSigningKey(t *testing.T) {
	t.Setenv(config.EnvOverrideIoTAuthorizerSigningPrivateKey, "not-a-pem")
	body := iotCredentialsWithSecrets(t, "dev-tab-s9", config.NewLoaderWithClient(nil))
	_, present := body["tokenSignature"]
	assert.False(t, present)
	assert.Equal(t, false, body["signingRequired"])
	assert.NotEmpty(t, body["token"])
}
