// Command relay publishes outbox rows to Kafka (SPEC.md 9.5) and creates the
// topics on start if they are missing.
package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/outbox"
	"github.com/huy205-dev/ticketrush/internal/platform/app"
	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
	"github.com/huy205-dev/ticketrush/internal/platform/postgres"
)

const (
	serviceName = "relay"
	// topicRetry is the pause between attempts to create the topics while
	// Kafka is not reachable yet.
	topicRetry   = 2 * time.Second
	topicTimeout = 10 * time.Second
)

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

	cl, err := kafka.NewClient(cfg.KafkaBrokers)
	if err != nil {
		logger.Error("init kafka", "err", err)
		return err
	}
	defer cl.Close()

	// Kafka may start after the relay; keep trying instead of exiting.
	for {
		tctx, cancel := context.WithTimeout(ctx, topicTimeout)
		err := kafka.EnsureTopics(tctx, cl, events.TopicPartitions())
		cancel()
		if err == nil {
			break
		}
		logger.Warn("create topics failed; retrying", "err", err, "brokers", cfg.KafkaBrokers)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(topicRetry):
		}
	}

	logger.Info("relay started", "brokers", cfg.KafkaBrokers)
	err = outbox.NewRelay(pool, cl, logger).Run(ctx)
	logger.Info("relay stopped")
	return err
}
