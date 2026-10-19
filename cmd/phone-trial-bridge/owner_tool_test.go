package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Account actions on owner calls routinely refuse (confirmation_required,
// invalid_args); the registry Result must reach the model, not end the call.
func TestOwnerToolReturnsRegistryRefusalsAndRejectsOthers(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		ok     bool
	}{
		{"success", 200, `{"tool":"job_create","ok":true,"output":{"status":"created"}}`, true},
		{"confirmation", 400, `{"tool":"job_create","ok":false,"error":{"code":"confirmation_required","message":"Read it back"}}`, true},
		{"not found", 404, `{"tool":"job_create","ok":false,"error":{"code":"not_found","message":"missing"}}`, true},
		{"registry forbidden", 403, `{"tool":"job_create","ok":false,"error":{"code":"forbidden","message":"no"}}`, true},
		{"route forbidden", 403, `{"error":"phone_tool_forbidden","message":"This phone call does not support that tool."}`, false},
		{"other tool result", 400, `{"tool":"send_email","ok":false,"error":{"code":"invalid_args","message":"x"}}`, false},
		{"unauthenticated", 401, `{"tool":"job_create","ok":false,"error":{"code":"forbidden","message":"x"}}`, false},
		{"server error", 502, `{"tool":"job_create","ok":false,"error":{"code":"upstream_error","message":"x"}}`, false},
		{"not json", 400, `nope`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fixture()
			s.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/api/v1/tools/invoke" || req.Header.Get("Authorization") != "Bearer token" {
					t.Error("unbound tool request")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
			})}
			got, err := s.ownerTool(context.Background(), "token", "job_create", map[string]any{"tool": "job_create"})
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok %v", err, tc.ok)
			}
			if tc.ok && string(got) != tc.body {
				t.Fatalf("result = %s", got)
			}
		})
	}
}
