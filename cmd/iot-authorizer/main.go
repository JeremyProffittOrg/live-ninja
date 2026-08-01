// Command iot-authorizer is the AWS IoT Core custom authorizer fronting MQTT
// over WebSockets for the web and Android clients (plan.md §6 WS-1).
//
// Why a custom authorizer rather than certificates or Cognito: identity here is
// Login with Amazon, there is no Cognito Identity Pool, and a browser has no
// X.509 certificate — but every client already holds a first-party ES256 access
// JWT, and cmd/authorizer already knows how to verify one. This function is
// that same decision, returning an IoT policy instead of an API Gateway one.
// The verification itself lives in internal/auth (TokenVerifier) precisely so
// the two authorizers cannot drift apart on what "revoked" means.
//
// How the token arrives: AWS IoT accepts credentials for MQTT over WebSockets
// in the MQTT CONNECT packet's user-name field, which is the only route a
// browser has — it cannot set WebSocket handshake headers. IoT passes that
// value here as protocolData.mqtt.username.
//
// The policy is the security boundary of this whole feature. It is scoped to
// ONE user's topic subtree, derived from the verified token's subject and never
// from anything the client said about itself:
//
//	Connect   client/<clientId>            — the clientId IoT reports
//	Subscribe topicfilter/liveninja/user/<uid>/#
//	Receive   topic/liveninja/user/<uid>/*
//	Publish   topic/liveninja/user/<uid>/presence/*   (presence only)
//
// Publish is deliberately NOT granted on the doc/memory topics: those are
// server-authored events, and a client that could forge one could make every
// other device of that user announce a change that never happened.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

const (
	// defaultJWKSURL mirrors template.yaml's JWKS_URL; a local-dev fallback so
	// `go run` needs no env.
	defaultJWKSURL = "https://live.jeremy.ninja/.well-known/jwks.json"

	// refreshAfterSeconds is how often IoT re-invokes this function to
	// revalidate a live connection. 300s bounds how long a revoked user keeps
	// an already-open subscription. Note AWS delays the next invocation by up
	// to 5 minutes on an IDLE connection, so worst-case revocation latency is
	// this plus that idle delay — not this alone.
	refreshAfterSeconds = 300

	// disconnectAfterSeconds is the hard ceiling on one authorization. The
	// access JWT itself lives only 15 minutes (auth.AccessTokenTTL); rather
	// than weaken exp checking to keep a socket alive, the client reconnects
	// with a fresh token and the connection is force-closed at this bound.
	disconnectAfterSeconds = 3600
)

// Request is the IoT custom-authorizer event. Only the fields this function
// reads are modelled; IoT sends more.
type Request struct {
	Token             string `json:"token"`
	SignatureVerified bool   `json:"signatureVerified"`
	ProtocolData      struct {
		MQTT *struct {
			Username string `json:"username"`
			Password string `json:"password"`
			ClientID string `json:"clientId"`
		} `json:"mqtt,omitempty"`
	} `json:"protocolData"`
}

// Response is the IoT custom-authorizer result shape.
type Response struct {
	IsAuthenticated          bool     `json:"isAuthenticated"`
	PrincipalID              string   `json:"principalId"`
	DisconnectAfterInSeconds int      `json:"disconnectAfterInSeconds"`
	RefreshAfterInSeconds    int      `json:"refreshAfterInSeconds"`
	PolicyDocuments          []string `json:"policyDocuments"`
}

var (
	logger   = observ.NewLogger(os.Stdout, config.FromEnv().LogLevel)
	verifier *auth.TokenVerifier
)

// deny is the single refusal shape. It carries no policy at all, so a denied
// connection can do nothing even if IoT were to keep it.
func deny() Response {
	return Response{IsAuthenticated: false, PrincipalID: "deny"}
}

// credential pulls the bearer token out of wherever IoT put it. MQTT-over-
// WebSocket browsers can only use the CONNECT user-name field; the top-level
// `token` field covers the header/query-parameter forms other protocols use.
//
// The user name may carry MQTT-style query suffixes (some SDKs append
// `?x-amz-customauthorizer-name=...`), so everything from the first `?` is
// dropped before the value is treated as a JWT.
func credential(req Request) string {
	if m := req.ProtocolData.MQTT; m != nil && m.Username != "" {
		return strings.TrimSpace(strings.SplitN(m.Username, "?", 2)[0])
	}
	return strings.TrimSpace(req.Token)
}

// policyFor builds the single-user IoT policy. userID comes from the VERIFIED
// token subject; clientID is what IoT reports for the connection.
func policyFor(userID, clientID string) string {
	region := os.Getenv("AWS_REGION")
	account := os.Getenv("AWS_ACCOUNT_ID")
	base := fmt.Sprintf("arn:aws:iot:%s:%s", region, account)
	user := fmt.Sprintf("liveninja/user/%s", userID)

	// clientID is echoed back into a resource ARN, so it must not be able to
	// smuggle a wildcard or a second statement in. IoT client ids are opaque;
	// anything outside a conservative set is refused by the caller before this
	// is reached (see handler).
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":"iot:Connect","Resource":"%s:client/%s"},`+
		`{"Effect":"Allow","Action":"iot:Subscribe","Resource":"%s:topicfilter/%s/#"},`+
		`{"Effect":"Allow","Action":"iot:Receive","Resource":"%s:topic/%s/*"},`+
		`{"Effect":"Allow","Action":"iot:Publish","Resource":"%s:topic/%s/presence/*"}`+
		`]}`, base, clientID, base, user, base, user, base, user)
}

// safeClientID rejects anything that could break out of the ARN it is
// interpolated into.
func safeClientID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == ':', r == '.':
		default:
			return false
		}
	}
	return true
}

func handler(ctx context.Context, req Request) (Response, error) {
	l := observ.WithRequest(logger, "", "", "iot-authorizer")

	token := credential(req)
	if token == "" {
		l.Info("iot-authorizer: no credential presented, denying")
		return deny(), nil
	}

	clientID := ""
	if m := req.ProtocolData.MQTT; m != nil {
		clientID = m.ClientID
	}
	if !safeClientID(clientID) {
		l.Info("iot-authorizer: unusable client id, denying", slog.String("clientId", clientID))
		return deny(), nil
	}

	claims, snap, err := verifier.Verify(ctx, token)
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		l.Warn("iot-authorizer: token subject has no user record, denying")
		return deny(), nil
	case errors.Is(err, auth.ErrUserNotActive):
		l.Info("iot-authorizer: user not active, denying", slog.String("userId", claims.Sub))
		return deny(), nil
	case errors.Is(err, auth.ErrTokenRevoked):
		l.Info("iot-authorizer: token predates tokensValidAfter, denying",
			slog.String("userId", claims.Sub))
		return deny(), nil
	case err != nil:
		l.Info("iot-authorizer: verification failed, denying", slog.String("error", err.Error()))
		return deny(), nil
	}

	l.Info("iot-authorizer: authorized",
		slog.String("userId", claims.Sub),
		slog.String("surface", claims.Surface),
		slog.String("clientId", clientID),
		slog.String("role", snap.Role))

	return Response{
		IsAuthenticated:          true,
		PrincipalID:              claims.Sub,
		DisconnectAfterInSeconds: disconnectAfterSeconds,
		RefreshAfterInSeconds:    refreshAfterSeconds,
		PolicyDocuments:          []string{policyFor(claims.Sub, clientID)},
	}, nil
}

func main() {
	ctx := context.Background()
	cfg := config.FromEnv()

	jwksURL := os.Getenv("JWKS_URL")
	if jwksURL == "" {
		jwksURL = defaultJWKSURL
	}

	st, err := store.New(ctx, cfg.TableName)
	if err != nil {
		logger.Error("iot-authorizer: store init failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	verifier = auth.NewTokenVerifier(jwksURL, st)

	lambda.Start(handler)
}
