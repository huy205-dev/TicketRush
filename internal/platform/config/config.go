// Package config loads and validates service configuration from environment
// variables. Every binary uses the same Config so a single .env drives the
// whole system; see .env.example for the full list.
package config

import (
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Supported values for APP_ENV.
const (
	EnvDev  = "dev"
	EnvProd = "prod"
)

// Supported values for INVENTORY_BACKEND.
const (
	BackendPG    = "pg"
	BackendRedis = "redis"
)

// MinSecretLen is the minimum length of every HMAC secret. HS256 keys shorter
// than the 256-bit hash output weaken the signature.
const MinSecretLen = 32

// Config holds all runtime settings. Fields map 1:1 to the environment
// variables listed in SPEC.md section 11.
type Config struct {
	AppEnv           string
	DatabaseURL      string
	DBMaxConns       int32
	RedisAddr        string
	KafkaBrokers     []string
	InventoryBackend string
	HoldTTL          time.Duration
	HoldGrace        time.Duration
	AdmissionTTL     time.Duration
	AdmitInterval    time.Duration
	AdmitBatch       int
	RequireAdmission bool
	JWTSecret        string
	AdmissionSecret  string
	WebhookSecret    string
	TicketSecret     string
	FakepayURL       string
	PublicBaseURL    string
	OTLPEndpoint     string
	TelegramBotToken string
	TelegramChatID   string
}

// IsDev reports whether dev-only features (such as dev-login) may be enabled.
func (c *Config) IsDev() bool { return c.AppEnv == EnvDev }

// ValidationError lists every configuration problem found, so the operator
// can fix them all in one go instead of one restart per missing variable.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid configuration: " + strings.Join(e.Problems, "; ")
}

// Load reads the configuration using lookup (normally os.LookupEnv), applies
// defaults and validates the result. It returns a *ValidationError when any
// variable is missing or malformed.
func Load(lookup func(string) (string, bool)) (*Config, error) {
	p := &parser{lookup: lookup}
	c := &Config{
		AppEnv:           p.oneOf("APP_ENV", EnvDev, EnvDev, EnvProd),
		DatabaseURL:      p.required("DATABASE_URL"),
		DBMaxConns:       int32(p.positiveInt("DB_MAX_CONNS", 20, math.MaxInt32)),
		RedisAddr:        p.str("REDIS_ADDR", "localhost:6379"),
		KafkaBrokers:     p.list("KAFKA_BROKERS", "localhost:19092"),
		InventoryBackend: p.oneOf("INVENTORY_BACKEND", BackendRedis, BackendPG, BackendRedis),
		HoldTTL:          p.duration("HOLD_TTL", 10*time.Minute, false),
		HoldGrace:        p.duration("HOLD_GRACE", 30*time.Second, true),
		AdmissionTTL:     p.duration("ADMISSION_TTL", 5*time.Minute, false),
		AdmitInterval:    p.duration("ADMIT_INTERVAL", time.Second, false),
		AdmitBatch:       p.positiveInt("ADMIT_BATCH", 200, math.MaxInt32),
		RequireAdmission: p.boolean("REQUIRE_ADMISSION", true),
		JWTSecret:        p.secret("JWT_SECRET"),
		AdmissionSecret:  p.secret("ADMISSION_SECRET"),
		WebhookSecret:    p.secret("WEBHOOK_SECRET"),
		TicketSecret:     p.secret("TICKET_SECRET"),
		FakepayURL:       p.httpURL("FAKEPAY_URL", "http://localhost:8090"),
		PublicBaseURL:    p.httpURL("PUBLIC_BASE_URL", "http://localhost:8080"),
		OTLPEndpoint:     p.str("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		TelegramBotToken: p.str("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:   p.str("TELEGRAM_CHAT_ID", ""),
	}

	p.distinctSecrets(map[string]string{
		"JWT_SECRET":       c.JWTSecret,
		"ADMISSION_SECRET": c.AdmissionSecret,
		"WEBHOOK_SECRET":   c.WebhookSecret,
		"TICKET_SECRET":    c.TicketSecret,
	})
	if (c.TelegramBotToken == "") != (c.TelegramChatID == "") {
		p.fail("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID must be set together")
	}

	if len(p.problems) > 0 {
		return nil, &ValidationError{Problems: p.problems}
	}
	return c, nil
}

// LogValue implements slog.LogValuer. Secrets are never logged and the
// database password is redacted. Durations are logged as strings ("10m0s")
// because the JSON handler would otherwise print nanoseconds.
func (c *Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("app_env", c.AppEnv),
		slog.String("database_url", redactURL(c.DatabaseURL)),
		slog.Int("db_max_conns", int(c.DBMaxConns)),
		slog.String("redis_addr", c.RedisAddr),
		slog.Any("kafka_brokers", c.KafkaBrokers),
		slog.String("inventory_backend", c.InventoryBackend),
		slog.String("hold_ttl", c.HoldTTL.String()),
		slog.String("hold_grace", c.HoldGrace.String()),
		slog.String("admission_ttl", c.AdmissionTTL.String()),
		slog.String("admit_interval", c.AdmitInterval.String()),
		slog.Int("admit_batch", c.AdmitBatch),
		slog.Bool("require_admission", c.RequireAdmission),
		slog.String("fakepay_url", c.FakepayURL),
		slog.String("public_base_url", c.PublicBaseURL),
		slog.Bool("tracing_enabled", c.OTLPEndpoint != ""),
		slog.Bool("telegram_enabled", c.TelegramBotToken != ""),
	)
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable>"
	}
	return u.Redacted()
}

// parser accumulates problems instead of stopping at the first one.
type parser struct {
	lookup   func(string) (string, bool)
	problems []string
}

func (p *parser) fail(format string, args ...any) {
	p.problems = append(p.problems, fmt.Sprintf(format, args...))
}

// get returns the trimmed value of key, treating empty as unset.
func (p *parser) get(key string) (string, bool) {
	v, ok := p.lookup(key)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (p *parser) str(key, def string) string {
	if v, ok := p.get(key); ok {
		return v
	}
	return def
}

func (p *parser) required(key string) string {
	v, ok := p.get(key)
	if !ok {
		p.fail("%s is required", key)
	}
	return v
}

func (p *parser) secret(key string) string {
	v, ok := p.get(key)
	switch {
	case !ok:
		p.fail("%s is required", key)
	case len(v) < MinSecretLen:
		p.fail("%s must be at least %d characters", key, MinSecretLen)
	}
	return v
}

func (p *parser) distinctSecrets(secrets map[string]string) {
	seen := make(map[string]string, len(secrets))
	// Iterate in a fixed order so the error message is deterministic.
	for _, key := range []string{"JWT_SECRET", "ADMISSION_SECRET", "WEBHOOK_SECRET", "TICKET_SECRET"} {
		v := secrets[key]
		if v == "" {
			continue
		}
		if other, dup := seen[v]; dup {
			p.fail("%s must differ from %s", key, other)
			continue
		}
		seen[v] = key
	}
}

func (p *parser) oneOf(key, def string, allowed ...string) string {
	v := p.str(key, def)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	p.fail("%s must be one of %s, got %q", key, strings.Join(allowed, "|"), v)
	return v
}

func (p *parser) positiveInt(key string, def, maxVal int) int {
	v, ok := p.get(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > maxVal {
		p.fail("%s must be an integer in [1, %d], got %q", key, maxVal, v)
		return def
	}
	return n
}

func (p *parser) duration(key string, def time.Duration, allowZero bool) time.Duration {
	v, ok := p.get(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 || (d == 0 && !allowZero) {
		p.fail("%s must be a positive duration like 10m or 30s, got %q", key, v)
		return def
	}
	return d
}

func (p *parser) boolean(key string, def bool) bool {
	v, ok := p.get(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.fail("%s must be true or false, got %q", key, v)
		return def
	}
	return b
}

func (p *parser) list(key, def string) []string {
	var out []string
	for _, item := range strings.Split(p.str(key, def), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	if len(out) == 0 {
		p.fail("%s must contain at least one entry", key)
	}
	return out
}

func (p *parser) httpURL(key, def string) string {
	v := strings.TrimRight(p.str(key, def), "/")
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		p.fail("%s must be an absolute http(s) URL, got %q", key, v)
	}
	return v
}
