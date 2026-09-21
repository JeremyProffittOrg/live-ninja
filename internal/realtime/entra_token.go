package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
)

// Entra tenant for the Voice Live client-credentials exchange
// (azure-voice-plan.md WS-C M1). Identifier, not a secret.
const EntraTenantID = "d0695ba8-1211-4da6-81a4-05427c842a2a"

// VoiceLiveScope is the Entra scope for Microsoft Foundry Voice Live (A7).
const VoiceLiveScope = "https://ai.azure.com/.default"

// EntraToken is an in-memory access token. Never log Value.
type EntraToken struct {
	Value     string
	ExpiresAt time.Time
}

type cachedEntra struct {
	value     string
	refreshAt time.Time
	expiresAt time.Time
}

// EntraTokenClient does the client-credentials grant against login.microsoftonline.com.
// Tokens are cached per scope and refreshed at 80% of observed lifetime.
type EntraTokenClient struct {
	httpc    *http.Client
	loader   *config.Loader
	tenant   string
	tokenURL string // empty → production login.microsoftonline.com
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cachedEntra
}

// NewEntraTokenClient builds a client that reads id+secret from SSM (or the
// AZURE_VOICELIVE_* env overrides). tenant empty uses EntraTenantID.
func NewEntraTokenClient(loader *config.Loader, tenant string) *EntraTokenClient {
	if tenant == "" {
		tenant = EntraTenantID
	}
	return &EntraTokenClient{
		httpc:  &http.Client{Timeout: 10 * time.Second},
		loader: loader,
		tenant: tenant,
		now:    time.Now,
		cache:  make(map[string]cachedEntra),
	}
}

// Token returns a bearer token for scope. Cached unexpired tokens are reused.
// The token value is never included in returned error strings.
func (c *EntraTokenClient) Token(ctx context.Context, scope string) (EntraToken, error) {
	if scope == "" {
		scope = VoiceLiveScope
	}
	now := c.now()
	c.mu.Lock()
	if hit, ok := c.cache[scope]; ok && now.Before(hit.refreshAt) {
		tok := EntraToken{Value: hit.value, ExpiresAt: hit.expiresAt}
		c.mu.Unlock()
		return tok, nil
	}
	c.mu.Unlock()

	id, err := c.loader.Get(ctx, config.ParamVoiceLiveClientID, config.EnvOverrideVoiceLiveClientID)
	if err != nil || strings.TrimSpace(id) == "" {
		return EntraToken{}, fmt.Errorf("entra: client id unavailable")
	}
	secret, err := c.loader.Get(ctx, config.ParamVoiceLiveClientSecret, config.EnvOverrideVoiceLiveClientSecret)
	if err != nil || strings.TrimSpace(secret) == "" {
		return EntraToken{}, fmt.Errorf("entra: client secret unavailable")
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {id},
		"client_secret": {secret},
		"scope":         {scope},
	}
	endpoint := c.tokenURL
	if endpoint == "" {
		endpoint = "https://login.microsoftonline.com/" + c.tenant + "/oauth2/v2.0/token"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return EntraToken{}, fmt.Errorf("entra: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return EntraToken{}, fmt.Errorf("entra: token request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return EntraToken{}, fmt.Errorf("entra: read token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return EntraToken{}, fmt.Errorf("entra: token endpoint status %d", resp.StatusCode)
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return EntraToken{}, fmt.Errorf("entra: decode token response: %w", err)
	}
	if parsed.AccessToken == "" {
		return EntraToken{}, fmt.Errorf("entra: token response missing access_token")
	}
	if parsed.ExpiresIn <= 0 {
		parsed.ExpiresIn = 3600
	}
	lifetime := time.Duration(parsed.ExpiresIn) * time.Second
	expiresAt := now.Add(lifetime)
	refreshAt := now.Add(time.Duration(float64(lifetime) * 0.8))
	c.mu.Lock()
	c.cache[scope] = cachedEntra{value: parsed.AccessToken, refreshAt: refreshAt, expiresAt: expiresAt}
	c.mu.Unlock()
	return EntraToken{Value: parsed.AccessToken, ExpiresAt: expiresAt}, nil
}
