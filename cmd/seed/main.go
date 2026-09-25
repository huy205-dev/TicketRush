// Command seed creates the demo event with 5,000 seats (SPEC.md M1).
//
// Each run creates a new event and prints its id. Run `make db-reset` first
// for a clean database where the event gets id 1.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
	"github.com/huy205-dev/ticketrush/internal/platform/postgres"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.LookupEnv); err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, lookupEnv func(string) (string, bool)) error {
	spec := catalog.DemoEvent()
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	fs.StringVar(&spec.Name, "name", spec.Name, "event name")
	fs.StringVar(&spec.Venue, "venue", spec.Venue, "venue")
	fs.DurationVar(&spec.SaleOpensIn, "sale-opens-in", spec.SaleOpensIn, "delay before the sale opens, e.g. 10m (0 = open now)")
	fs.DurationVar(&spec.StartsIn, "starts-in", spec.StartsIn, "delay before the event starts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if spec.SaleOpensIn < 0 || spec.StartsIn <= spec.SaleOpensIn {
		return errors.New("need 0 <= -sale-opens-in < -starts-in")
	}

	// Seeding needs only the database, not the full service configuration.
	dsn, ok := lookupEnv("DATABASE_URL")
	if !ok || dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	pool, err := postgres.NewPool(ctx, dsn, 2)
	if err != nil {
		return err
	}
	defer pool.Close()

	logger := logging.New(os.Stdout, slog.LevelInfo).With("service", "seed")
	start := time.Now()
	eventID, err := catalog.Seed(ctx, pool, spec)
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "event seeded",
		"event_id", eventID,
		"seats", len(spec.Seats()),
		"sale_opens_in", spec.SaleOpensIn.String(),
		"took_ms", time.Since(start).Milliseconds(),
	)
	return nil
}
