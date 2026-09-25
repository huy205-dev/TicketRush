// Command booking serves the public ticketing API.
//
// M0 exposes only /healthz and /readyz; the booking endpoints arrive in M1.
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
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/huy205-dev/ticketrush/internal/httpx"
	"github.com/huy205-dev/ticketrush/internal/platform/config"
	"github.com/huy205-dev/ticketrush/internal/platform/httpserver"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
	"github.com/huy205-dev/ticketrush/internal/platform/postgres"
	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
)

const (
	listenAddr     = ":8080"
	serviceName    = "booking"
	readinessLimit = time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first signal, restore default handling so a second Ctrl+C
	// kills the process instead of waiting for the graceful shutdown.
	go func() {
		<-ctx.Done()
		stop()
	}()

	err := run(ctx, os.LookupEnv, os.Stdout)
	stop()
	if err != nil {
		os.Exit(1)
	}
}

// run wires dependencies and blocks until ctx is cancelled. Errors are
// logged before being returned.
func run(ctx context.Context, lookupEnv func(string) (string, bool), stdout io.Writer) error {
	logger := logging.New(stdout, slog.LevelInfo).With("service", serviceName)

	cfg, err := config.Load(lookupEnv)
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			logger.Error("invalid configuration, see .env.example", "problems", ve.Problems)
		} else {
			logger.Error("load configuration", "err", err)
		}
		return err
	}

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		logger.Error("init postgres", "err", err)
		return err
	}
	defer pool.Close()

	rdb := redisx.NewClient(cfg.RedisAddr)
	defer func() { _ = rdb.Close() }()

	health := httpx.NewHealth(logger, readinessLimit,
		httpx.Check{Name: "postgres", Fn: pool.Ping},
		httpx.Check{Name: "redis", Fn: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
	)

	srv := &http.Server{
		Handler:           newRouter(logger, health),
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

	logger.Info("booking started", "addr", ln.Addr().String(), "config", cfg)
	if err := httpserver.Serve(ctx, srv, ln, httpserver.ShutdownTimeout); err != nil {
		logger.Error("server stopped with error", "err", err)
		return err
	}
	logger.Info("booking stopped")
	return nil
}

func newRouter(logger *slog.Logger, health *httpx.Health) http.Handler {
	r := chi.NewRouter()
	r.Use(
		httpx.RequestID,
		httpx.AccessLog(logger, "/healthz", "/readyz"),
		httpx.Recover(logger),
	)
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.MethodNotAllowed)

	r.Get("/healthz", health.Live)
	r.Get("/readyz", health.Ready)
	return r
}
