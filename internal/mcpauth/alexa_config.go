package mcpauth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

const AlexaClientParameter = "/live-ninja/prod/alexa/oauth-client"

type ParameterReader interface {
	Get(context.Context, string, string) (string, error)
}

// LoadAlexaClient reads deployment-owned configuration. The skill's shared
// secret stays with deployment/SMAPI; the application receives only its hash.
func LoadAlexaClient(ctx context.Context, loader ParameterReader) (*Client, error) {
	if loader == nil {
		return nil, errors.New("Alexa account-link configuration is unavailable")
	}
	raw, err := loader.Get(ctx, AlexaClientParameter, "")
	if err != nil {
		return nil, errors.New("Alexa account-link configuration is unavailable")
	}
	var config struct {
		ID         string   `json:"clientId"`
		SecretHash string   `json:"secretHash"`
		Redirects  []string `json:"redirectUris"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil || len(config.ID) != 43 || len(config.Redirects) == 0 || len(config.Redirects) > 5 {
		return nil, errors.New("Alexa account-link configuration is invalid")
	}
	hash, err := hex.DecodeString(config.SecretHash)
	if err != nil || len(hash) != 32 {
		return nil, errors.New("Alexa account-link hash is invalid")
	}
	for _, redirect := range config.Redirects {
		if !ValidRedirect(redirect) {
			return nil, errors.New("Alexa redirect configuration is invalid")
		}
	}
	return &Client{ID: config.ID, Name: "Live Ninja Alexa", SecretHash: config.SecretHash, RedirectURIs: config.Redirects, Resource: AlexaResource, Scope: "alexa:chat"}, nil
}
