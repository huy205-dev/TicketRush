// Package app holds the start-up steps every binary shares: signal handling,
// the JSON logger and configuration loading.
package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/huy205-dev/ticketrush/internal/platform/config"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
)

// SignalContext returns a context cancelled by the first SIGINT or SIGTERM.
// After that signal the default handling is restored, so a second Ctrl+C
// kills the process instead of waiting for a graceful stop.
func SignalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// Setup builds the JSON logger for service and loads the configuration. An
// invalid configuration is logged with every problem found, then returned.
func Setup(service string, lookupEnv func(string) (string, bool), stdout io.Writer) (*config.Config, *slog.Logger, error) {
	logger := logging.New(stdout, slog.LevelInfo).With("service", service)
	cfg, err := config.Load(lookupEnv)
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			logger.Error("invalid configuration, see .env.example", "problems", ve.Problems)
		} else {
			logger.Error("load configuration", "err", err)
		}
		return nil, logger, err
	}
	return cfg, logger, nil
}
