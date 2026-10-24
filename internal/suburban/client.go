// Package suburban connects the owner's Live Ninja account to the fixed
// Suburban MCP resource. Provider credentials never enter this application.
package suburban

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/mcpauth"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

const Origin = "https://suburban.jeremy.ninja"
const Resource = Origin + "/mcp"
const Callback = "https://live.jeremy.ninja/auth/suburban/callback"

var ErrConnect = errors.New("connect Suburban Remote in Help")
var ErrUnavailable = errors.New("Suburban request unconfirmed; check command status before another command")
var ErrInvalid = errors.New("invalid Suburban request")
var ErrBusy = errors.New("Suburban connection is refreshing; try again shortly")

type Crypto interface {
	Seal(context.Context, string, []byte) ([]byte, error)
	Open(context.Context, string, []byte) ([]byte, error)
}
type Service struct {
	Repo   mcpauth.Repository
	Crypto Crypto
	HTTP   *http.Client
	base   string
	Now    func() time.Time
}
type kmsCrypto struct {
	client *kms.Client
	key    string
}

func (k kmsCrypto) Seal(ctx context.Context, owner string, data []byte) ([]byte, error) {
	r, e := k.client.Encrypt(ctx, &kms.EncryptInput{KeyId: aws.String(k.key), Plaintext: data, EncryptionContext: map[string]string{"service": "live-ninja-suburban", "owner": owner}})
	if e != nil {
		return nil, ErrUnavailable
	}
	return r.CiphertextBlob, nil
}
func (k kmsCrypto) Open(ctx context.Context, owner string, data []byte) ([]byte, error) {
	r, e := k.client.Decrypt(ctx, &kms.DecryptInput{KeyId: aws.String(k.key), CiphertextBlob: data, EncryptionContext: map[string]string{"service": "live-ninja-suburban", "owner": owner}})
	if e != nil {
		return nil, ErrConnect
	}
	return r.Plaintext, nil
}
func New(cfg aws.Config, repo mcpauth.Repository, key string) (*Service, error) {
	if repo == nil || key == "" {
		return nil, ErrUnavailable
	}
	return &Service{Repo: repo, Crypto: kmsCrypto{kms.NewFromConfig(cfg), key}, HTTP: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: Origin}, nil
}
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func (s *Service) request(ctx context.Context, path, contentType, token string, body []byte, out any) error {
	req, e := http.NewRequestWithContext(ctx, "POST", s.base+path, bytes.NewReader(body))
	if e != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json, text/event-stream")
	if path == "/mcp" {
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, e := s.HTTP.Do(req)
	if e != nil {
		return ErrUnavailable
	}
	defer res.Body.Close()
	data, e := io.ReadAll(io.LimitReader(res.Body, 256*1024+1))
	if e != nil || len(data) > 256*1024 {
		return ErrUnavailable
	}
	if res.StatusCode == 400 || res.StatusCode == 401 || res.StatusCode == 403 {
		return ErrConnect
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return ErrUnavailable
	}
	if json.Unmarshal(data, out) != nil {
		return ErrUnavailable
	}
	return nil
}
func (s *Service) load(ctx context.Context, owner, key string, out any) (*mcpauth.Record, error) {
	r, e := s.Repo.Get(ctx, key)
	if e != nil {
		return nil, ErrUnavailable
	}
	if r == nil || r.Expires <= s.now().Unix() {
		return nil, ErrConnect
	}
	sealed, e := base64.RawStdEncoding.DecodeString(string(r.Data))
	if e != nil {
		return nil, ErrConnect
	}
	plain, e := s.Crypto.Open(ctx, owner, sealed)
	if e != nil {
		return nil, e
	}
	defer clear(plain)
	if json.Unmarshal(plain, out) != nil {
		return nil, ErrConnect
	}
	return r, nil
}
func (s *Service) change(ctx context.Context, owner, key string, value any, expires int64, before *mcpauth.Record) (mcpauth.Change, error) {
	plain, e := json.Marshal(value)
	if e != nil || len(plain) > 4096 {
		return mcpauth.Change{}, ErrUnavailable
	}
	defer clear(plain)
	sealed, e := s.Crypto.Seal(ctx, owner, plain)
	if e != nil {
		return mcpauth.Change{}, ErrUnavailable
	}
	v := int64(1)
	if before != nil {
		v = before.Version + 1
	}
	return mcpauth.Change{Before: before, After: mcpauth.Record{Key: key, Version: v, Expires: expires, Data: []byte(base64.RawStdEncoding.EncodeToString(sealed))}}, nil
}

type pending struct {
	Owner, Client, Verifier, CookieHash string
	Used                                bool
}
type connection struct {
	Client, Access, Refresh string
	AccessExpires           int64
	Refreshing              bool
}
type tokenResponse struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Type    string `json:"token_type"`
	Expires int64  `json:"expires_in"`
	Scope   string `json:"scope"`
}

var handle = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func (s *Service) Begin(ctx context.Context, owner string) (redirect, cookie string, err error) {
	if owner == "" {
		return "", "", ErrConnect
	}
	var client struct {
		ID string `json:"client_id"`
	}
	body, _ := json.Marshal(map[string]any{"client_name": "Live Ninja", "redirect_uris": []string{Callback}, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
	if e := s.request(ctx, "/oauth/register", "application/json", "", body, &client); e != nil {
		return "", "", e
	}
	if !handle.MatchString(client.ID) {
		return "", "", ErrUnavailable
	}
	state, e := mcpauth.Random()
	if e != nil {
		return "", "", ErrUnavailable
	}
	verifier, e := mcpauth.Random()
	if e != nil {
		return "", "", ErrUnavailable
	}
	cookie, e = mcpauth.Random()
	if e != nil {
		return "", "", ErrUnavailable
	}
	c, e := s.change(ctx, owner, "suburban-pending:"+mcpauth.Hash(state), pending{Owner: owner, Client: client.ID, Verifier: verifier, CookieHash: mcpauth.Hash(cookie)}, s.now().Add(10*time.Minute).Unix(), nil)
	if e != nil {
		return "", "", e
	}
	if s.Repo.Commit(ctx, []mcpauth.Change{c}) != nil {
		return "", "", ErrUnavailable
	}
	q := url.Values{"response_type": {"code"}, "client_id": {client.ID}, "redirect_uri": {Callback}, "state": {state}, "resource": {Resource}, "scope": {"vehicle"}, "code_challenge": {mcpauth.Challenge(verifier)}, "code_challenge_method": {"S256"}}
	return s.base + "/oauth/authorize?" + q.Encode(), cookie, nil
}
func (s *Service) Complete(ctx context.Context, owner, state, code, cookie, issuer string) error {
	if owner == "" || !handle.MatchString(state) || !handle.MatchString(cookie) || !handle.MatchString(code) || issuer != Origin {
		return ErrInvalid
	}
	var p pending
	r, e := s.load(ctx, owner, "suburban-pending:"+mcpauth.Hash(state), &p)
	if e != nil {
		return e
	}
	if p.Used || p.Owner != owner || !mcpauth.Equal(p.CookieHash, mcpauth.Hash(cookie)) {
		return ErrInvalid
	}
	p.Used = true
	used, e := s.change(ctx, owner, r.Key, p, r.Expires, r)
	if e != nil {
		return e
	}
	if s.Repo.Commit(ctx, []mcpauth.Change{used}) != nil {
		return ErrInvalid
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {p.Client}, "code": {code}, "redirect_uri": {Callback}, "code_verifier": {p.Verifier}, "resource": {Resource}}
	var token tokenResponse
	if e = s.request(ctx, "/oauth/token", "application/x-www-form-urlencoded", "", []byte(form.Encode()), &token); e != nil {
		return e
	}
	if !validToken(token) {
		return ErrUnavailable
	}
	key := "suburban-connection:" + mcpauth.Hash(owner)
	before, e := s.Repo.Get(ctx, key)
	if e != nil {
		return ErrUnavailable
	}
	c, e := s.change(ctx, owner, key, connection{Client: p.Client, Access: token.Access, Refresh: token.Refresh, AccessExpires: s.now().Unix() + token.Expires}, s.now().Add(30*24*time.Hour).Unix(), before)
	if e != nil {
		return e
	}
	if s.Repo.Commit(ctx, []mcpauth.Change{c}) != nil {
		return ErrUnavailable
	}
	return nil
}
func validToken(t tokenResponse) bool {
	return handle.MatchString(t.Access) && handle.MatchString(t.Refresh) && t.Type == "Bearer" && t.Scope == "vehicle" && t.Expires > 0 && t.Expires <= 900
}
func (s *Service) access(ctx context.Context, owner string) (string, error) {
	var c connection
	r, e := s.load(ctx, owner, "suburban-connection:"+mcpauth.Hash(owner), &c)
	if e != nil {
		return "", e
	}
	if c.Refreshing {
		return "", ErrConnect
	}
	if c.AccessExpires > s.now().Add(time.Minute).Unix() {
		return c.Access, nil
	}
	c.Refreshing = true
	claim, e := s.change(ctx, owner, r.Key, c, r.Expires, r)
	if e != nil {
		return "", e
	}
	if s.Repo.Commit(ctx, []mcpauth.Change{claim}) != nil {
		return "", ErrBusy
	}
	var token tokenResponse
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {c.Client}, "refresh_token": {c.Refresh}, "resource": {Resource}}
	if e = s.request(ctx, "/oauth/token", "application/x-www-form-urlencoded", "", []byte(form.Encode()), &token); e != nil {
		return "", e
	}
	if !validToken(token) {
		return "", ErrUnavailable
	}
	c.Access, c.Refresh, c.AccessExpires, c.Refreshing = token.Access, token.Refresh, s.now().Unix()+token.Expires, false
	next, e := s.change(ctx, owner, r.Key, c, s.now().Add(30*24*time.Hour).Unix(), &claim.After)
	if e != nil {
		return "", e
	}
	if s.Repo.Commit(ctx, []mcpauth.Change{next}) != nil {
		return "", ErrConnect
	}
	return c.Access, nil
}
func (s *Service) rpc(ctx context.Context, access, method string, params any, out any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "live-ninja", "method": method, "params": params})
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if e := s.request(ctx, "/mcp", "application/json", access, body, &envelope); e != nil {
		return e
	}
	if len(envelope.Error) > 0 || len(envelope.Result) == 0 || json.Unmarshal(envelope.Result, out) != nil {
		return ErrUnavailable
	}
	return nil
}

type RemoteTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations struct {
		ReadOnly bool `json:"readOnlyHint"`
	} `json:"annotations"`
}

func (s *Service) Invoke(ctx context.Context, owner, name string, args map[string]any, requestID string) (map[string]any, error) {
	access, e := s.access(ctx, owner)
	if e != nil {
		return nil, e
	}
	var initialized map[string]any
	if e = s.rpc(ctx, access, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "live-ninja", "version": "1"}}, &initialized); e != nil {
		return nil, e
	}
	var listed struct {
		Tools []RemoteTool `json:"tools"`
	}
	if e = s.rpc(ctx, access, "tools/list", map[string]any{}, &listed); e != nil {
		return nil, e
	}
	if name == "list" {
		return map[string]any{"tools": listed.Tools}, nil
	}
	var selected *RemoteTool
	for i := range listed.Tools {
		if listed.Tools[i].Name == name {
			selected = &listed.Tools[i]
			break
		}
	}
	if selected == nil {
		return nil, ErrInvalid
	}
	if args == nil {
		args = map[string]any{}
	}
	if !selected.Annotations.ReadOnly {
		if requestID == "" {
			return nil, ErrInvalid
		}
		if _, provided := args["request_id"]; provided {
			return nil, ErrInvalid
		}
		args["request_id"] = requestID
	}
	var result map[string]any
	if e = s.rpc(ctx, access, "tools/call", map[string]any{"name": name, "arguments": args}, &result); e != nil {
		return map[string]any{"requestId": requestID, "outcome": "unconfirmed"}, e
	}
	if !selected.Annotations.ReadOnly {
		result["requestId"] = requestID
	}
	return result, nil
}
