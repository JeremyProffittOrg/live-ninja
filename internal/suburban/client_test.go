package suburban

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/mcpauth"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

type testCrypto struct{ g cipher.AEAD }

func (c testCrypto) Seal(_ context.Context, owner string, b []byte) ([]byte, error) {
	n := make([]byte, c.g.NonceSize())
	_, _ = rand.Read(n)
	return c.g.Seal(n, n, b, []byte(owner)), nil
}
func (c testCrypto) Open(_ context.Context, owner string, b []byte) ([]byte, error) {
	return c.g.Open(nil, b[:c.g.NonceSize()], b[c.g.NonceSize():], []byte(owner))
}

func TestSuburbanOAuthConnectionAndAvailableTools(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	var exchanges, refreshes, calls atomic.Int64
	var failRefresh atomic.Bool
	challenge := ""
	lastRequest := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/register":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["token_endpoint_auth_method"] != "none" {
				t.Error("unexpected client authentication")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"client_id": strings.Repeat("a", 43)})
		case "/oauth/token":
			_ = r.ParseForm()
			if r.Form.Get("resource") != Resource {
				t.Error("missing resource binding")
			}
			if r.Header.Get("Authorization") != "" {
				t.Error("public OAuth client used secret authentication")
			}
			if r.Form.Get("grant_type") == "authorization_code" {
				exchanges.Add(1)
				if mcpauth.Challenge(r.Form.Get("code_verifier")) != challenge || r.Form.Get("redirect_uri") != Callback {
					t.Error("PKCE or redirect mismatch")
				}
			} else {
				refreshes.Add(1)
				if failRefresh.Load() {
					w.WriteHeader(503)
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": strings.Repeat("t", 43), "refresh_token": strings.Repeat("r", 43), "token_type": "Bearer", "expires_in": 900, "scope": "vehicle"})
		case "/mcp":
			if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 43) {
				t.Error("missing resource credential")
			}
			var body struct {
				ID     string
				Method string
				Params struct {
					Name      string
					Arguments map[string]any
				}
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var result any = map[string]any{"protocolVersion": "2025-11-25"}
			switch body.Method {
			case "tools/list":
				result = map[string]any{"tools": []any{map[string]any{"name": "vehicle_status", "annotations": map[string]any{"readOnlyHint": true}}, map[string]any{"name": "vehicle_lock", "annotations": map[string]any{"readOnlyHint": false}}}}
			case "tools/call":
				calls.Add(1)
				if body.Params.Name == "vehicle_lock" {
					lastRequest <- body.Params.Arguments["request_id"].(string)
				}
				result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "synthetic result"}}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
		default:
			t.Error("unexpected endpoint")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	block, _ := aes.NewCipher(make([]byte, 32))
	gcm, _ := cipher.NewGCM(block)
	repo := &mcpauth.Dynamo{Client: testutil.NewFakeDynamo(), Table: "test"}
	s := &Service{Repo: repo, Crypto: testCrypto{gcm}, HTTP: server.Client(), base: server.URL, Now: func() time.Time { return now }}
	if _, e := s.Invoke(ctx, "owner", "vehicle_status", nil, "stable"); !errors.Is(e, ErrConnect) {
		t.Fatal("unconnected request was sent")
	}
	redirect, cookie, e := s.Begin(ctx, "owner")
	if e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse(redirect)
	q := u.Query()
	challenge = q.Get("code_challenge")
	if q.Get("resource") != Resource || q.Get("scope") != "vehicle" || q.Get("code_challenge_method") != "S256" {
		t.Fatal("authorization binding")
	}
	code := strings.Repeat("c", 43)
	if e = s.Complete(ctx, "owner", q.Get("state"), code, strings.Repeat("x", 43), Origin); e == nil {
		t.Fatal("wrong browser accepted")
	}
	if e = s.Complete(ctx, "other", q.Get("state"), code, cookie, Origin); e == nil {
		t.Fatal("wrong owner accepted")
	}
	if exchanges.Load() != 0 {
		t.Fatal("invalid callback contacted token endpoint")
	}
	if e = s.Complete(ctx, "owner", q.Get("state"), code, cookie, Origin); e != nil {
		t.Fatal(e)
	}
	if e = s.Complete(ctx, "owner", q.Get("state"), code, cookie, Origin); e == nil {
		t.Fatal("callback replay accepted")
	}
	r, e := repo.Get(ctx, "suburban-connection:"+mcpauth.Hash("owner"))
	if e != nil || strings.Contains(string(r.Data), strings.Repeat("r", 43)) {
		t.Fatal("grant not encrypted")
	}
	if _, e = s.Invoke(ctx, "owner", "list", nil, "stable"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Invoke(ctx, "owner", "vehicle_unlock", nil, "stable"); !errors.Is(e, ErrInvalid) {
		t.Fatal("unavailable tool accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("unavailable tool reached call endpoint")
	}
	result, e := s.Invoke(ctx, "owner", "vehicle_lock", map[string]any{}, "stable-request-id")
	if e != nil || result["requestId"] != "stable-request-id" || <-lastRequest != "stable-request-id" {
		t.Fatal("command identity changed")
	}
	now = now.Add(16 * time.Minute)
	if _, e = s.Invoke(ctx, "owner", "vehicle_status", nil, "read-request"); e != nil {
		t.Fatal("refresh failed", e)
	}
	if refreshes.Load() != 1 {
		t.Fatal("refresh count")
	}
	now = now.Add(16 * time.Minute)
	failRefresh.Store(true)
	if _, e = s.Invoke(ctx, "owner", "vehicle_status", nil, "read-request"); e == nil {
		t.Fatal("unknown refresh accepted")
	}
	if _, e = s.Invoke(ctx, "owner", "vehicle_status", nil, "read-request"); !errors.Is(e, ErrConnect) {
		t.Fatal("unknown refresh was repeated")
	}
	if refreshes.Load() != 2 {
		t.Fatal("automatic refresh retry")
	}
}
