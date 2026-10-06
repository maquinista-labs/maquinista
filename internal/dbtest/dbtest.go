// Package dbtest provides test-only helpers for integration tests that need a
// real Postgres. PgContainer spins up a disposable Postgres 16 container via
// testcontainers-go; prefer it over any ad-hoc DB setup so migration state is
// always applied fresh.
package dbtest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PgContainer runs a postgres:16-alpine container and returns a connected
// pool plus the connection string. The container is terminated via
// t.Cleanup. Tests are skipped (not failed) when Docker is unavailable so
// this package stays usable in CI environments without Docker.
func PgContainer(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()

	if os.Getenv("MAQUINISTA_SKIP_DOCKER_TESTS") != "" {
		t.Skip("MAQUINISTA_SKIP_DOCKER_TESTS set")
	}

	ctx := context.Background()
	// Startup timeout 180s, not testcontainers' typical 60s: this box runs
	// container-PER-TEST — when several suites churn containers at once
	// (a merge gate + a fixer pane mid-verification), dockerd degrades
	// measurably (port-bind retries, broken first execs — see dockerd logs
	// 2026-10-06 07:15-07:17). A container declared ready into a congested
	// daemon hands the test a sick Postgres and the suite goes red for
	// infra reasons; the higher bound just refuses to do that. Worst case
	// (daemon truly stuck) is the same skip as before, three minutes later.
	container, err := tcpg.Run(ctx,
		"postgres:16-alpine",
		tcpg.WithDatabase("maquinista_test"),
		tcpg.WithUsername("test"),
		tcpg.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(180*time.Second),
		),
	)
	if err != nil {
		t.Skipf("postgres container unavailable: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool, dsn
}
