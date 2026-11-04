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
