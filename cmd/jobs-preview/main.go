// jobs-preview runs the real Jobs API/UI and durable file store on loopback.
// It has no cloud credentials, tool router, email sender or production auth
// bypass. It is a separate executable and never included in the Lambda build.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"flag"
	"fmt"
	"github.com/JeremyProffittOrg/live-ninja/internal/codeapproval"
	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/JeremyProffittOrg/live-ninja/internal/webapp"
	"github.com/JeremyProffittOrg/live-ninja/web"
	"github.com/gofiber/fiber/v2"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8793", "loopback listen address")
	data := flag.String("data", "", "required durable local Jobs data file")
	flag.Parse()
	host, _, e := net.SplitHostPort(*listen)
	if e != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("jobs-preview only accepts a literal loopback listen IP")
	}
	if *data == "" {
		log.Fatal("-data is required; choose a private local Jobs data file")
	}
	st, e := jobs.NewFileStore(*data)
	if e != nil {
		log.Fatal(e)
	}
	defer st.Close()
	svc := jobs.NewService(st)
	approvalsStore, e := codeapproval.NewFileStore(*data + ".approvals")
	if e != nil {
		log.Fatal(e)
	}
	defer approvalsStore.Close()
	approvalsService := codeapproval.NewPreviewService(approvalsStore)
	assets, e := webapp.NewAssets(web.Files)
	if e != nil {
		log.Fatal(e)
	}
	renderer, e := webapp.NewRenderer(web.Files, assets)
	if e != nil {
		log.Fatal(e)
	}
	app := fiber.New(fiber.Config{Views: renderer, DisableStartupMessage: true, BodyLimit: 16384, ErrorHandler: webapp.ErrorHandler()})
	origin := "http://" + *listen
	tokenBytes := make([]byte, 32)
	if _, e = rand.Read(tokenBytes); e != nil {
		log.Fatal(e)
	}
	token := hex.EncodeToString(tokenBytes)
	app.Use(func(c *fiber.Ctx) error {
		if c.Hostname() != *listen {
			return c.SendStatus(403)
		}
		c.Set("Cache-Control", "no-store")
		c.Set("X-Live-Ninja-Mode", "local-jobs-preview")
		if c.Get("Sec-Fetch-Site") == "cross-site" {
			return c.SendStatus(403)
		}
		if c.Method() != fiber.MethodGet && c.Method() != fiber.MethodHead && c.Get("Origin") != origin {
			return c.Status(403).JSON(fiber.Map{"error": "local_origin_required"})
		}
		return c.Next()
	})
	app.Use(webapp.SecurityHeaders(assets))
	app.Get("/healthz", func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"status": "ok", "mode": "local-jobs-preview"}) })
	app.Get("/static/*", assets.Handler())
	app.Get("/sw.js", func(c *fiber.Ctx) error { return c.SendStatus(404) })
	app.Post("/api/v1/auth/refresh", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"accessToken": token, "expiresAt": time.Now().Add(time.Hour).Unix()})
	})
	app.Use(func(c *fiber.Ctx) error {
		if strings.HasPrefix(c.Path(), "/api/") && subtle.ConstantTimeCompare([]byte(c.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			return c.SendStatus(401)
		}
		c.Locals("userId", "local-preview")
		c.Locals("surface", "web")
		c.Locals("role", "owner")
		return c.Next()
	})
	webapp.RegisterJobsAPI(app, svc, true, func(_ context.Context, uid string) error {
		if uid != "local-preview" {
			return jobs.ErrForbidden
		}
		return nil
	})
	webapp.RegisterCodeApprovalAPI(app, approvalsService, func(_ context.Context, uid string) error {
		if uid != "local-preview" {
			return jobs.ErrForbidden
		}
		return nil
	})
	app.Get("/approvals", func(c *fiber.Ctx) error { return c.Render("pages/approvals", nil) })
	app.Get("/", func(c *fiber.Ctx) error { return c.Redirect("/jobs") })
	app.Get("/jobs", func(c *fiber.Ctx) error { return c.Render("pages/jobs", nil) })
	app.Get("/conversation", func(c *fiber.Ctx) error { return c.Render("pages/conversation", nil) })
	app.Use(func(c *fiber.Ctx) error {
		return c.Status(404).JSON(fiber.Map{"error": "preview_only", "message": "This local runner provides Jobs and explicitly unverified coding approval fixtures. Other application services are not connected."})
	})
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if _, err := svc.Tick(ctx, now, 50); err != nil {
					log.Printf("jobs tick: %v", err)
				}
			}
		}
	}()
	go func() { <-ctx.Done(); _ = app.Shutdown() }()
	fmt.Printf("Local Jobs runner: %s/jobs (durable local data; no cloud execution)\n", origin)
	if err := app.Listen(*listen); err != nil {
		log.Fatal(err)
	}
}
