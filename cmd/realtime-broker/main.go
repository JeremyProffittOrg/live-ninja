// Command realtime-broker is a direct-invoke Lambda (called by the web
// function's realtime handlers once those exist) responsible for minting
// OpenAI Realtime ephemeral tokens.
//
// M0 scope (per plan.md): validate the request shape and return a
// structured "not yet configured" error. Real ephemeral-token minting
// (SSM-backed OpenAI key, quota gate, persona/tool resolution, fallback
// cascade) is M2 scope — see plan.md's realtime-voice-backend milestone.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
)

// Request is the shape the web function sends when it invokes this
// broker directly (AWS Lambda Invoke, not HTTP). Fields mirror the
// eventual `GET /api/v1/realtime/session` contract described in plan.md
// M2.
type Request struct {
	UserID  string   `json:"userId"`
	Surface string   `json:"surface"`
	Persona string   `json:"persona,omitempty"`
	Voice   string   `json:"voice,omitempty"`
	Tools   []string `json:"tools,omitempty"`
}

// Response is returned for every M0 invocation: either a shape-validation
// error, or (for well-formed requests) a structured "not configured yet"
// error — never a fabricated token.
type Response struct {
	Ok    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

var (
	logger = observ.NewLogger(os.Stdout, config.FromEnv().LogLevel)

	validSurfaces = map[string]bool{
		"web":     true,
		"android": true,
		"m5stack": true,
	}

	errNotConfigured = errors.New("realtime broker is not yet configured; ephemeral-token minting lands in M2")
)

func handler(ctx context.Context, req Request) (Response, error) {
	l := observ.WithRequest(logger, "", req.UserID, req.Surface)

	if err := validateRequest(req); err != nil {
		l.Warn("realtime-broker: invalid request shape", slog.String("error", err.Error()))
		return Response{Ok: false, Error: err.Error(), Code: "invalid_request"}, nil
	}

	l.Info("realtime-broker: valid request shape, session minting not yet configured")
	observ.EmitMetric("LiveNinja/RealtimeBroker", "NotConfiguredInvocations", 1, "Count",
		map[string]string{"Surface": req.Surface})

	return Response{Ok: false, Error: errNotConfigured.Error(), Code: "not_configured"}, nil
}

func validateRequest(req Request) error {
	if req.UserID == "" {
		return errors.New("userId is required")
	}
	if req.Surface == "" {
		return errors.New("surface is required")
	}
	if !validSurfaces[req.Surface] {
		return errors.New("surface must be one of: web, android, m5stack")
	}
	return nil
}

func main() {
	lambda.Start(handler)
}
