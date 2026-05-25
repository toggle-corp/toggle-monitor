package migrate_test

import (
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/migrate"
)

func TestLatestVersion_returnsHighestEmbeddedVersion(t *testing.T) {
	v, err := migrate.LatestVersion()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v == 0 {
		t.Fatal("LatestVersion returned 0; expected at least 1")
	}
}

// TestPendingNames_listsMigrationsAboveCurrent indirectly covers the
// pre-Up summary that operators see when running `toggle-monitor
// migrate` — the list of pending migration filenames is what the CLI
// prints before applying.
func TestPendingNames_listsMigrationsAboveCurrent(t *testing.T) {
	latest, err := migrate.LatestVersion()
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if latest < 2 {
		t.Skipf("need at least 2 embedded migrations to exercise pendingNames, have %d", latest)
	}

	// From a fresh DB (current=0), every migration should be pending
	// and the list should be ordered with the lowest version first.
	names, err := migrate.PendingNames(0, latest)
	if err != nil {
		t.Fatalf("PendingNames: %v", err)
	}
	if uint(len(names)) != latest {
		t.Fatalf("from current=0 expected %d pending, got %d (%v)", latest, len(names), names)
	}
	if !strings.HasPrefix(names[0], "0001_") {
		t.Errorf("expected first pending to start with 0001_, got %q", names[0])
	}

	// At the latest version there is nothing pending.
	none, err := migrate.PendingNames(latest, latest)
	if err != nil {
		t.Fatalf("PendingNames at latest: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("expected zero pending at latest, got %v", none)
	}

	// From version (latest-1), exactly one migration is pending.
	one, err := migrate.PendingNames(latest-1, latest)
	if err != nil {
		t.Fatalf("PendingNames at latest-1: %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("expected exactly 1 pending at latest-1, got %v", one)
	}
}
