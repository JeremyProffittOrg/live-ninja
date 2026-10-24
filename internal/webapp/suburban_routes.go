package webapp

import (
	"context"
	"github.com/gofiber/fiber/v2"
)

type suburbanOAuth interface {
	Begin(context.Context, string) (string, string, error)
	Complete(context.Context, string, string, string, string, string) error
}

const suburbanCookie = "__Host-ln-suburban"

func registerSuburbanRoutes(app *fiber.App, client suburbanOAuth, owner func(*fiber.Ctx) string) {
	app.Get("/auth/suburban/start", func(c *fiber.Ctx) error {
		c.Set("Cache-Control", "no-store")
		c.Set("Referrer-Policy", "no-referrer")
		uid := owner(c)
		if uid == "" {
			return c.Status(401).SendString("Sign in to the Live Ninja owner account, then open Connect Suburban Remote from Help.")
		}
		if client == nil {
			return c.SendStatus(503)
		}
		redirect, cookie, e := client.Begin(c.UserContext(), uid)
		if e != nil {
			return c.Status(503).SendString("Suburban connection could not start. Return to Help and try again.")
		}
		c.Cookie(&fiber.Cookie{Name: suburbanCookie, Value: cookie, Path: "/", Secure: true, HTTPOnly: true, SameSite: "Lax", MaxAge: 600})
		return c.Redirect(redirect, 302)
	})
	app.Get("/auth/suburban/callback", func(c *fiber.Ctx) error {
		c.Set("Cache-Control", "no-store")
		c.Set("Referrer-Policy", "no-referrer")
		uid := owner(c)
		if uid == "" {
			return c.Status(401).SendString("Sign in to the Live Ninja owner account and reconnect Suburban Remote from Help.")
		}
		q := c.Context().QueryArgs()
		for _, name := range []string{"state", "code", "iss"} {
			if len(q.PeekMulti(name)) != 1 {
				return c.SendStatus(400)
			}
		}
		if client == nil {
			return c.SendStatus(503)
		}
		e := client.Complete(c.UserContext(), uid, c.Query("state"), c.Query("code"), c.Cookies(suburbanCookie), c.Query("iss"))
		c.Cookie(&fiber.Cookie{Name: suburbanCookie, Value: "", Path: "/", Secure: true, HTTPOnly: true, SameSite: "Lax", MaxAge: -1})
		if e != nil {
			return c.Status(400).SendString("Suburban connection was not completed. Start a fresh connection from Help; do not reuse this return page.")
		}
		c.Type("html")
		return c.SendString(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Suburban connected</title><h1>Suburban Remote connected</h1><p>You can ask Live Ninja for vehicle status and enabled controls.</p><a href="/conversation">Return to conversation</a></html>`)
	})
}
