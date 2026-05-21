//go:build integration

// Package testpg spins up a real Postgres container via testcontainers
// for integration tests. Compiled only under the `integration` build
// tag so the production binary does not link the testcontainers tree.
package testpg

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Start launches a Postgres 17 container and returns the DSN. The
// container is terminated when the test ends. Tests should `t.Skip` on
// errors that indicate Docker isn't available — they should not fail
// CI environments that aren't configured for integration tests.
func Start(t *testing.T) string {
	t.Helper()

	ctx := context.Background()
	container, err := postgres.Run(ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("tm_test"),
		postgres.WithUsername("tm"),
		postgres.WithPassword("tm-secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(45*time.Second),
		),
	)
	if err != nil {
		t.Skipf("could not start postgres testcontainer (Docker available?): %v", err)
	}

	t.Cleanup(func() {
		// Use a separate context so a cancelled test doesn't leak
		// containers (testcontainers' ryuk normally cleans up, but
		// explicit termination is friendlier).
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = container.Terminate(stopCtx)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get connection string: %v", err)
	}
	return dsn
}
