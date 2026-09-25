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
