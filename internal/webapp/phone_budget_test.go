package webapp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestPhoneBudgetIsOwnerOnlyAndApprovalIsRetired(t *testing.T) {
	for _, tc := range []struct {
		name, user, role, surface, scope string
		want                             int
	}{
		{"owner", "owner-uuid", "owner", "web", "", 200},
		{"anonymous", "", "", "web", "", 401},
		{"member", "member", "member", "web", "", 403},
		{"revoked-owner", "revoked", "owner", "web", "", 403},
		{"mcp", "owner-uuid", "owner", "mcp", "", 403},
		{"scoped", "owner-uuid", "owner", "web", "nova", 403},
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
			read, err := app.Test(httptest.NewRequest("GET", "/api/v1/phone/budget", nil))
			if err != nil {
				t.Fatal(err)
			}
			defer read.Body.Close()
			if read.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d", read.StatusCode, tc.want)
			}
			if tc.want != 200 {
				if fake.payload != nil {
					t.Fatal("unauthorized Lambda invocation")
				}
				return
			}
			if fake.payload["operation"] != "owner_budget_status" || fake.payload["ownerUserId"] != "owner-uuid" || len(fake.payload) != 2 {
				t.Fatal(fake.payload)
			}
			var body map[string]any
			if err := json.NewDecoder(read.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			// The retired $5 approval never reaches the ledger.
			fake.payload = nil
			req := httptest.NewRequest("POST", "/api/v1/phone/budget/grant", strings.NewReader(`{"requestId":"grant-12345678","expectedDay":"2026-10-16"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Cookie", CSRFCookieName+"=match")
			req.Header.Set(CSRFHeaderName, "match")
			grant, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer grant.Body.Close()
			if grant.StatusCode != 410 || fake.payload != nil {
				t.Fatalf("grant status=%d payload=%v", grant.StatusCode, fake.payload)
			}
		})
	}
}
