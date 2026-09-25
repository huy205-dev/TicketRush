package config

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// validEnv returns the minimal set of variables needed for Load to succeed.
func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":     "postgres://app:s3cret@localhost:5432/app?sslmode=disable",
		"JWT_SECRET":       strings.Repeat("j", MinSecretLen),
		"ADMISSION_SECRET": strings.Repeat("a", MinSecretLen),
		"WEBHOOK_SECRET":   strings.Repeat("w", MinSecretLen),
		"TICKET_SECRET":    strings.Repeat("t", MinSecretLen),
	}
}

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(lookupFrom(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	checks := []struct {
		name      string
		got, want any
	}{
		{"AppEnv", c.AppEnv, EnvDev},
		{"DBMaxConns", c.DBMaxConns, int32(20)},
		{"RedisAddr", c.RedisAddr, "localhost:6379"},
		{"KafkaBrokers", strings.Join(c.KafkaBrokers, ","), "localhost:19092"},
		{"InventoryBackend", c.InventoryBackend, BackendRedis},
		{"HoldTTL", c.HoldTTL, 10 * time.Minute},
		{"HoldGrace", c.HoldGrace, 30 * time.Second},
		{"AdmissionTTL", c.AdmissionTTL, 5 * time.Minute},
		{"AdmitInterval", c.AdmitInterval, time.Second},
		{"AdmitBatch", c.AdmitBatch, 200},
		{"ExpiryInterval", c.ExpiryInterval, 5 * time.Second},
		{"RequireAdmission", c.RequireAdmission, true},
		{"FakepayURL", c.FakepayURL, "http://localhost:8090"},
		{"PublicBaseURL", c.PublicBaseURL, "http://localhost:8080"},
		{"IsDev", c.IsDev(), true},
	}
	for _, tc := range checks {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	env := validEnv()
	env["APP_ENV"] = "prod"
	env["DB_MAX_CONNS"] = "50"
	env["KAFKA_BROKERS"] = " k1:9092, k2:9092 ,"
	env["INVENTORY_BACKEND"] = "pg"
	env["HOLD_TTL"] = "2s"
	env["HOLD_GRACE"] = "0s"
	env["REQUIRE_ADMISSION"] = "false"
	env["FAKEPAY_URL"] = "http://fakepay:8090/"

	c, err := Load(lookupFrom(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.IsDev() {
		t.Error("IsDev = true for APP_ENV=prod")
	}
	if c.DBMaxConns != 50 {
		t.Errorf("DBMaxConns = %d, want 50", c.DBMaxConns)
	}
	if got := strings.Join(c.KafkaBrokers, ","); got != "k1:9092,k2:9092" {
		t.Errorf("KafkaBrokers = %q, want trimmed list", got)
	}
	if c.InventoryBackend != BackendPG {
		t.Errorf("InventoryBackend = %q, want pg", c.InventoryBackend)
	}
	if c.HoldTTL != 2*time.Second || c.HoldGrace != 0 {
		t.Errorf("HoldTTL/HoldGrace = %v/%v, want 2s/0s", c.HoldTTL, c.HoldGrace)
	}
	if c.RequireAdmission {
		t.Error("RequireAdmission = true, want false")
	}
	if c.FakepayURL != "http://fakepay:8090" {
		t.Errorf("FakepayURL = %q, want trailing slash trimmed", c.FakepayURL)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(env map[string]string)
		problem string
	}{
		{"missing database url", func(e map[string]string) { delete(e, "DATABASE_URL") }, "DATABASE_URL is required"},
		{"blank database url", func(e map[string]string) { e["DATABASE_URL"] = "  " }, "DATABASE_URL is required"},
		{"missing secret", func(e map[string]string) { delete(e, "TICKET_SECRET") }, "TICKET_SECRET is required"},
		{"short secret", func(e map[string]string) { e["JWT_SECRET"] = "short" }, "JWT_SECRET must be at least"},
		{"reused secret", func(e map[string]string) { e["WEBHOOK_SECRET"] = e["JWT_SECRET"] }, "WEBHOOK_SECRET must differ from JWT_SECRET"},
		{"bad app env", func(e map[string]string) { e["APP_ENV"] = "staging" }, "APP_ENV must be one of"},
		{"bad backend", func(e map[string]string) { e["INVENTORY_BACKEND"] = "mysql" }, "INVENTORY_BACKEND must be one of"},
		{"bad int", func(e map[string]string) { e["DB_MAX_CONNS"] = "many" }, "DB_MAX_CONNS must be an integer"},
		{"zero int", func(e map[string]string) { e["ADMIT_BATCH"] = "0" }, "ADMIT_BATCH must be an integer"},
		{"int32 overflow", func(e map[string]string) { e["DB_MAX_CONNS"] = "3000000000" }, "DB_MAX_CONNS must be an integer"},
		{"bad duration", func(e map[string]string) { e["HOLD_TTL"] = "10" }, "HOLD_TTL must be a positive duration"},
		{"zero ttl", func(e map[string]string) { e["HOLD_TTL"] = "0s" }, "HOLD_TTL must be a positive duration"},
		{"bad expiry interval", func(e map[string]string) { e["EXPIRY_INTERVAL"] = "soon" }, "EXPIRY_INTERVAL must be a positive duration"},
		{"negative grace", func(e map[string]string) { e["HOLD_GRACE"] = "-1s" }, "HOLD_GRACE must be a positive duration"},
		{"bad bool", func(e map[string]string) { e["REQUIRE_ADMISSION"] = "yes" }, "REQUIRE_ADMISSION must be true or false"},
		{"empty broker list", func(e map[string]string) { e["KAFKA_BROKERS"] = " , " }, "KAFKA_BROKERS must contain"},
		{"relative url", func(e map[string]string) { e["FAKEPAY_URL"] = "localhost:8090" }, "FAKEPAY_URL must be an absolute http(s) URL"},
		{"half telegram", func(e map[string]string) { e["TELEGRAM_BOT_TOKEN"] = "x" }, "must be set together"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv()
			tc.mutate(env)
			_, err := Load(lookupFrom(env))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Load error = %v, want *ValidationError", err)
			}
			if !strings.Contains(ve.Error(), tc.problem) {
				t.Errorf("error %q does not mention %q", ve.Error(), tc.problem)
			}
		})
	}
}

func TestLoadReportsAllProblems(t *testing.T) {
	_, err := Load(lookupFrom(map[string]string{}))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Load error = %v, want *ValidationError", err)
	}
	// DATABASE_URL plus four secrets.
	if len(ve.Problems) != 5 {
		t.Errorf("got %d problems, want 5: %v", len(ve.Problems), ve.Problems)
	}
}

func TestLogValueHidesSecrets(t *testing.T) {
	c, err := Load(lookupFrom(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var sb strings.Builder
	slog.New(slog.NewJSONHandler(&sb, nil)).Info("config", "config", c)
	out := sb.String()

	for _, secret := range []string{c.JWTSecret, c.AdmissionSecret, c.WebhookSecret, c.TicketSecret, "s3cret"} {
		if strings.Contains(out, secret) {
			t.Errorf("log output leaks %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, "app:xxxxx@localhost") {
		t.Errorf("database password not redacted: %s", out)
	}
}
