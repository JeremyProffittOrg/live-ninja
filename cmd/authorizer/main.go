// Command authorizer is the HTTP API v2 Lambda authorizer (simple
// response format) fronting the Live Ninja API.
//
// M0 behavior (per plan.md): deny by default, allow only the explicitly
// public routes (/healthz, /static/*, /auth/*, /.well-known/*). It is not
// yet attached to the HTTP API's $default route in M0 — that wiring, plus
// real JWT/JWKS validation, lands in M1 once internal/auth/session.go
// exists.
package main

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
)

// publicPrefixes are the only routes allowed through without a valid
// session in M0. Every other path is denied by default. M1 replaces the
// "deny" branch with real ES256/JWKS validation against
// internal/auth/session.go's signer.
var publicPrefixes = []string{
	"/healthz",
	"/static/",
	"/auth/",
	"/.well-known/",
}

var logger = observ.NewLogger(os.Stdout, config.FromEnv().LogLevel)

func handler(ctx context.Context, req events.APIGatewayV2CustomAuthorizerV2Request) (events.APIGatewayV2CustomAuthorizerSimpleResponse, error) {
	path := req.RawPath
	requestID := req.RequestContext.RequestID
	l := observ.WithRequest(logger, requestID, "", "authorizer")

	if isPublicRoute(path) {
		l.Info("authorizer: public route allowed", slog.String("path", path))
		return events.APIGatewayV2CustomAuthorizerSimpleResponse{
			IsAuthorized: true,
			Context: map[string]interface{}{
				"surface": "public",
			},
		}, nil
	}

	// No session/JWT verification exists yet (that's M1 scope), so every
	// non-public route is denied by default rather than silently allowed.
	l.Info("authorizer: non-public route denied (M0 deny-by-default)", slog.String("path", path))
	return events.APIGatewayV2CustomAuthorizerSimpleResponse{
		IsAuthorized: false,
	}, nil
}

func isPublicRoute(path string) bool {
	for _, prefix := range publicPrefixes {
		if strings.HasSuffix(prefix, "/") {
			bare := strings.TrimSuffix(prefix, "/")
			if path == bare || strings.HasPrefix(path, prefix) {
				return true
			}
			continue
		}
		if path == prefix {
			return true
		}
	}
	return false
}

func main() {
	lambda.Start(handler)
}
