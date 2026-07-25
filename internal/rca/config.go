package rca

import (
	"errors"
	"os"
	"strconv"
	"time"
)

// Environment variable names the analyzer reads. Every one is populated by
// template.yaml's RcaAnalyzerFunction; RCA_MODEL_ID is the only required one
// (it comes from the RcaBedrockModelId Parameter, and the model id is NEVER
// hardcoded in Go — a Bedrock generation rename must be a stack parameter
// change, not a code deploy).
const (
	EnvModelID          = "RCA_MODEL_ID"
	EnvMaxOutputTokens  = "RCA_MAX_OUTPUT_TOKENS"
	EnvModelTimeout     = "RCA_MODEL_TIMEOUT_SECONDS"
	EnvDailyCap         = "RCA_DAILY_CAP"
	EnvCooldownMinutes  = "RCA_COOLDOWN_MINUTES"
	EnvEmailTo          = "RCA_EMAIL_TO"
	EnvEmailFrom        = "RCA_EMAIL_FROM"
	EnvEmailReplyTo     = "RCA_EMAIL_REPLY_TO"
	EnvConfigurationSet = "EMAIL_CONFIGURATION_SET"
)

// Defaults for everything except the model id. Each is defensible on its own
// so a partial deploy (a Parameter added but not wired into Environment)
// degrades rather than crash-loops.
const (
	defaultMaxOutputTokens = int32(2000)
	defaultModelTimeout    = 120 * time.Second
	defaultDailyCap        = 10
	defaultCooldown        = 60 * time.Minute

	// noticeWindow is how long an operational notice (model unavailable,
	// malformed response) suppresses the next one. Fixed at 24h: these are
	// "there is a pending owner action" mails, and a flooded queue must not
	// turn one pending action into a mailbox full of identical reminders.
	noticeWindow = 24 * time.Hour
)

// Config is the analyzer's resolved environment.
type Config struct {
	ModelID          string        // RCA_MODEL_ID (required)
	MaxOutputTokens  int32         // RCA_MAX_OUTPUT_TOKENS, default 2000
	ModelTimeout     time.Duration // RCA_MODEL_TIMEOUT_SECONDS, default 120s
	DailyCap         int           // RCA_DAILY_CAP, default 10
	Cooldown         time.Duration // RCA_COOLDOWN_MINUTES, default 60m
	EmailTo          string        // RCA_EMAIL_TO, default DefaultRecipient
	EmailFrom        string        // RCA_EMAIL_FROM, default DefaultFromAddress
	EmailReplyTo     string        // RCA_EMAIL_REPLY_TO, default DefaultReplyTo
	ConfigurationSet string        // EMAIL_CONFIGURATION_SET (optional)

	// NoticeWindow is how long one operational notice suppresses the next.
	// A field rather than a bare const so tests can shrink it.
	NoticeWindow time.Duration
}

// ConfigFromEnv resolves Config, erroring only on a missing RCA_MODEL_ID.
// That one is fatal on purpose: there is no defensible default model id (the
// exact Opus generation must be read out of the account, see the
// RcaBedrockModelId Parameter), and silently falling back to some other model
// would mean paying for an analysis nobody asked for from an analyst nobody
// chose. Every other value has a real default.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		ModelID:          os.Getenv(EnvModelID),
		MaxOutputTokens:  defaultMaxOutputTokens,
		ModelTimeout:     defaultModelTimeout,
		DailyCap:         defaultDailyCap,
		Cooldown:         defaultCooldown,
		EmailTo:          envOr(EnvEmailTo, DefaultRecipient),
		EmailFrom:        envOr(EnvEmailFrom, DefaultFromAddress),
		EmailReplyTo:     envOr(EnvEmailReplyTo, DefaultReplyTo),
		ConfigurationSet: os.Getenv(EnvConfigurationSet),
		NoticeWindow:     noticeWindow,
	}
	if cfg.ModelID == "" {
		return Config{}, errors.New("rca: " + EnvModelID + " is required (set from the RcaBedrockModelId stack parameter)")
	}
	if n, ok := envInt(EnvMaxOutputTokens); ok && n > 0 {
		cfg.MaxOutputTokens = int32(n)
	}
	if n, ok := envInt(EnvModelTimeout); ok && n > 0 {
		cfg.ModelTimeout = time.Duration(n) * time.Second
	}
	if n, ok := envInt(EnvDailyCap); ok && n > 0 {
		cfg.DailyCap = n
	}
	if n, ok := envInt(EnvCooldownMinutes); ok && n > 0 {
		cfg.Cooldown = time.Duration(n) * time.Minute
	}
	return cfg, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// envInt parses an integer env var. An unparseable value is treated as
// "unset" (the default applies) rather than as a fatal error: a typo in a
// tuning knob must not take the analyzer down.
func envInt(name string) (int, bool) {
	v := os.Getenv(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}
