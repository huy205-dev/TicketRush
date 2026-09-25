// Command refunder carries out refunds (SPEC.md 9.7): it consumes
// order.refund_requested, asks fakepay for the refund and records it.
package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/payment"
	"github.com/huy205-dev/ticketrush/internal/platform/app"
	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
	"github.com/huy205-dev/ticketrush/internal/platform/postgres"
)

const (
	serviceName   = "refunder"
	consumerGroup = "refunder"
	// gatewayTimeout bounds one refund call; a timeout is retried.
	gatewayTimeout = 10 * time.Second
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

	refunds := order.NewRefundProcessor(pool, payment.NewClient(cfg.FakepayURL, gatewayTimeout), logger)
	err = kafka.Run(ctx, logger, cfg.KafkaBrokers, consumerGroup, []string{events.TopicOrders}, refunds.Handler())
	if err != nil {
		logger.Error("consumer stopped with error", "err", err)
	}
	return err
}
