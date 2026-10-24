package main

import (
	"context"
	"log"
	"log/slog"
	"os"

	"github.com/JeremyProffittOrg/live-ninja/internal/alexa"
	"github.com/aws/aws-lambda-go/lambda"
)

func main() {
	if os.Getenv("ALEXA_ENABLED") != "true" {
		h := &alexa.Handler{SkillID: os.Getenv("ALEXA_SKILL_ID"), Log: slog.New(slog.NewJSONHandler(os.Stdout, nil))}
		lambda.Start(h.Handle)
		return
	}
	runtime, err := alexa.NewRuntime(context.Background())
	if err != nil {
		log.Fatal("Alexa initialization failed; verify deployment configuration")
	}
	lambda.Start(runtime.Handler.Handle)
}
