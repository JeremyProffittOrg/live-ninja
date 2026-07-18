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
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/deliv"
	"github.com/JeremyProffittOrg/live-ninja/internal/memory"
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
	// X-LN-Server on every response + X-LN-Client parsing/EMF/below-min
	// 426 gate (contracts/headers.md, plan.md M7 "Versioning/compat") —
	// mounted early like SecurityHeaders so it applies uniformly ahead of
	// both the public routes below and the authenticated route groups.
	app.Use(webapp.VersionMiddleware(deps))

	// Registered before the auth middleware: liveness, static assets, and
	// the compat-negotiation route need neither auth context nor CSRF
	// handling. /sw.js sits at the root so the service worker's scope is
	// "/". /v1/compat must be reachable by a device that cannot yet
	// authenticate (contracts/headers.md) — already in cmd/authorizer's
	// public allowlist.
	app.Get("/healthz", healthzHandler)
	app.Get("/static/*", assets.Handler())
	app.Get("/sw.js", swHandler)
	webapp.RegisterCompatRoute(app, deps)

	// Auth context extraction (authorizer passthrough header, Bearer JWT
	// fallback) + CSRF double-submit enforcement for cookie-bearing POSTs,
	// then the route registrars (auth surface in auth_routes.go, /api/v1
	// resources in api_routes.go, settings page + API in
	// settings_routes.go, SSR pages in pages_routes.go).
	app.Use(webapp.ExtractAuthContext(deps))
	app.Use(webapp.CSRFProtect())
	webapp.RegisterAuthRoutes(app, deps)
	webapp.RegisterAPIRoutes(app, deps)
	webapp.RegisterAccountRoutes(app, deps)
	webapp.RegisterSettingsRoutes(app, deps)
	webapp.RegisterDeliverablesRoutes(app, deps)
	webapp.RegisterWakewordRoutes(app, deps)
	webapp.RegisterMemoryRoutes(app, deps, buildMemoryService(ctx, deps, logger))
	webapp.RegisterHistoryRoutes(app, deps)
	webapp.RegisterTelemetryRoutes(app, deps)
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

	lambdaClient := lambdasvc.NewFromConfig(awsCfg)
	deps := &webapp.Deps{
		Store:               st,
		LWA:                 lwa,
		Signer:              signer,
		Cfg:                 cfg,
		Secrets:             loader,
		Log:                 logger,
		BrokerFn:            os.Getenv("BROKER_FUNCTION_NAME"),
		SQSEmailURL:         cfg.EmailQueueURL,
		SQS:                 sqs.NewFromConfig(awsCfg),
		Lambda:              lambdaClient,
		Firehose:            firehose.NewFromConfig(awsCfg),
		TelemetryStreamName: os.Getenv("TELEMETRY_FIREHOSE_STREAM_NAME"),
	}
	if deps.TelemetryStreamName == "" {
		logger.Warn("telemetry lake disabled (TELEMETRY_FIREHOSE_STREAM_NAME not set)")
	}

	// M9 deliverables service: only wired when the dedicated bucket is
	// configured (DELIVERABLES_BUCKET + ZIPPER_FUNCTION_NAME env, set by
	// template.yaml). Absent config degrades cleanly: deliverables routes
	// answer 503 and the deliverable_* tools report not_configured.
	if bucket := os.Getenv("DELIVERABLES_BUCKET"); bucket != "" {
		s3c := s3.NewFromConfig(awsCfg)
		svc, err := deliv.New(deliv.Config{
			S3:           s3c,
			Presign:      s3.NewPresignClient(s3c),
			Lambda:       lambdaClient,
			Store:        st,
			Bucket:       bucket,
			ZipperFn:     os.Getenv("ZIPPER_FUNCTION_NAME"),
			EnqueueEmail: deps.EnqueueEmail,
			Log:          logger,
		})
		if err != nil {
			return nil, err
		}
		deps.Deliv = svc
	} else {
		logger.Warn("deliverables store disabled (DELIVERABLES_BUCKET not set)")
	}

	return deps, nil
}

// buildMemoryService wires the M10 memory core (internal/memory): the
// Titan v2 Bedrock embedder over the shared store. The embedder client
// builds from the ambient AWS config (bedrock:InvokeModel on the one
// Titan model ARN is granted in template.yaml); a construction failure
// degrades gracefully — RegisterMemoryRoutes answers 503 not_configured
// on the embedding-dependent routes while the store-only memory routes
// (list/get/forget/guides) and all history routes stay live.
func buildMemoryService(ctx context.Context, deps *webapp.Deps, logger *slog.Logger) *memory.Service {
	embedder, err := memory.NewBedrockEmbedder(ctx)
	if err != nil {
		logger.Warn("memory embedder unavailable; semantic memory routes degraded",
			slog.String("error", err.Error()))
		return nil
	}
	svc, err := memory.NewService(deps.Store, embedder)
	if err != nil {
		logger.Warn("memory service unavailable; semantic memory routes degraded",
			slog.String("error", err.Error()))
		return nil
	}
	return svc
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
