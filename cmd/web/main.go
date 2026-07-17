// Command web is the Live Ninja Fiber application. It runs behind the
// AWS Lambda Web Adapter layer (see template.yaml: WebFunction), which
// proxies Lambda invocations to a plain HTTP server listening on $PORT —
// so this main() is a completely ordinary Fiber app with no Lambda SDK
// involved.
//
// M1+M2 scope: wires webapp.Deps (store, LWA client, KMS signer, SQS email
// queue, broker Lambda client) and mounts RegisterAuthRoutes +
// RegisterAPIRoutes behind the ExtractAuthContext/CSRFProtect middleware,
// while keeping M0's /healthz, landing page, JSON 404, structured request
// logging, and graceful shutdown.
//
// Local dev: run with LWA_CLIENT_ID/LWA_CLIENT_SECRET (bypasses SSM) and a
// real JWT_KMS_KEY_ID (or expect signer-dependent routes to be the only
// broken ones). Startup fails fast on missing hard dependencies rather
// than serving a half-configured auth surface.
package main

import (
	"context"
	"html/template"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/webapp"
)

const landingPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Live Ninja</title>
<style>
  :root { color-scheme: light dark; }
  body {
    font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    margin: 0;
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    background: #0b0b12;
    color: #f4f4f8;
  }
  main { text-align: center; padding: 2rem; max-width: 32rem; }
  h1 { font-size: 1.75rem; margin: 0 0 .5rem; }
  p { color: #9a9aab; margin: 0 0 1.5rem; }
  .signin {
    display: inline-block; padding: .65rem 1.4rem; border-radius: .5rem;
    background: #8ab4ff; color: #0b0b12; text-decoration: none; font-weight: 600;
  }
</style>
</head>
<body>
<main>
  <h1>Live Ninja</h1>
  <p>Your private realtime voice assistant.</p>
  <a class="signin" href="/auth/lwa/login">Sign in with Amazon</a>
</main>
</body>
</html>
`

func main() {
	cfg := config.FromEnv()
	logger := observ.NewLogger(os.Stdout, cfg.LogLevel)
	ctx := context.Background()

	deps, err := buildDeps(ctx, cfg, logger)
	if err != nil {
		logger.Error("startup failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler:          jsonErrorHandler,
	})

	app.Use(requestLoggerMiddleware(logger))

	// Registered before the auth middleware: liveness and the landing page
	// need neither auth context nor CSRF handling.
	app.Get("/healthz", healthzHandler)

	landingTmpl := template.Must(template.New("landing").Parse(landingPageHTML))
	app.Get("/", landingHandler(landingTmpl))

	// Auth context extraction (authorizer passthrough header, Bearer JWT
	// fallback) + CSRF double-submit enforcement for cookie-bearing POSTs,
	// then the route registrars (auth surface here, /api/v1 resources in
	// api_routes.go).
	app.Use(webapp.ExtractAuthContext(deps))
	app.Use(webapp.CSRFProtect())
	webapp.RegisterAuthRoutes(app, deps)
	webapp.RegisterAPIRoutes(app, deps)

	// Catch-all: any request that fell through the routes above gets a
	// JSON 404 rather than Fiber's default plaintext response.
	app.Use(notFoundHandler)

	port := envOr("PORT", "8080")
	logger.Info("starting web server",
		slog.String("port", port),
		slog.String("domain", cfg.DomainName),
	)

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- app.Listen(":" + port)
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		if err != nil {
			logger.Error("server stopped unexpectedly", slog.String("error", err.Error()))
			os.Exit(1)
		}
	case <-quit:
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.ShutdownWithContext(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", slog.String("error", err.Error()))
	} else {
		logger.Info("shutdown complete")
	}
}

// buildDeps constructs every dependency the webapp route registrars need.
// Hard dependencies (table, LWA credentials, KMS signer) fail startup;
// there is no degraded half-auth mode.
func buildDeps(ctx context.Context, cfg config.App, logger *slog.Logger) (*webapp.Deps, error) {
	st, err := store.New(ctx, cfg.TableName)
	if err != nil {
		return nil, err
	}

	loader, err := config.NewLoader(ctx)
	if err != nil {
		return nil, err
	}

	lwa, err := auth.NewLWAClient(ctx, loader)
	if err != nil {
		return nil, err
	}

	signer, err := auth.NewSigner(ctx, cfg.JWTKmsKeyID)
	if err != nil {
		return nil, err
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}

	return &webapp.Deps{
		Store:       st,
		LWA:         lwa,
		Signer:      signer,
		Cfg:         cfg,
		Secrets:     loader,
		Log:         logger,
		BrokerFn:    os.Getenv("BROKER_FUNCTION_NAME"),
		SQSEmailURL: cfg.EmailQueueURL,
		SQS:         sqs.NewFromConfig(awsCfg),
		Lambda:      lambdasvc.NewFromConfig(awsCfg),
	}, nil
}

func healthzHandler(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"status":  "ok",
		"service": "live-ninja",
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

func landingHandler(tmpl *template.Template) fiber.Handler {
	return func(c *fiber.Ctx) error {
		c.Type("html")
		return tmpl.Execute(c.Response().BodyWriter(), nil)
	}
}

func notFoundHandler(c *fiber.Ctx) error {
	return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
		"error": "not found",
		"path":  c.Path(),
	})
}

func jsonErrorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	if fe, ok := err.(*fiber.Error); ok {
		code = fe.Code
	}
	return c.Status(code).JSON(fiber.Map{"error": err.Error()})
}

// requestLoggerMiddleware logs one structured JSON line per request with
// the standard requestId/userId/surface fields. userId/surface come from
// the auth context when ExtractAuthContext resolved one (it runs later in
// the chain, so read Locals after Next), falling back to a path-derived
// surface label pre-auth.
func requestLoggerMiddleware(logger *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()

		requestID := c.Get("X-Request-Id")
		if requestID == "" {
			requestID = c.Get("X-Amzn-Trace-Id")
		}

		err := c.Next()

		surface := webapp.Surface(c)
		if surface == "" {
			surface = surfaceForPath(c.Path())
		}
		l := observ.WithRequest(logger, requestID, webapp.UserID(c), surface)
		l.Info("request",
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.Int("status", c.Response().StatusCode()),
			slog.Duration("latency", time.Since(start)),
		)
		return err
	}
}

func surfaceForPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/static/"):
		return "static"
	case strings.HasPrefix(path, "/auth/"):
		return "auth"
	case strings.HasPrefix(path, "/.well-known/"):
		return "well-known"
	default:
		return "web"
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
