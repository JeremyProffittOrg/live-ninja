// Package config resolves Live Ninja's runtime configuration: plain
// per-function environment variables (set directly by the SAM template)
// plus SSM Parameter Store SecureString/String values (secrets and
// LWA config, set out-of-band by the deploy workflow — see deploy.md and
// plan.md M0). Agents never see the underlying secret values; the Loader
// only ever fetches them at runtime inside a deployed Lambda.
package config

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// SSM parameter names, fixed by the shared spec (deploy.md / plan.md M0).
// These are created (as empty/placeholder slots) by CloudFormation-adjacent
// tooling and populated with real values by the deploy workflow from
// GitHub secrets/variables — never by application code.
const (
	ParamOpenAIAPIKey     = "/live-ninja/prod/openai/api_key"
	ParamLWAClientID      = "/live-ninja/prod/lwa/client_id"
	ParamLWAClientSecret  = "/live-ninja/prod/lwa/client_secret"
	ParamDeviceCredPepper = "/live-ninja/prod/device/cred_pepper"
)

// Local-dev environment variable overrides for each SSM parameter above.
// When set, Loader.Get returns the env var directly and never calls SSM —
// this lets a developer run any function against `go run` without AWS
// credentials or a deployed stack.
const (
	EnvOverrideOpenAIAPIKey     = "OPENAI_API_KEY"
	EnvOverrideLWAClientID      = "LWA_CLIENT_ID"
	EnvOverrideLWAClientSecret  = "LWA_CLIENT_SECRET"
	EnvOverrideDeviceCredPepper = "DEVICE_CRED_PEPPER"
)

// cacheTTL is how long a resolved SSM parameter value is kept in memory
// before Loader.Get re-fetches it. Five minutes bounds the blast radius of
// a rotated secret while keeping steady-state Lambda invocations from
// hitting SSM on every request.
const cacheTTL = 5 * time.Minute

// ssmAPI is the subset of the SSM client Loader depends on, so tests can
// inject a fake without spinning up real AWS credentials.
type ssmAPI interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

type cacheEntry struct {
	value     string
	expiresAt time.Time
}

// Loader resolves SSM parameter values with an in-memory cache. It is safe
// for concurrent use across goroutines handling separate warm-container
// invocations.
type Loader struct {
	client ssmAPI

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

// NewLoader builds a Loader backed by the ambient AWS config (the Lambda
// execution role's credentials, resolved automatically by the SDK from the
// environment).
func NewLoader(ctx context.Context) (*Loader, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("config: load aws config: %w", err)
	}
	return &Loader{
		client: ssm.NewFromConfig(cfg),
		cache:  make(map[string]cacheEntry),
	}, nil
}

// NewLoaderWithClient allows injecting an ssmAPI implementation (e.g. a
// mock) for tests.
func NewLoaderWithClient(client ssmAPI) *Loader {
	return &Loader{client: client, cache: make(map[string]cacheEntry)}
}

// Get resolves a configuration value. If the process environment has
// envOverride set to a non-empty value, that value is returned immediately
// and SSM is never called — the local-dev escape hatch. Otherwise it
// fetches ssmName from SSM Parameter Store (requesting decryption, so
// SecureString values come back in plaintext) and caches the result for
// cacheTTL.
func (l *Loader) Get(ctx context.Context, ssmName, envOverride string) (string, error) {
	if envOverride != "" {
		if v := os.Getenv(envOverride); v != "" {
			return v, nil
		}
	}

	l.mu.RLock()
	entry, ok := l.cache[ssmName]
	l.mu.RUnlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry.value, nil
	}

	out, err := l.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(ssmName),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", fmt.Errorf("config: get parameter %s: %w", ssmName, err)
	}
	value := aws.ToString(out.Parameter.Value)

	l.mu.Lock()
	l.cache[ssmName] = cacheEntry{value: value, expiresAt: time.Now().Add(cacheTTL)}
	l.mu.Unlock()

	return value, nil
}

// Invalidate drops a cached value, forcing the next Get to re-fetch from
// SSM. Useful after a known credential rotation.
func (l *Loader) Invalidate(ssmName string) {
	l.mu.Lock()
	delete(l.cache, ssmName)
	l.mu.Unlock()
}

// App holds the plain (non-secret) per-function environment configuration
// that the SAM template sets directly on every Lambda's Environment block
// (see the shared spec: TABLE_NAME/LOG_LEVEL everywhere, plus
// DOMAIN_NAME/EMAIL_QUEUE_URL/JWT_KMS_KEY_ID/AUTH_KMS_KEY_ID on the web
// function). None of these values are secret, so they are read directly
// from the environment rather than through the SSM-backed Loader above.
type App struct {
	TableName     string
	LogLevel      string
	DomainName    string
	EmailQueueURL string
	JWTKmsKeyID   string
	AuthKmsKeyID  string
}

// FromEnv reads the App configuration from the process environment,
// applying sane defaults for local development.
func FromEnv() App {
	return App{
		TableName:     getenv("TABLE_NAME", "live-ninja"),
		LogLevel:      getenv("LOG_LEVEL", "info"),
		DomainName:    os.Getenv("DOMAIN_NAME"),
		EmailQueueURL: os.Getenv("EMAIL_QUEUE_URL"),
		JWTKmsKeyID:   os.Getenv("JWT_KMS_KEY_ID"),
		AuthKmsKeyID:  os.Getenv("AUTH_KMS_KEY_ID"),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
