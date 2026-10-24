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
	"runtime/debug"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	fiberrecover "github.com/gofiber/fiber/v2/middleware/recover"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/memory"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
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

	app := newHTTPApp(renderer)

	// TxnMiddleware runs first: it assigns each request its transaction id,
	// sets the X-LN-Txn response header + Locals/context, and emits the
	// verbose request/response log pair (with txId, redacted auth headers).
	// It supersedes the old single-line request logger.
	app.Use(webapp.TxnMiddleware(logger))
	// fasthttp does no panic recovery of its own ("any panic will take down
	// the entire server" — fasthttp/server.go), and under the Lambda Web
	// Adapter a dead process is a 502 for THIS request plus a cold start for
	// the next one on the container. Recover here, inside TxnMiddleware so
	// the response log line still lands, and route the panic to the app
	// ErrorHandler as a 500 with its stack in the txId-tagged log.
	app.Use(fiberrecover.New(fiberrecover.Config{
		EnableStackTrace: true,
		StackTraceHandler: func(c *fiber.Ctx, e any) {
			logger.Error("handler panic recovered",
				slog.String("txId", webapp.TxID(c)),
				slog.String("method", c.Method()),
				slog.String("path", c.Path()),
				slog.Any("panic", e),
				slog.String("stack", string(debug.Stack())),
			)
		},
	}))
	app.Use(webapp.SecurityHeaders(assets))
	// X-LN-Server on every response + X-LN-Client parsing/EMF/below-min
	// 426 gate (contracts/headers.md, plan.md M7 "Versioning/compat") —
	// mounted early like SecurityHeaders so it applies uniformly ahead of
	// both the public routes below and the authenticated route groups.
	app.Use(webapp.VersionMiddleware(deps))

	// Registered before the auth middleware: liveness, static assets,
	// compatibility, and Android distribution documents need neither auth context nor CSRF
	// handling. /sw.js sits at the root so the service worker's scope is
	// "/". /v1/compat must be reachable by a device that cannot yet
	// authenticate (contracts/headers.md) — already in cmd/authorizer's
	// public allowlist.
	app.Get("/healthz", healthzHandler)
	app.Get("/static/*", assets.Handler())
	app.Get("/sw.js", swHandler)
	webapp.RegisterCompatRoute(app, deps)
	webapp.RegisterAndroidDistributionRoutes(app, deps)
	// The code-update progress endpoint is public by necessity: the caller is a
	// coding agent on the owner's PC holding a per-run token and no session. It
	// must therefore be registered HERE, ahead of ExtractAuthContext/CSRF, and
	// outside the /api/v1 group whose prefix middleware would reject it.
	webapp.RegisterCodeUpdateRoutes(app, deps)

	// Auth context extraction (authorizer passthrough header, Bearer JWT
	// fallback) + CSRF double-submit enforcement for cookie-bearing POSTs,
	// then the route registrars (auth surface in auth_routes.go, /api/v1
	// resources in api_routes.go, settings page + API in
	// settings_routes.go, SSR pages in pages_routes.go).
	app.Use(webapp.ExtractAuthContext(deps))
	app.Use(webapp.CSRFProtect())
	webapp.RegisterAuthRoutes(app, deps)
	webapp.RegisterAPIRoutes(app, deps)
	webapp.RegisterJobsRoutes(app, deps)
	webapp.RegisterCodeApprovalRoutes(app, deps)
	webapp.RegisterRuleReviewRoutes(app, deps)
	webapp.RegisterAccountRoutes(app, deps)
	webapp.RegisterSettingsRoutes(app, deps)
	webapp.RegisterIoTRoutes(app, deps)
	webapp.RegisterDeliverablesRoutes(app, deps)
	webapp.RegisterWakewordRoutes(app, deps)
	memSvc := buildMemoryService(ctx, deps, logger)
	webapp.RegisterMemoryRoutes(app, deps, memSvc)
	// Base Knowledge profile support (M15): the location typeahead behind the
	// "About you" pickers, and the memory-derived profile suggestion.
	webapp.RegisterProfileRoutes(app, deps, memSvc)
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

func newHTTPApp(renderer fiber.Views) *fiber.App {
	return fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler:          webapp.ErrorHandler(),
		Views:                 renderer,
		// API Gateway accepts a 10 KiB request line/header budget. Leave
		// headroom for proxy headers instead of rejecting valid login cookies
		// and bearer tokens at Fiber's 4 KiB default.
		ReadBufferSize: 16 << 10,
		// Fiber's default is 4 MiB, which cut the fallback STT upload
		// (handleFallbackSTT reads a larger multipart body) off with a bare
		// 413 before the handler ran. API Gateway's HTTP API payload cap is
		// 10 MiB, so nothing larger can arrive anyway.
		BodyLimit: 10 << 20,
	})
}

// buildDeps constructs every dependency the webapp route registrars need.
// Hard dependencies (table, LWA credentials, KMS signer) fail startup;
// there is no degraded half-auth mode.
func buildDeps(ctx context.Context, cfg config.App, logger *slog.Logger) (*webapp.Deps, error) {
	deps, err := webapp.NewServerDependencies(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	deps.LWA, err = auth.NewLWAClient(ctx, deps.Secrets)
	if err != nil {
		return nil, err
	}
	deps.Signer, err = auth.NewSigner(ctx, cfg.JWTKmsKeyID)
	if err != nil {
		return nil, err
	}
	return deps, nil
}

// buildMemoryService wires the memory core (internal/memory) over the
// shared store. ENT# rows are the structured facts. Conversation recall
// is AgentCore Memory. A nil store leaves the memory routes unconfigured.
func buildMemoryService(_ context.Context, deps *webapp.Deps, logger *slog.Logger) *memory.Service {
	if deps.Store == nil {
		logger.Warn("memory store unavailable; memory routes degraded")
		return nil
	}
	svc, err := memory.NewService(deps.Store)
	if err != nil {
		logger.Warn("memory service unavailable", slog.String("error", err.Error()))
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
