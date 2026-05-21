//go:build integration

package migrate_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

func TestUp_appliesAllMigrations(t *testing.T) {
	dsn := testpg.Start(t)

	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("first Up(): %v", err)
	}
	// Idempotent: a second Up() is a no-op.
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("second Up() should be a no-op: %v", err)
	}

	// Verify the expected tables exist after migration.
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, table := range []string{"monitors", "alert_events"} {
		var exists bool
		row := pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name=$1)`,
			table)
		if err := row.Scan(&exists); err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s expected to exist after migrate.Up", table)
		}
	}
}

func TestCheck_returnsNilWhenSchemaIsCurrent(t *testing.T) {
	dsn := testpg.Start(t)

	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := migrate.Check(dsn); err != nil {
		t.Fatalf("Check after Up: %v", err)
	}
}

func TestCheck_returnsErrorWhenPending(t *testing.T) {
	dsn := testpg.Start(t)

	err := migrate.Check(dsn)
	if err == nil {
		t.Fatal("expected Check on un-migrated DB to error, got nil")
	}
}

