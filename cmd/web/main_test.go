package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPAppAcceptsGatewaySizedHeaders(t *testing.T) {
	for _, header := range []string{"Cookie", "Authorization"} {
		t.Run(header, func(t *testing.T) {
			app := newHTTPApp(nil)
			app.Get("/healthz", healthzHandler)
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			if header == "Cookie" {
				req.AddCookie(&http.Cookie{Name: "first", Value: strings.Repeat("x", 3500)})
				req.AddCookie(&http.Cookie{Name: "second", Value: strings.Repeat("x", 3500)})
			} else {
				req.Header.Set(header, "Bearer "+strings.Repeat("x", 7000))
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("gateway-sized %s header rejected before the handler: status %d", header, resp.StatusCode)
			}
		})
	}
}
