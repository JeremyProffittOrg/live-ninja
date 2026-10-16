package webapp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestPhoneBudgetOwnerCSRFAndFixedGrant(t *testing.T) {
	for _, tc := range []struct {
		name, user, role, surface, scope, csrf, body string
		want                                         int
	}{
		{"owner", "owner-uuid", "owner", "web", "", "match", `{"requestId":"grant-12345678","expectedDay":"2026-10-16"}`, 200},
		{"anonymous", "", "", "web", "", "match", `{}`, 401},
		{"member", "member", "member", "web", "", "match", `{}`, 403},
		{"revoked-owner", "revoked", "owner", "web", "", "match", `{}`, 403},
		{"mcp", "owner-uuid", "owner", "mcp", "", "match", `{}`, 403},
		{"scoped", "owner-uuid", "owner", "web", "nova", "match", `{}`, 403},
		{"missing-csrf", "owner-uuid", "owner", "web", "", "", `{}`, 403},
		{"wrong-csrf", "owner-uuid", "owner", "web", "", "wrong", `{}`, 403},
		{"identity-injection", "owner-uuid", "owner", "web", "", "match", `{"ownerUserId":"other","requestId":"grant-12345678","expectedDay":"2026-10-16"}`, 400},
		{"amount-injection", "owner-uuid", "owner", "web", "", "match", `{"amount":50,"requestId":"grant-12345678","expectedDay":"2026-10-16"}`, 400},
		{"invalid-day", "owner-uuid", "owner", "web", "", "match", `{"requestId":"grant-12345678","expectedDay":"2026-99-99"}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &phoneLambdaFake{status: 200}
			service := &phoneMessages{client: fake, function: "test"}
			app := fiber.New()
			app.Use(func(c *fiber.Ctx) error {
				c.Locals(localUserID, tc.user)
				c.Locals(localRole, tc.role)
				c.Locals(localSurface, tc.surface)
				c.Locals(localScope, tc.scope)
				return c.Next()
			})
			app.Use(CSRFProtect())
			registerPhoneBudgetAPI(app, service, func(_ context.Context, uid string) error {
				if uid != "owner-uuid" {
					return errAPIUserNotAllowed
				}
				return nil
			})
			req := httptest.NewRequest("POST", "/api/v1/phone/budget/grant", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Cookie", CSRFCookieName+"=match")
			req.Header.Set(CSRFHeaderName, tc.csrf)
			res, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d", res.StatusCode, tc.want)
			}
			if tc.want != 200 {
				if fake.payload != nil {
					t.Fatal("unauthorized Lambda invocation")
				}
				return
			}
			if fake.payload["operation"] != "owner_budget_grant" || fake.payload["ownerUserId"] != "owner-uuid" || fake.payload["requestId"] != "grant-12345678" || fake.payload["expectedDay"] != "2026-10-16" || len(fake.payload) != 4 {
				t.Fatal(fake.payload)
			}
			// An exact HTTP retry carries the same idempotency identity to the ledger.
			retry := httptest.NewRequest("POST", "/api/v1/phone/budget/grant", strings.NewReader(tc.body))
			retry.Header = req.Header.Clone()
			again, err := app.Test(retry)
			if err != nil {
				t.Fatal(err)
			}
			again.Body.Close()
			if fake.payload["requestId"] != "grant-12345678" {
				t.Fatal(fake.payload)
			}
			fake.payload = nil
			read, err := app.Test(httptest.NewRequest("GET", "/api/v1/phone/budget", nil))
			if err != nil {
				t.Fatal(err)
			}
			defer read.Body.Close()
			if read.StatusCode != 200 || fake.payload["operation"] != "owner_budget_status" || fake.payload["ownerUserId"] != "owner-uuid" || len(fake.payload) != 2 {
				t.Fatal(fake.payload)
			}
			var body map[string]any
			if err := json.NewDecoder(read.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
		})
	}
}
