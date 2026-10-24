package mcpauth

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"strings"
	"sync"
	"testing"
	"time"
)

func newMemory() Repository { return &Dynamo{Client: testutil.NewFakeDynamo(), Table: "test"} }
func setup(t *testing.T) (*Service, *Client, Authorization, string) {
	t.Helper()
	s := &Service{Repo: newMemory(), Now: func() time.Time { return time.Unix(1800000000, 0) }, OwnerActive: func(_ context.Context, u string, _ int64) bool { return u == "owner" }}
	client, e := s.Register(context.Background(), "Test client", []string{"https://client.example/callback"}, "phone:read memory:read")
	if e != nil {
		t.Fatal(e)
	}
	v := strings.Repeat("a", 43)
	a := Authorization{ClientID: client.ID, RedirectURI: client.RedirectURIs[0], Resource: Resource, Scope: "phone:read", State: "opaque-state", Challenge: Challenge(v)}
	return s, client, a, v
}
func approve(t *testing.T, s *Service, a Authorization) string {
	t.Helper()
	ctx := context.Background()
	id, _, e := s.Begin(ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	_, csrf, e := s.BindConsent(ctx, id, "owner")
	if e != nil {
		t.Fatal(e)
	}
	_, code, e := s.Consent(ctx, id, csrf, "owner", true)
	if e != nil {
		t.Fatal(e)
	}
	return code
}
func grant(t *testing.T, s *Service, a Authorization, v string) *Tokens {
	t.Helper()
	out, e := s.Exchange(context.Background(), approve(t, s, a), a.ClientID, a.RedirectURI, v, Resource)
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func TestMCPAuthorizationRejectsUnsafeRequests(t *testing.T) {
	for _, kind := range []string{"http", "fragment", "userinfo", "ip", "wrong_redirect", "wrong_resource", "missing_challenge", "plain_challenge", "scope_escalation", "unknown_client"} {
		t.Run(kind, func(t *testing.T) {
			s, _, a, _ := setup(t)
			switch kind {
			case "http":
				a.RedirectURI = "http://client.example/callback"
			case "fragment":
				a.RedirectURI += "#attack"
			case "userinfo":
				a.RedirectURI = "https://evil@client.example/callback"
			case "ip":
				a.RedirectURI = "https://127.0.0.1/callback"
			case "wrong_redirect":
				a.RedirectURI = "https://client.example/callback/"
			case "wrong_resource":
				a.Resource = Issuer + "/api/v1"
			case "missing_challenge":
				a.Challenge = ""
			case "plain_challenge":
				a.Challenge = "123"
			case "scope_escalation":
				a.Scope = "phone:send"
			case "unknown_client":
				a.ClientID = strings.Repeat("z", 43)
			}
			if _, e := s.ValidateAuthorization(context.Background(), a); e == nil {
				t.Fatal("accepted unsafe request")
			}
		})
	}
}

func TestAlexaConfidentialClientIsIsolatedFromMCP(t *testing.T) {
	s, public, _, verifier := setup(t)
	s.AlexaClient = &Client{ID: strings.Repeat("x", 43), Name: "Live Ninja Alexa", RedirectURIs: []string{"https://pitangui.amazon.com/api/skill/link/test"}, Scope: "alexa:chat", Resource: AlexaResource, SecretHash: Hash("test-secret")}
	a := Authorization{ClientID: s.AlexaClient.ID, RedirectURI: s.AlexaClient.RedirectURIs[0], Scope: "alexa:chat", Challenge: Challenge(verifier)}
	ctx := t.Context()
	if _, e := s.AuthenticateClient(ctx, a.ClientID, "wrong", ""); e == nil {
		t.Fatal("accepted wrong secret")
	}
	resource, e := s.AuthenticateClient(ctx, a.ClientID, "test-secret", "")
	if e != nil || resource != AlexaResource {
		t.Fatalf("confidential client: %s %v", resource, e)
	}
	if _, e = s.AuthenticateClient(ctx, public.ID, "test-secret", Resource); e == nil {
		t.Fatal("public client accepted secret")
	}
	if _, e = s.Register(ctx, "fake", aRedirects(), "alexa:chat"); e == nil {
		t.Fatal("dynamic client acquired Alexa scope")
	}
	code := approve(t, s, a)
	tokens, e := s.Exchange(ctx, code, a.ClientID, a.RedirectURI, verifier, resource)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Verify(ctx, tokens.AccessToken); e == nil {
		t.Fatal("Alexa token accepted at MCP")
	}
	id, e := s.VerifyAlexa(ctx, tokens.AccessToken)
	if e != nil || id.UserID != "owner" {
		t.Fatalf("Alexa verify: %+v %v", id, e)
	}
	if _, e = s.VerifyGrant(ctx, id.GrantID, AlexaResource); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Refresh(ctx, tokens.RefreshToken, a.ClientID, Resource, ""); e == nil {
		t.Fatal("cross-resource refresh")
	}
	next, e := s.Refresh(ctx, tokens.RefreshToken, a.ClientID, AlexaResource, "")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Revoke(ctx, next.AccessToken, a.ClientID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.VerifyAlexa(ctx, next.AccessToken); e == nil {
		t.Fatal("revoked access accepted")
	}
	if _, e = s.VerifyGrant(ctx, id.GrantID, AlexaResource); e == nil {
		t.Fatal("queued work retained revoked authority")
	}
}

func aRedirects() []string { return []string{"https://client.example/callback"} }
func TestMCPRegistrationRedirectValidation(t *testing.T) {
	s, _, _, _ := setup(t)
	for _, u := range []string{"http://client.example/cb", "https://client.example/cb#fragment", "https://user@client.example/cb", "https://127.0.0.1/cb", "https://localhost/cb", "https://client.local/cb", "https://client.example/\r\nheader", "https://*.client.example/cb", "https://client.example;sandbox/cb"} {
		if _, e := s.Register(context.Background(), "bad", []string{u}, "phone:read"); e == nil {
			t.Fatalf("accepted %q", u)
		}
	}
}
func TestMCPConsentBoundToOwnerAndNonce(t *testing.T) {
	s, _, a, _ := setup(t)
	ctx := context.Background()
	id, _, _ := s.Begin(ctx, a)
	_, csrf, e := s.BindConsent(ctx, id, "owner")
	if e != nil {
		t.Fatal(e)
	}
	for _, x := range []struct{ id, csrf, user string }{{id, "wrong", "owner"}, {"wrong", csrf, "owner"}, {id, csrf, "member"}} {
		if _, _, e := s.Consent(ctx, x.id, x.csrf, x.user, true); e == nil {
			t.Fatal("bad consent accepted")
		}
	}
	_, fresh, _ := s.BindConsent(ctx, id, "owner")
	if _, _, e := s.Consent(ctx, id, csrf, "owner", true); e == nil {
		t.Fatal("stale form accepted")
	}
	req, code, e := s.Consent(ctx, id, fresh, "owner", false)
	if e != nil || code != "" || req.State != a.State {
		t.Fatal("denial incorrect")
	}
	if _, _, e = s.Consent(ctx, id, fresh, "owner", true); e == nil {
		t.Fatal("consent replay accepted")
	}
}
func TestMCPCodePKCEClientRedirectAndAudience(t *testing.T) {
	for _, kind := range []string{"verifier", "client", "redirect", "resource"} {
		t.Run(kind, func(t *testing.T) {
			s, _, a, v := setup(t)
			code := approve(t, s, a)
			client, redirect, resource := a.ClientID, a.RedirectURI, Resource
			switch kind {
			case "verifier":
				v = strings.Repeat("b", 43)
			case "client":
				client = strings.Repeat("b", 43)
			case "redirect":
				redirect += "/"
			case "resource":
				resource = Issuer
			}
			if _, e := s.Exchange(context.Background(), code, client, redirect, v, resource); e == nil {
				t.Fatal("bad exchange accepted")
			}
		})
	}
}
func TestMCPCodeSingleUseAndConcurrentExchange(t *testing.T) {
	s, _, a, v := setup(t)
	code := approve(t, s, a)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Exchange(context.Background(), code, a.ClientID, a.RedirectURI, v, Resource)
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("success count %d", success)
	}
}
func TestMCPRefreshRotationReplayRevokesAccess(t *testing.T) {
	s, _, a, v := setup(t)
	first := grant(t, s, a, v)
	ctx := context.Background()
	next, e := s.Refresh(ctx, first.RefreshToken, a.ClientID, Resource, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Verify(ctx, next.AccessToken); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Refresh(ctx, first.RefreshToken, a.ClientID, Resource, ""); e == nil {
		t.Fatal("refresh replay accepted")
	}
	for _, token := range []string{first.AccessToken, next.AccessToken} {
		if _, e = s.Verify(ctx, token); e == nil {
			t.Fatal("replay did not revoke family")
		}
	}
}
func TestMCPRefreshRejectsEscalationAndWrongBindings(t *testing.T) {
	for _, kind := range []string{"scope", "client", "resource"} {
		t.Run(kind, func(t *testing.T) {
			s, _, a, v := setup(t)
			tok := grant(t, s, a, v)
			client, scope, resource := a.ClientID, "", Resource
			switch kind {
			case "scope":
				scope = "phone:send"
			case "client":
				client = "other"
			case "resource":
				resource = Issuer
			}
			if _, e := s.Refresh(context.Background(), tok.RefreshToken, client, resource, scope); e == nil {
				t.Fatal("accepted escalation")
			}
			if _, e := s.Verify(context.Background(), tok.AccessToken); e != nil {
				t.Fatal("invalid request revoked valid grant")
			}
		})
	}
}
func TestMCPExplicitRevocationAndActiveOwner(t *testing.T) {
	s, _, a, v := setup(t)
	tok := grant(t, s, a, v)
	ctx := context.Background()
	if _, e := s.Verify(ctx, tok.AccessToken); e != nil {
		t.Fatal(e)
	}
	s.OwnerActive = func(context.Context, string, int64) bool { return false }
	if _, e := s.Verify(ctx, tok.AccessToken); e == nil {
		t.Fatal("disabled owner accepted")
	}
	if _, e := s.Refresh(ctx, tok.RefreshToken, a.ClientID, Resource, ""); e == nil {
		t.Fatal("disabled owner refreshed")
	}
	s.OwnerActive = func(context.Context, string, int64) bool { return true }
	if e := s.Revoke(ctx, tok.RefreshToken, a.ClientID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Verify(ctx, tok.AccessToken); e == nil {
		t.Fatal("revoked token accepted")
	}
}
func TestMCPTokenExpiryEnforcedBeforeDynamoTTLDeletion(t *testing.T) {
	s, _, a, v := setup(t)
	tok := grant(t, s, a, v)
	now := s.now()
	s.Now = func() time.Time { return now.Add(15 * time.Minute) }
	if _, e := s.Verify(context.Background(), tok.AccessToken); e == nil {
		t.Fatal("expired access accepted")
	}
	s.Now = func() time.Time { return now.Add(31 * 24 * time.Hour) }
	if _, e := s.Refresh(context.Background(), tok.RefreshToken, a.ClientID, Resource, ""); e == nil {
		t.Fatal("expired refresh accepted")
	}
}
func TestMCPStoresOnlyCredentialHashes(t *testing.T) {
	s, _, a, v := setup(t)
	ctx := context.Background()
	code := approve(t, s, a)
	tok, e := s.Exchange(ctx, code, a.ClientID, a.RedirectURI, v, Resource)
	if e != nil {
		t.Fatal(e)
	}
	snapshot := map[string]*Record{}
	for _, k := range []string{"code:" + Hash(code), "access:" + Hash(tok.AccessToken), "refresh:" + Hash(tok.RefreshToken)} {
		r, err := s.Repo.Get(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		snapshot[k] = r
	}
	var cred Credential
	json.Unmarshal(snapshot["access:"+Hash(tok.AccessToken)].Data, &cred)
	snapshot["grant:"+cred.GrantID], _ = s.Repo.Get(ctx, "grant:"+cred.GrantID)
	all, _ := json.Marshal(snapshot)
	for _, secret := range []string{code, tok.AccessToken, tok.RefreshToken, v} {
		if strings.Contains(string(all), secret) {
			t.Fatal("raw credential stored")
		}
	}
}
func TestMCPCodeExpiryAndOwnerRevocation(t *testing.T) {
	s, _, a, v := setup(t)
	code := approve(t, s, a)
	now := s.now()
	s.Now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, e := s.Exchange(context.Background(), code, a.ClientID, a.RedirectURI, v, Resource); !errors.Is(e, ErrInvalidGrant) {
		t.Fatal(e)
	}
}
func TestMCPRegistrationRateLimit(t *testing.T) {
	s, _, _, _ := setup(t)
	for i := 0; i < 20; i++ {
		if !s.RegistrationLimit(context.Background(), "client-ip") {
			t.Fatal(i)
		}
	}
	if s.RegistrationLimit(context.Background(), "client-ip") {
		t.Fatal("unbounded registrations")
	}
}

func TestMCPConcurrentRefreshReuseRevokesFamily(t *testing.T) {
	s, _, a, v := setup(t)
	first := grant(t, s, a, v)
	ctx := context.Background()
	var wg sync.WaitGroup
	out := make(chan *Tokens, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokens, _ := s.Refresh(ctx, first.RefreshToken, a.ClientID, Resource, "")
			out <- tokens
		}()
	}
	wg.Wait()
	close(out)
	for token := range out {
		if token != nil {
			if _, e := s.Verify(ctx, token.AccessToken); e == nil {
				t.Fatal("raced refresh family still usable")
			}
		}
	}
	if _, e := s.Verify(ctx, first.AccessToken); e == nil {
		t.Fatal("old access survived replay")
	}
}
