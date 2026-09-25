// Package postgres creates the pgx connection pool shared by a process.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool parses databaseURL and returns a pool capped at maxConns.
//
// Connections are opened lazily, so the pool is returned even when
// PostgreSQL is not reachable yet; /readyz reports that state instead of the
// process refusing to start.
func NewPool(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// Deliberately not wrapped: pgx redacts the password only on a best
		// effort basis and the underlying url.Error echoes the full URL.
		return nil, errors.New("parse DATABASE_URL: invalid connection string")
	}
	cfg.MaxConns = maxConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	return pool, nil
}
