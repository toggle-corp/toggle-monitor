package migrate_test

import (
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
