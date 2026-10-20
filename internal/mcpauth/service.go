package mcpauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const Issuer = "https://live.jeremy.ninja"
const Resource = Issuer + "/mcp"

var Scopes = []string{"phone:send", "phone:read", "jobs:read", "jobs:manage", "memory:read", "memory:write", "ghost:read", "ghost:manage"}
var ErrInvalidGrant = errors.New("invalid_grant")
var ErrInvalidClient = errors.New("invalid_client")
var ErrInvalidRequest = errors.New("invalid_request")
var ErrInvalidScope = errors.New("invalid_scope")
var ErrInvalidTarget = errors.New("invalid_target")

type Service struct {
	Repo        Repository
	OwnerActive func(context.Context, string, int64) bool
	Now         func() time.Time
}
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	Scope        string
}
type Authorization struct{ ClientID, RedirectURI, Resource, Scope, State, Challenge string }
type Pending struct {
	Request  Authorization
	CSRFHash string
	UserID   string
	Used     bool
}
type Code struct {
	Request Authorization
	UserID  string
	Issued  int64
	Used    bool
}
type Grant struct {
	ID, UserID, ClientID, Resource, Scope string
	Issued                                int64
	Revoked                               bool
}
type Credential struct {
	GrantID, ClientID, Resource, Scope string
	Used                               bool
}
type Tokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}
type Identity struct{ UserID, GrantID, ClientID, Scope string }

func Random() (string, error) {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
func Hash(v string) string { b := sha256.Sum256([]byte(v)); return hex.EncodeToString(b[:]) }
func Challenge(v string) string {
	b := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func Equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func NormalizeScope(raw string) (string, error) {
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		return "", ErrInvalidScope
	}
	allowed := map[string]bool{}
	for _, v := range Scopes {
		allowed[v] = true
	}
	seen := map[string]bool{}
	for _, p := range parts {
		if !allowed[p] {
			return "", ErrInvalidScope
		}
		seen[p] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return strings.Join(out, " "), nil
}
func scopeSubset(scope, parent string) bool {
	allowed := map[string]bool{}
	for _, v := range strings.Fields(parent) {
		allowed[v] = true
	}
	for _, v := range strings.Fields(scope) {
		if !allowed[v] {
			return false
		}
	}
	return true
}
func ValidRedirect(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && len(raw) <= 2048 && u.Scheme == "https" && regexp.MustCompile("^[A-Za-z0-9.-]+$").MatchString(u.Hostname()) && strings.Contains(u.Hostname(), ".") && net.ParseIP(u.Hostname()) == nil && u.User == nil && u.Fragment == "" && u.Opaque == "" && !strings.ContainsAny(raw, "\r\n\\") && !strings.HasSuffix(strings.ToLower(u.Hostname()), ".local")
}
func (s *Service) Register(ctx context.Context, name string, redirects []string, scope string) (*Client, error) {
	if len(name) > 120 || len(redirects) == 0 || len(redirects) > 5 {
		return nil, ErrInvalidRequest
	}
	for _, u := range redirects {
		if !ValidRedirect(u) {
			return nil, ErrInvalidRequest
		}
	}
	if scope == "" {
		scope = strings.Join(Scopes, " ")
	}
	scope, e := NormalizeScope(scope)
	if e != nil {
		return nil, e
	}
	id, e := Random()
	if e != nil {
		return nil, e
	}
	c := &Client{ID: id, Name: name, RedirectURIs: redirects, Scope: scope}
	e = s.Repo.Commit(ctx, []Change{record("client:"+id, c, s.now().Add(90*24*time.Hour).Unix(), nil)})
	return c, e
}
func (s *Service) Client(ctx context.Context, id string) (*Client, error) {
	if len(id) != 43 {
		return nil, ErrInvalidClient
	}
	var c Client
	if _, e := s.read(ctx, "client:"+id, &c); e != nil {
		return nil, ErrInvalidClient
	}
	return &c, nil
}
func (s *Service) ValidateAuthorization(ctx context.Context, a Authorization) (*Client, error) {
	c, e := s.Client(ctx, a.ClientID)
	if e != nil {
		return nil, e
	}
	found := false
	for _, u := range c.RedirectURIs {
		if a.RedirectURI == u {
			found = true
		}
	}
	if !found {
		return nil, ErrInvalidRequest
	}
	if a.Resource != Resource {
		return nil, ErrInvalidTarget
	}
	if len(a.State) > 2048 || !regexp.MustCompile("^[A-Za-z0-9_-]{43}$").MatchString(a.Challenge) {
		return nil, ErrInvalidRequest
	}
	scope, e := NormalizeScope(a.Scope)
	if e != nil || !scopeSubset(scope, c.Scope) {
		return nil, ErrInvalidScope
	}
	return c, nil
}
func (s *Service) Begin(ctx context.Context, a Authorization) (string, string, error) {
	if _, e := s.ValidateAuthorization(ctx, a); e != nil {
		return "", "", e
	}
	a.Scope, _ = NormalizeScope(a.Scope)
	id, e := Random()
	if e != nil {
		return "", "", e
	}
	csrf, e := Random()
	if e != nil {
		return "", "", e
	}
	p := Pending{Request: a, CSRFHash: Hash(csrf)}
	e = s.Repo.Commit(ctx, []Change{record("pending:"+Hash(id), p, s.now().Add(10*time.Minute).Unix(), nil)})
	return id, csrf, e
}
func (s *Service) Pending(ctx context.Context, id string) (*Pending, error) {
	var p Pending
	_, e := s.read(ctx, "pending:"+Hash(id), &p)
	if e != nil || p.Used {
		return nil, ErrInvalidGrant
	}
	return &p, nil
}

// BindConsent rotates the form nonce on every render, and binds it to the current
// verified browser owner. Switching accounts or reusing an old form cannot approve.
func (s *Service) BindConsent(ctx context.Context, id, user string) (*Pending, string, error) {
	if !s.active(ctx, user, s.now().Unix()) {
		return nil, "", ErrInvalidGrant
	}
	var p Pending
	r, e := s.read(ctx, "pending:"+Hash(id), &p)
	if e != nil || p.Used {
		return nil, "", ErrInvalidGrant
	}
	csrf, e := Random()
	if e != nil {
		return nil, "", e
	}
	p.CSRFHash = Hash(csrf)
	p.UserID = user
	e = s.Repo.Commit(ctx, []Change{record(r.Key, p, r.Expires, r)})
	return &p, csrf, e
}
func (s *Service) Consent(ctx context.Context, id, csrf, user string, approved bool) (*Authorization, string, error) {
	var p Pending
	r, e := s.read(ctx, "pending:"+Hash(id), &p)
	if e != nil || p.Used || p.UserID != user || !Equal(p.CSRFHash, Hash(csrf)) || !s.active(ctx, user, s.now().Unix()) {
		return nil, "", ErrInvalidGrant
	}
	if _, e = s.ValidateAuthorization(ctx, p.Request); e != nil {
		return nil, "", e
	}
	p.Used = true
	changes := []Change{record(r.Key, p, r.Expires, r)}
	raw := ""
	if approved {
		raw, e = Random()
		if e != nil {
			return nil, "", e
		}
		code := Code{Request: p.Request, UserID: user, Issued: s.now().Unix()}
		changes = append(changes, record("code:"+Hash(raw), code, s.now().Add(2*time.Minute).Unix(), nil))
	}
	if e = s.Repo.Commit(ctx, changes); e != nil {
		return nil, "", ErrInvalidGrant
	}
	return &p.Request, raw, nil
}
func (s *Service) active(ctx context.Context, user string, issued int64) bool {
	return s.OwnerActive != nil && s.OwnerActive(ctx, user, issued)
}
func (s *Service) Exchange(ctx context.Context, raw, client, redirect, verifier, resource string) (*Tokens, error) {
	if resource != Resource {
		return nil, ErrInvalidTarget
	}
	if !regexp.MustCompile("^[A-Za-z0-9._~-]{43,128}$").MatchString(verifier) {
		return nil, ErrInvalidGrant
	}
	var code Code
	r, e := s.read(ctx, "code:"+Hash(raw), &code)
	if e != nil || code.Used || code.Request.ClientID != client || code.Request.RedirectURI != redirect || code.Request.Resource != resource || !Equal(code.Request.Challenge, Challenge(verifier)) || !s.active(ctx, code.UserID, code.Issued) {
		return nil, ErrInvalidGrant
	}
	if _, e = s.Client(ctx, client); e != nil {
		return nil, e
	}
	id, e := Random()
	if e != nil {
		return nil, e
	}
	g := Grant{ID: id, UserID: code.UserID, ClientID: client, Resource: resource, Scope: code.Request.Scope, Issued: s.now().Unix()}
	code.Used = true
	expiry := s.now().Add(30 * 24 * time.Hour).Unix()
	return s.issue(ctx, g, nil, expiry, []Change{record(r.Key, code, r.Expires, r)})
}
func (s *Service) issue(ctx context.Context, g Grant, gr *Record, expires int64, changes []Change) (*Tokens, error) {
	a, e := Random()
	if e != nil {
		return nil, e
	}
	r, e := Random()
	if e != nil {
		return nil, e
	}
	a = "ln_mcp_a_" + a
	r = "ln_mcp_r_" + r
	cred := Credential{GrantID: g.ID, ClientID: g.ClientID, Resource: g.Resource, Scope: g.Scope}
	changes = append(changes, record("grant:"+g.ID, g, expires, gr), record("access:"+Hash(a), cred, s.now().Add(15*time.Minute).Unix(), nil), record("refresh:"+Hash(r), cred, expires, nil))
	if e = s.Repo.Commit(ctx, changes); e != nil {
		return nil, ErrInvalidGrant
	}
	return &Tokens{a, "Bearer", 900, r, g.Scope}, nil
}
func (s *Service) Refresh(ctx context.Context, raw, client, resource, scope string) (*Tokens, error) {
	if resource != Resource {
		return nil, ErrInvalidTarget
	}
	var cred Credential
	r, e := s.read(ctx, "refresh:"+Hash(raw), &cred)
	if e != nil || cred.ClientID != client || cred.Resource != resource {
		return nil, ErrInvalidGrant
	}
	var g Grant
	gr, e := s.read(ctx, "grant:"+cred.GrantID, &g)
	if e != nil || g.Revoked || g.ClientID != client || !s.active(ctx, g.UserID, g.Issued) {
		return nil, ErrInvalidGrant
	}
	if cred.Used {
		if e := s.revokeFamily(ctx, g.ID); e != nil {
			return nil, e
		}
		return nil, ErrInvalidGrant
	}
	if _, e = s.Client(ctx, client); e != nil {
		return nil, e
	}
	if scope != "" {
		scope, e = NormalizeScope(scope)
		if e != nil || !scopeSubset(scope, cred.Scope) {
			return nil, ErrInvalidScope
		}
		g.Scope = scope
	}
	cred.Used = true
	out, e := s.issue(ctx, g, gr, gr.Expires, []Change{record(r.Key, cred, r.Expires, r)})
	if e != nil {
		// A simultaneous exchange of the same refresh token revokes its whole family.
		var latest Credential
		if _, readErr := s.read(ctx, r.Key, &latest); readErr == nil && latest.Used {
			var grant Grant
			if _, readErr := s.read(ctx, "grant:"+cred.GrantID, &grant); readErr == nil {
				if revokeErr := s.revokeFamily(ctx, grant.ID); revokeErr != nil {
					return nil, revokeErr
				}
			}
		}
	}
	return out, e
}
func (s *Service) Verify(ctx context.Context, raw string) (*Identity, error) {
	if !strings.HasPrefix(raw, "ln_mcp_a_") || len(raw) != 52 {
		return nil, ErrInvalidGrant
	}
	var cred Credential
	if _, e := s.read(ctx, "access:"+Hash(raw), &cred); e != nil {
		return nil, ErrInvalidGrant
	}
	var g Grant
	if _, e := s.read(ctx, "grant:"+cred.GrantID, &g); e != nil || g.Revoked || g.Resource != Resource || cred.Resource != Resource || g.ClientID != cred.ClientID || !scopeSubset(cred.Scope, g.Scope) || !s.active(ctx, g.UserID, g.Issued) {
		return nil, ErrInvalidGrant
	}
	if _, e := s.Client(ctx, g.ClientID); e != nil {
		return nil, ErrInvalidGrant
	}
	return &Identity{g.UserID, g.ID, g.ClientID, cred.Scope}, nil
}
func (s *Service) revokeGrant(ctx context.Context, r *Record, g Grant) error {
	g.Revoked = true
	return s.Repo.Commit(ctx, []Change{record(r.Key, g, r.Expires, r)})
}
func (s *Service) revokeFamily(ctx context.Context, id string) error {
	for i := 0; i < 3; i++ {
		var g Grant
		r, e := s.read(ctx, "grant:"+id, &g)
		if e != nil {
			return e
		}
		if g.Revoked {
			return nil
		}
		e = s.revokeGrant(ctx, r, g)
		if e == nil {
			return nil
		}
		if !errors.Is(e, ErrConflict) {
			return e
		}
	}
	return ErrConflict
}
func (s *Service) Revoke(ctx context.Context, raw, client string) error {
	prefix := "access:"
	if strings.HasPrefix(raw, "ln_mcp_r_") {
		prefix = "refresh:"
	}
	var cred Credential
	if _, e := s.read(ctx, prefix+Hash(raw), &cred); e != nil || cred.ClientID != client {
		return nil
	}
	for i := 0; i < 3; i++ {
		var g Grant
		r, e := s.read(ctx, "grant:"+cred.GrantID, &g)
		if e != nil || g.Revoked {
			return nil
		}
		e = s.revokeGrant(ctx, r, g)
		if e == nil {
			return nil
		}
		if !errors.Is(e, ErrConflict) {
			return e
		}
	}
	return ErrConflict
}

// RegistrationLimit bounds unauthenticated DCR writes without keeping an IP address.
func (s *Service) RegistrationLimit(ctx context.Context, remote string) bool {
	now := s.now()
	k := "registration:" + Hash(remote) + ":" + now.UTC().Format("2006010215")
	for i := 0; i < 3; i++ {
		r, e := s.Repo.Get(ctx, k)
		if e != nil {
			return false
		}
		n := 0
		if r != nil {
			if json.Unmarshal(r.Data, &n) != nil {
				return false
			}
		}
		if n >= 20 {
			return false
		}
		e = s.Repo.Commit(ctx, []Change{record(k, n+1, now.Add(2*time.Hour).Unix(), r)})
		if e == nil {
			return true
		}
		if !errors.Is(e, ErrConflict) {
			return false
		}
	}
	return false
}
