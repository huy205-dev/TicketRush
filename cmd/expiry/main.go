// Command expiry moves HELD orders past their deadline to EXPIRED and
// releases their seats (SPEC.md 9.4). Running several instances is safe.
package main

import (
	"context"
	"io"
	"os"

	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/platform/app"
	"github.com/huy205-dev/ticketrush/internal/platform/postgres"
	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
)

const serviceName = "expiry"

func main() {
	ctx, stop := app.SignalContext()
	err := run(ctx, os.LookupEnv, os.Stdout)
	stop()
	if err != nil {
		os.Exit(1)
	}
}

// run blocks until ctx is cancelled; a batch in progress is finished first.
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

	inv, err := inventory.New(cfg.InventoryBackend, pool, rdb, logger)
	if err != nil {
		logger.Error("init inventory", "err", err)
		return err
	}

	logger.Info("expiry started", "interval", cfg.ExpiryInterval.String(), "inventory_backend", cfg.InventoryBackend)
	err = order.NewExpirer(pool, inv, logger).Run(ctx, cfg.ExpiryInterval)
	logger.Info("expiry stopped")
	return err
}
