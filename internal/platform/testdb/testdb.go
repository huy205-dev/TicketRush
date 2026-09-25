//go:build integration

// Package testdb gives integration tests a real PostgreSQL. One container is
// started per test binary; every test gets its own database cloned from a
// migrated template, so tests are isolated and can run in parallel.
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/huy205-dev/ticketrush/migrations"
)

// Image matches deploy/compose.yaml.
const Image = "postgres:16.15-alpine"

const templateDB = "ticketrush_template"

// DB is a running PostgreSQL container with a migrated template database.
type DB struct {
	container *tcpostgres.PostgresContainer
	baseURL   *url.URL
	admin     *pgxpool.Pool
	next      atomic.Int64
}

// Main starts the container, runs the tests and tears it down. Use it from
// TestMain:
//
//	var db *testdb.DB
//	func TestMain(m *testing.M) { testdb.Main(m, &db) }
func Main(m *testing.M, dst **DB) {
	db, err := Start(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
		os.Exit(1)
	}
	*dst = db
	code := m.Run()
	db.Terminate()
	os.Exit(code)
}

// Start launches the container and migrates the template database. Use Main
// unless the test binary needs other containers too.
func Start(ctx context.Context) (*DB, error) {
	c, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase(templateDB),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
		// Durability is irrelevant for throwaway test data.
		testcontainers.WithCmdArgs("-c", "fsync=off", "-c", "synchronous_commit=off",
			"-c", "full_page_writes=off", "-c", "max_connections=500"),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}
	raw, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("connection string: %w", err)
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}

	if err := migrate(ctx, raw); err != nil {
		return nil, err
	}

	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		return nil, fmt.Errorf("admin pool: %w", err)
	}
	return &DB{container: c, baseURL: base, admin: admin}, nil
}

// Terminate stops the container.
func (d *DB) Terminate() {
	d.admin.Close()
	if err := testcontainers.TerminateContainer(d.container); err != nil {
		fmt.Fprintf(os.Stderr, "testdb: terminate: %v\n", err)
	}
}

func migrate(ctx context.Context, dsn string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open for migration: %w", err)
	}
	defer sqlDB.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// New creates a fresh migrated database for the test and returns a pool with
// at most maxConns connections. Both are removed when the test ends.
func (d *DB) New(t testing.TB, maxConns int32) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	name := pgx.Identifier{fmt.Sprintf("test_%d", d.next.Add(1))}.Sanitize()

	// Identifiers cannot be bound as parameters; name is generated and quoted.
	if _, err := d.admin.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE "+templateDB); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	u := *d.baseURL
	u.Path = "/" + name[1:len(name)-1] // strip the quotes added by Sanitize
	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		t.Fatalf("parse test database url: %v", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := d.admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop test database: %v", err)
		}
	})
	return pool
}
