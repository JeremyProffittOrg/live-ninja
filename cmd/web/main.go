// Command web is the Live Ninja Fiber application. It runs behind the
// AWS Lambda Web Adapter layer (see template.yaml: WebFunction), which
// proxies Lambda invocations to a plain HTTP server listening on $PORT —
// so this main() is a completely ordinary Fiber app with no Lambda SDK
// involved.
//
// M1–M3 scope: wires webapp.Deps (store, LWA client, KMS signer, SQS email
// queue, broker Lambda client), the embedded SSR shell (web.Files →
// fingerprinted assets + html/template Views engine), and mounts
// RegisterAuthRoutes + RegisterAPIRoutes + RegisterSettingsRoutes +
// RegisterPageRoutes behind the ExtractAuthContext/CSRFProtect middleware,
// while keeping M0's /healthz, structured request logging, and graceful
// shutdown. 404s and errors render HTML for browser navigations and the
// JSON envelope for API callers (webapp.NotFoundHandler/ErrorHandler).
//
// Local dev: run with LWA_CLIENT_ID/LWA_CLIENT_SECRET (bypasses SSM) and a
// real JWT_KMS_KEY_ID (or expect signer-dependent routes to be the only
// broken ones). Startup fails fast on missing hard dependencies rather
// than serving a half-configured auth surface.
package main

import (
	"context"
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
	"github.com/JeremyProffittOrg/live-ninja/web"
)

func main() {
	cfg := config.FromEnv()
	logger := observ.NewLogger(os.Stdout, cfg.LogLevel)
	ctx := context.Background()

	deps, err := buildDeps(ctx, cfg, logger)
	if err != nil {
		logger.Error("startup failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// SSR shell: fingerprint the embedded static assets and build the
	// html/template Views engine over the embedded templates. Both fail
	// startup on error — a web app with no templates has nothing to serve.
	assets, err := webapp.NewAssets(web.Files)
	if err != nil {
		logger.Error("startup failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	renderer, err := webapp.NewRenderer(web.Files, assets)
	if err != nil {
		logger.Error("startup failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	swHandler, err := assets.FileHandler("sw.js", "text/javascript; charset=utf-8")
	if err != nil {
		logger.Error("startup failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler:          webapp.ErrorHandler(),
		Views:                 renderer,
	})

	app.Use(requestLoggerMiddleware(logger))
	app.Use(webapp.SecurityHeaders())

	// Registered before the auth middleware: liveness and static assets
	// need neither auth context nor CSRF handling. /sw.js sits at the root
	// so the service worker's scope is "/".
	app.Get("/healthz", healthzHandler)
	app.Get("/static/*", assets.Handler())
	app.Get("/sw.js", swHandler)

	// Auth context extraction (authorizer passthrough header, Bearer JWT
	// fallback) + CSRF double-submit enforcement for cookie-bearing POSTs,
	// then the route registrars (auth surface in auth_routes.go, /api/v1
	// resources in api_routes.go, settings page + API in
	// settings_routes.go, SSR pages in pages_routes.go).
	app.Use(webapp.ExtractAuthContext(deps))
	app.Use(webapp.CSRFProtect())
	webapp.RegisterAuthRoutes(app, deps)
	webapp.RegisterAPIRoutes(app, deps)
	webapp.RegisterSettingsRoutes(app, deps)
	webapp.RegisterPageRoutes(app, deps)

	// Catch-all: HTML error page for browser navigations, JSON 404 for
	// API/asset requests.
	app.Use(webapp.NotFoundHandler())

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
