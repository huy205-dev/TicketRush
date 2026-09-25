// Command booking serves the public ticketing API: dev-login, events and
// seat maps, and creating, reading and cancelling orders.
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

	"github.com/huy205-dev/ticketrush/internal/auth"
	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/httpx"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/platform/app"
	"github.com/huy205-dev/ticketrush/internal/platform/config"
	"github.com/huy205-dev/ticketrush/internal/platform/httpserver"
	"github.com/huy205-dev/ticketrush/internal/platform/postgres"
	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
)

const (
	listenAddr     = ":8080"
	serviceName    = "booking"
	readinessLimit = time.Second
)

func main() {
	ctx, stop := app.SignalContext()
	err := run(ctx, os.LookupEnv, os.Stdout)
	stop()
	if err != nil {
		os.Exit(1)
	}
}

// run wires dependencies and blocks until ctx is cancelled. Errors are
// logged before being returned.
func run(ctx context.Context, lookupEnv func(string) (string, bool), stdout io.Writer) error {
	cfg, logger, err := app.Setup(serviceName, lookupEnv, stdout)
	if err != nil {
		return err
	}
	if err := checkSupported(cfg); err != nil {
		logger.Error("unsupported configuration", "err", err)
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

	inv, err := inventory.New(cfg.InventoryBackend, pool, rdb, logger)
	if err != nil {
		logger.Error("init inventory", "err", err)
		return err
	}
	cat := catalog.NewService(pool, inv)
	orders := order.NewService(pool, cat, inv, redisx.NewLocker(rdb),
		order.Config{HoldTTL: cfg.HoldTTL, HoldGrace: cfg.HoldGrace}, logger)
	tokens := auth.NewTokens(cfg.JWTSecret, auth.AccessTokenTTL)
	api := newAPI(logger, pool, tokens, cat, orders, cfg.IsDev())

	srv := &http.Server{
		Handler:           newRouter(logger, health, api),
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

// checkSupported rejects settings whose implementation belongs to a later
// milestone, rather than silently running without them.
func checkSupported(cfg *config.Config) error {
	if cfg.RequireAdmission {
		return errors.New("REQUIRE_ADMISSION=true needs the waiting room (M5); set REQUIRE_ADMISSION=false")
	}
	return nil
}

func newRouter(logger *slog.Logger, health *httpx.Health, api *api) http.Handler {
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
	api.routes(r)
	return r
}
