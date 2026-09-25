package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/huy205-dev/ticketrush/internal/platform/config"
)

func TestRunRejectsInvalidConfig(t *testing.T) {
	var out bytes.Buffer
	noEnv := func(string) (string, bool) { return "", false }

	err := run(context.Background(), noEnv, &out)

	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("run error = %v, want *config.ValidationError", err)
	}
	log := out.String()
	for _, want := range []string{`"level":"ERROR"`, "invalid configuration", "DATABASE_URL is required", "JWT_SECRET is required"} {
		if !strings.Contains(log, want) {
			t.Errorf("startup log missing %q: %s", want, log)
		}
	}
}

func TestRunRejectsFeaturesOfLaterMilestones(t *testing.T) {
	base := map[string]string{
		"DATABASE_URL":     "postgres://u:p@localhost:1/db",
		"JWT_SECRET":       strings.Repeat("j", 32),
		"ADMISSION_SECRET": strings.Repeat("a", 32),
		"WEBHOOK_SECRET":   strings.Repeat("w", 32),
		"TICKET_SECRET":    strings.Repeat("t", 32),
	}
	tests := []struct {
		name    string
		env     map[string]string
		message string
	}{
		{"admission", map[string]string{"INVENTORY_BACKEND": "pg", "REQUIRE_ADMISSION": "true"}, "REQUIRE_ADMISSION=true needs the waiting room"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			for k, v := range tc.env {
				env[k] = v
			}
			var out bytes.Buffer
			err := run(context.Background(), func(k string) (string, bool) { v, ok := env[k]; return v, ok }, &out)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("run error = %v, want %q", err, tc.message)
			}
			if !strings.Contains(out.String(), "unsupported configuration") {
				t.Errorf("startup log does not explain the problem: %s", out.String())
			}
		})
	}
}

func TestParseIdempotencyKey(t *testing.T) {
	canonical := "0192f7a0-1b2c-7d3e-8f40-123456789abc"
	for raw, ok := range map[string]bool{
		canonical:                              true,
		strings.ToUpper(canonical):             true,
		"{" + canonical + "}":                  true,
		"":                                     false,
		"not-a-uuid":                           false,
		"00000000-0000-0000-0000-000000000000": false,
		canonical + "0":                        false,
	} {
		key, err := parseIdempotencyKey(raw)
		if ok != (err == nil) {
			t.Errorf("parseIdempotencyKey(%q) error = %v, want ok=%v", raw, err, ok)
			continue
		}
		if ok && key.String() != canonical {
			t.Errorf("parseIdempotencyKey(%q) = %s, want canonical %s", raw, key, canonical)
		}
	}
}
