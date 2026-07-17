// Command web is the Live Ninja Fiber application. It runs behind the
// AWS Lambda Web Adapter layer (see template.yaml: WebFunction), which
// proxies Lambda invocations to a plain HTTP server listening on $PORT —
// so this main() is a completely ordinary Fiber app with no Lambda SDK
// involved.
//
// M0 scope (per plan.md): /healthz for health checks, a minimal HTML
// landing page at "/", JSON 404 everywhere else, structured request
// logging, and graceful shutdown. Auth, realtime, settings, etc. land in
// later milestones.
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

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
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
  p { color: #9a9aab; margin: 0; }
</style>
</head>
<body>
<main>
  <h1>Live Ninja</h1>
  <p>The backend is online. The full conversational experience is coming soon.</p>
</main>
</body>
</html>
`

func main() {
	logger := observ.NewLogger(os.Stdout, config.FromEnv().LogLevel)
	cfg := config.FromEnv()

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler:          jsonErrorHandler,
	})

	app.Use(requestLoggerMiddleware(logger))

	app.Get("/healthz", healthzHandler)

	landingTmpl := template.Must(template.New("landing").Parse(landingPageHTML))
	app.Get("/", landingHandler(landingTmpl))

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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.ShutdownWithContext(ctx); err != nil {
		logger.Error("graceful shutdown failed", slog.String("error", err.Error()))
	} else {
		logger.Info("shutdown complete")
	}
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
// the standard requestId/userId/surface fields (userId is empty pre-auth;
// M1 populates it once sessions exist).
func requestLoggerMiddleware(logger *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()

		requestID := c.Get("X-Request-Id")
		if requestID == "" {
			requestID = c.Get("X-Amzn-Trace-Id")
		}

		err := c.Next()

		l := observ.WithRequest(logger, requestID, "", surfaceForPath(c.Path()))
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
