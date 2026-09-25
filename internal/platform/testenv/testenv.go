//go:build integration

// Package testenv starts the containers a test binary needs, in parallel,
// and stops them after the tests.
package testenv

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/huy205-dev/ticketrush/internal/platform/testdb"
	"github.com/huy205-dev/ticketrush/internal/platform/testkafka"
	"github.com/huy205-dev/ticketrush/internal/platform/testredis"
)

// Env holds the running containers. Fields not requested stay nil.
type Env struct {
	DB    *testdb.DB
	Redis *testredis.Redis
	Kafka *testkafka.Kafka
}

// Needs selects the containers to start.
type Needs struct {
	Postgres, Redis, Kafka bool
}

// Main starts what needs asks for, stores it in env, runs the tests and
// stops everything:
//
//	var env testenv.Env
//	func TestMain(m *testing.M) { testenv.Main(m, testenv.Needs{Postgres: true, Redis: true}, &env) }
func Main(m *testing.M, needs Needs, env *Env) {
	ctx := context.Background()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	fail := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	if needs.Postgres {
		wg.Go(func() {
			db, err := testdb.Start(ctx)
			if err != nil {
				fail(err)
				return
			}
			env.DB = db
		})
	}
	if needs.Redis {
		wg.Go(func() {
			r, err := testredis.Start(ctx)
			if err != nil {
				fail(err)
				return
			}
			env.Redis = r
		})
	}
	if needs.Kafka {
		wg.Go(func() {
			k, err := testkafka.Start(ctx)
			if err != nil {
				fail(err)
				return
			}
			env.Kafka = k
		})
	}
	wg.Wait()

	code := 1
	if len(errs) == 0 {
		code = m.Run()
	} else {
		for _, err := range errs {
			fmt.Fprintf(os.Stderr, "testenv: %v\n", err)
		}
	}
	if env.Kafka != nil {
		env.Kafka.Terminate()
	}
	if env.Redis != nil {
		env.Redis.Terminate()
	}
	if env.DB != nil {
		env.DB.Terminate()
	}
	os.Exit(code)
}
