// Command ticket issues tickets for paid orders (SPEC.md 9.6): it consumes
// order.paid, signs a QR payload for every seat and moves the order to
// TICKETED.
package main

import (
	"context"
	"io"
	"os"

	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/platform/app"
	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
	"github.com/huy205-dev/ticketrush/internal/platform/postgres"
)

const (
	serviceName   = "ticket"
	consumerGroup = "ticket"
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

	issuer := order.NewTicketIssuer(pool, cfg.TicketSecret, logger)
	err = kafka.Run(ctx, logger, cfg.KafkaBrokers, consumerGroup, []string{events.TopicOrders}, issuer.Handler())
	if err != nil {
		logger.Error("consumer stopped with error", "err", err)
	}
	return err
}
