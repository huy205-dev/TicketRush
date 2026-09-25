//go:build integration

// Package testredis gives integration tests a real Redis. One container is
// started per test binary; each test starts from an empty database.
package testredis

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
	"github.com/huy205-dev/ticketrush/internal/platform/testdb"
)

// Image matches deploy/compose.yaml.
const Image = "redis:7.4.11-alpine"

// Redis is a running Redis container.
type Redis struct {
	container *tcredis.RedisContainer
	addr      string
}

// Start launches the container.
func Start(ctx context.Context) (*Redis, error) {
	c, err := tcredis.Run(ctx, Image)
	if err != nil {
		return nil, fmt.Errorf("start redis: %w", err)
	}
	endpoint, err := c.Endpoint(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("redis endpoint: %w", err)
	}
	return &Redis{container: c, addr: endpoint}, nil
}

// Terminate stops the container.
func (r *Redis) Terminate() {
	if err := testcontainers.TerminateContainer(r.container); err != nil {
		fmt.Fprintf(os.Stderr, "testredis: terminate: %v\n", err)
	}
}

// New empties the database and returns a client configured like the
// services' (redisx.NewClient). Tests in one binary run one at a time unless
// they call t.Parallel, which tests using New must not do.
func (r *Redis) New(t testing.TB) *redis.Client {
	t.Helper()
	rdb := redisx.NewClient(r.addr)
	if err := rdb.FlushAll(context.Background()).Err(); err != nil {
		t.Fatalf("flush redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// Main starts Redis, runs the tests and stops it.
func Main(m *testing.M, dst **Redis) {
	r, err := Start(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "testredis: %v\n", err)
		os.Exit(1)
	}
	*dst = r
	code := m.Run()
	r.Terminate()
	os.Exit(code)
}

// MainWithDB starts PostgreSQL and Redis, runs the tests and stops both.
//
//	var (db *testdb.DB; rds *testredis.Redis)
//	func TestMain(m *testing.M) { testredis.MainWithDB(m, &db, &rds) }
func MainWithDB(m *testing.M, db **testdb.DB, rds **Redis) {
	ctx := context.Background()
	pg, err := testdb.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
		os.Exit(1)
	}
	r, err := Start(ctx)
	if err != nil {
		pg.Terminate()
		fmt.Fprintf(os.Stderr, "testredis: %v\n", err)
		os.Exit(1)
	}
	*db, *rds = pg, r
	code := m.Run()
	r.Terminate()
	pg.Terminate()
	os.Exit(code)
}
