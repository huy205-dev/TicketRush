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
	err := run(context.Background(), func(string) (string, bool) { return "", false }, &out)
	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("run error = %v, want *config.ValidationError", err)
	}
	if !strings.Contains(out.String(), "invalid configuration") || !strings.Contains(out.String(), `"service":"`+serviceName+`"`) {
		t.Errorf("startup log does not explain the problem: %s", out.String())
	}
}
