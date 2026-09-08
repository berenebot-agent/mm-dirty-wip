package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr          string
	InboundListenAddr   string
	BaseURL             string
	DataDir             string
	Mode                string
	AllowRegistration   bool
	TrustProxyHeaders   bool
	AppEncryptionKey    string
	MailgunSigningKey   string
	CloudflareSecret    string
	MaxMessageBytes     int64
	DefaultQuotaBytes   int64
	SessionTTL          time.Duration
	RelayEnrollTTL      time.Duration
	RelayRequireBearer  bool
	LoginLimitPerMinute int
	SendLimitPerMinute  int
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr:          env("LISTEN_ADDR", ":8081"),
		InboundListenAddr:   env("INBOUND_LISTEN_ADDR", ""),
		BaseURL:             strings.TrimRight(env("BASE_URL", "http://localhost:8081"), "/"),
		DataDir:             env("DATA_DIR", "/data"),
		Mode:                strings.ToLower(env("MODE", "selfhosted")),
		AllowRegistration:   envBool("ALLOW_REGISTRATION", false),
		TrustProxyHeaders:   envBool("TRUST_PROXY_HEADERS", false),
		AppEncryptionKey:    strings.TrimSpace(os.Getenv("APP_ENCRYPTION_KEY")),
		MailgunSigningKey:   strings.TrimSpace(os.Getenv("MAILGUN_SIGNING_KEY")),
		CloudflareSecret:    strings.TrimSpace(os.Getenv("CLOUDFLARE_WEBHOOK_SECRET")),
		MaxMessageBytes:     envInt64("MAX_MESSAGE_BYTES", 30<<20),
		DefaultQuotaBytes:   envInt64("DEFAULT_STORAGE_QUOTA_BYTES", 100<<20),
		SessionTTL:          time.Duration(envInt("SESSION_TTL_HOURS", 24*14)) * time.Hour,
		RelayEnrollTTL:      time.Duration(envInt("RELAY_ENROLL_TTL_MINUTES", 15)) * time.Minute,
		RelayRequireBearer:  envBool("RELAY_REQUIRE_CALLER_AUTH", false),
		LoginLimitPerMinute: envInt("LOGIN_LIMIT_PER_MINUTE", 10),
		SendLimitPerMinute:  envInt("SEND_LIMIT_PER_MINUTE", 60),
	}
	if cfg.AppEncryptionKey == "" {
		return Config{}, fmt.Errorf("APP_ENCRYPTION_KEY is required")
	}
	if cfg.Mode != "selfhosted" && cfg.Mode != "hosted" {
		return Config{}, fmt.Errorf("MODE must be selfhosted or hosted")
	}
	if cfg.InboundListenAddr != "" && cfg.InboundListenAddr == cfg.ListenAddr {
		return Config{}, fmt.Errorf("INBOUND_LISTEN_ADDR must differ from LISTEN_ADDR")
	}
	if cfg.MaxMessageBytes < 1<<20 {
		return Config{}, fmt.Errorf("MAX_MESSAGE_BYTES is too small")
	}
	return cfg, nil
}

func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
func envBool(name string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}
func envInt(name string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return fallback
	}
	return v
}
func envInt64(name string, fallback int64) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil {
		return fallback
	}
	return v
}
