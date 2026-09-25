// Command fakepay runs the fake payment gateway (SPEC.md 7.3): a payment page
// for buyers, an automatic mode for k6, refunds, and webhooks that arrive
// late, twice or more, and are resent on purpose.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/huy205-dev/ticketrush/internal/fakepay"
	"github.com/huy205-dev/ticketrush/internal/httpx"
	"github.com/huy205-dev/ticketrush/internal/platform/app"
	"github.com/huy205-dev/ticketrush/internal/platform/config"
	"github.com/huy205-dev/ticketrush/internal/platform/httpserver"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
)

const (
	listenAddr  = ":8090"
	serviceName = "fakepay"
)

func main() {
	ctx, stop := app.SignalContext()
	err := run(ctx, os.LookupEnv, os.Stdout)
	stop()
	if err != nil {
		os.Exit(1)
	}
}

// run serves until ctx is cancelled. Webhooks still waiting for their delay
// are dropped on shutdown: fakepay keeps no state across restarts.
func run(ctx context.Context, lookupEnv func(string) (string, bool), stdout io.Writer) error {
	logger := logging.New(stdout, slog.LevelInfo).With("service", serviceName)
	cfg, err := config.LoadFakepay(lookupEnv)
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			logger.Error("invalid configuration, see .env.example", "problems", ve.Problems)
		}
		return err
	}

	gw := fakepay.New(ctx, fakepay.Config{
		WebhookSecret: cfg.WebhookSecret,
		PublicURL:     cfg.PublicURL,
		DelayMin:      cfg.DelayMin,
		DelayMax:      cfg.DelayMax,
		DuplicateRate: cfg.DuplicateRate,
		LateRate:      cfg.LateRate,
		LateExtra:     cfg.LateExtra,
		FailRate:      cfg.FailRate,
	}, logger)

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.AccessLog(logger, "/healthz"), httpx.Recover(logger))
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.MethodNotAllowed)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	gw.Routes(r)

	srv := &http.Server{
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		logger.Error("listen", "addr", listenAddr, "err", err)
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}

	logger.Info("fakepay started", "addr", ln.Addr().String(),
		"public_url", cfg.PublicURL,
		"delay_ms_min", cfg.DelayMin.Milliseconds(), "delay_ms_max", cfg.DelayMax.Milliseconds(),
		"duplicate_rate", cfg.DuplicateRate, "late_rate", cfg.LateRate,
		"late_extra_ms", cfg.LateExtra.Milliseconds(), "fail_rate", cfg.FailRate)
	err = httpserver.Serve(ctx, srv, ln, httpserver.ShutdownTimeout)
	gw.Wait()
	if err != nil {
		logger.Error("server stopped with error", "err", err)
		return err
	}
	logger.Info("fakepay stopped")
	return nil
}
