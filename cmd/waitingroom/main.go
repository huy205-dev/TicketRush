// Command waitingroom runs the virtual waiting room (SPEC.md 7.2): buyers
// join an event's queue, follow their position over Server-Sent Events, and
// get an admission token when the in-process worker admits them.
package main

import (
	"context"
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
	"github.com/huy205-dev/ticketrush/internal/platform/app"
	"github.com/huy205-dev/ticketrush/internal/platform/httpserver"
	"github.com/huy205-dev/ticketrush/internal/platform/postgres"
	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
	"github.com/huy205-dev/ticketrush/internal/queue"
)

const (
	listenAddr     = ":8081"
	serviceName    = "waitingroom"
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

func run(ctx context.Context, lookupEnv func(string) (string, bool), stdout io.Writer) error {
	cfg, logger, err := app.Setup(serviceName, lookupEnv, stdout)
	if err != nil {
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

	// Only events are read here: the catalog needs no seat inventory.
	cat := catalog.NewService(pool, nil)
	q := queue.New(rdb)
	admissions := auth.NewAdmissionTokens(cfg.AdmissionSecret, cfg.AdmissionTTL)
	worker := queue.NewWorker(q, admissions, cat, cfg.AdmitBatch, logger)

	a := &api{
		logger: logger, tokens: auth.NewTokens(cfg.JWTSecret, auth.AccessTokenTTL), events: cat, queue: q,
		batch: cfg.AdmitBatch, interval: cfg.AdmitInterval, done: make(chan struct{}),
	}
	health := httpx.NewHealth(logger, readinessLimit,
		httpx.Check{Name: "postgres", Fn: pool.Ping},
		httpx.Check{Name: "redis", Fn: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
	)
	srv := &http.Server{
		Handler:           newRouter(logger, health, a),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second, // streams extend it on every write
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	srv.RegisterOnShutdown(func() { close(a.done) })
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		logger.Error("listen", "addr", listenAddr, "err", err)
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}

	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(ctx, cfg.AdmitInterval) }()

	logger.Info("waitingroom started", "addr", ln.Addr().String(), "config", cfg)
	err = httpserver.Serve(ctx, srv, ln, httpserver.ShutdownTimeout)
	<-workerDone
	if err != nil {
		logger.Error("server stopped with error", "err", err)
		return err
	}
	logger.Info("waitingroom stopped")
	return nil
}

func newRouter(logger *slog.Logger, health *httpx.Health, a *api) http.Handler {
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
	a.routes(r)
	return r
}
