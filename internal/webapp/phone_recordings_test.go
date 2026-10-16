package webapp

import (
	"context"
	"errors"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
	"github.com/gofiber/fiber/v2"
	"net/http/httptest"
	"strings"
	"testing"
)

type recordingServiceFake struct {
	owner, source, sid, disposition string
	calls                           int
	url                             string
	err                             *tools.ToolError
}

func (f *recordingServiceFake) Recordings(_ context.Context, owner, source, cursor string, limit int) (map[string]any, *tools.ToolError) {
	f.owner = owner
	f.source = source
	f.calls++
	return map[string]any{"items": []any{}}, f.err
}
func (f *recordingServiceFake) RecordingMedia(_ context.Context, owner, source, sid, disposition string) (map[string]any, *tools.ToolError) {
	f.owner = owner
	f.source = source
	f.sid = sid
	f.disposition = disposition
	f.calls++
	return map[string]any{"url": f.url, "expiresIn": 300}, f.err
}
func recordingTestApp(f *recordingServiceFake, uid, role, scope string, authErr error) *fiber.App {
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, uid)
		c.Locals(localRole, role)
		c.Locals(localScope, scope)
		return c.Next()
	})
	registerPhoneRecordingAPI(app, f, func(context.Context, string) error { return authErr })
	return app
}
func TestPhoneRecordingsRequireFreshOwner(t *testing.T) {
	for _, tc := range []struct {
		uid, role, scope string
		err              error
		status           int
	}{
		{"", "", "", nil, 401}, {"owner", "user", "", nil, 403}, {"owner", "owner", "mcp", nil, 403}, {"owner", "owner", "", errAPIUserNotAllowed, 403}, {"owner", "owner", "", errors.New("store down"), 503},
	} {
		f := &recordingServiceFake{}
		app := recordingTestApp(f, tc.uid, tc.role, tc.scope, tc.err)
		res, e := app.Test(httptest.NewRequest("GET", "/api/v1/phone/recordings", nil))
		if e != nil || res.StatusCode != tc.status || f.calls != 0 {
			t.Fatal(res, e, f.calls)
		}
	}
	f := &recordingServiceFake{}
	app := recordingTestApp(f, "authenticated-owner", "owner", "", nil)
	res, e := app.Test(httptest.NewRequest("GET", "/api/v1/phone/recordings?ownerUserId=attacker", nil))
	if e != nil || res.StatusCode != 200 || f.owner != "authenticated-owner" || f.source != "messages" || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatal(res, e, f)
	}
}
func TestPhoneRecordingMediaValidation(t *testing.T) {
	sid := "CA" + strings.Repeat("1", 32)
	f := &recordingServiceFake{url: "https://live-ninja-deliverables-759775734231.s3.us-east-1.amazonaws.com/phone-trial/audio.mp3?signature=test"}
	app := recordingTestApp(f, "owner", "owner", "", nil)
	for _, p := range []string{"/api/v1/phone/recordings?limit=0", "/api/v1/phone/recordings?limit=51", "/api/v1/phone/recordings?source=../other", "/api/v1/phone/recordings/wrong/media", "/api/v1/phone/recordings/" + sid + "/media?disposition=other"} {
		res, e := app.Test(httptest.NewRequest("GET", p, nil))
		if e != nil || res.StatusCode != 400 {
			t.Fatal(p, res, e)
		}
	}
	if f.calls != 0 {
		t.Fatal("Invalid request reached service")
	}
	path := "/api/v1/phone/recordings/" + sid + "/media?disposition=attachment&source=messages"
	res, e := app.Test(httptest.NewRequest("GET", path, nil))
	if e != nil || res.StatusCode != 200 || f.disposition != "attachment" || f.sid != sid {
		t.Fatal(res, e, f)
	}
	for _, raw := range []string{"https://attacker.example/audio?sig=test", "http://live-ninja-deliverables-759775734231.s3.amazonaws.com/a?sig=test", "https://live-ninja-deliverables-759775734231.s3.amazonaws.com/a", "https://user@live-ninja-deliverables-759775734231.s3.amazonaws.com/a?sig=test"} {
		f.url = raw
		res, e = app.Test(httptest.NewRequest("GET", path, nil))
		if e != nil || res.StatusCode != 502 {
			t.Fatal(raw, res, e)
		}
	}
}
func TestPhoneRecordingCSPIsHistoryOnly(t *testing.T) {
	app := fiber.New()
	app.Use(SecurityHeaders(nil))
	app.Get("/*", func(c *fiber.Ctx) error { return c.Type("html").SendString("<p>page</p>") })
	for _, path := range []string{"/history", "/conversation"} {
		res, e := app.Test(httptest.NewRequest("GET", path, nil))
		if e != nil {
			t.Fatal(e)
		}
		allowed := strings.Contains(res.Header.Get("Content-Security-Policy"), "media-src 'self' blob: https://live-ninja-deliverables-")
		if allowed != (path == "/history") {
			t.Fatal(path, res.Header)
		}
	}
}
