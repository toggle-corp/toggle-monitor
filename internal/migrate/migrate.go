// Package migrate drives schema migrations via golang-migrate over an
// embedded migration set.
package migrate

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Migrations holds the embedded SQL migration set used by the migrate
// subcommand. Files are loaded as a virtual filesystem (golang-migrate's
// iofs source).
//
//go:embed migrations/*.sql
var Migrations embed.FS

// LatestVersion returns the highest migration version embedded in the
// binary, derived from the migration filenames (NNNN_name.up.sql).
// Used by the startup schema-version check.
func LatestVersion() (uint, error) {
	entries, err := fs.ReadDir(Migrations, "migrations")
	if err != nil {
		return 0, fmt.Errorf("read embedded migrations: %w", err)
	}
	var highest uint
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		under := strings.IndexByte(name, '_')
		if under <= 0 {
			continue
		}
		v, err := strconv.ParseUint(name[:under], 10, 32)
		if err != nil {
			continue
		}
		if uint(v) > highest {
			highest = uint(v)
		}
	}
	if highest == 0 {
		return 0, errors.New("no migration files found")
	}
	return highest, nil
}

// Up applies all pending migrations against the given Postgres DSN
// and writes a per-migration progress report to os.Stdout. Idempotent:
// returns nil with a "no pending migrations" line if the schema is
// already at the latest version.
func Up(dsn string) error {
	return UpTo(dsn, os.Stdout)
}

// UpTo is the Up variant that writes its progress report to the given
// writer. Useful for tests; production code should call Up.
func UpTo(dsn string, w io.Writer) error {
	m, err := open(dsn)
	if err != nil {
		return err
	}
	defer closeM(m)

	latest, err := LatestVersion()
	if err != nil {
		return err
	}
	current, _, vErr := m.Version()
	if vErr != nil && !errors.Is(vErr, migrate.ErrNilVersion) {
		return fmt.Errorf("read schema version: %w", vErr)
	}
	currentDisplay := strconv.FormatUint(uint64(current), 10)
	if errors.Is(vErr, migrate.ErrNilVersion) {
		currentDisplay = "none"
	}

	pending, err := PendingNames(uint(current), latest)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		_, _ = fmt.Fprintf(w, "schema is at version %s (latest); no migrations to apply\n", currentDisplay)
		return nil
	}

	_, _ = fmt.Fprintf(w, "schema at version %s; applying %d pending migration(s) to reach version %d:\n",
		currentDisplay, len(pending), latest)
	for _, name := range pending {
		_, _ = fmt.Fprintf(w, "  - %s\n", name)
	}

	m.Log = &writerLogger{w: w}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	_, _ = fmt.Fprintf(w, "schema is now at version %d (%d migration(s) applied)\n", latest, len(pending))
	return nil
}

// Check reports whether the DB schema matches the embedded migrations
// without applying anything. Returns nil when the DB is at the latest
// version, otherwise an error describing the drift.
func Check(dsn string) error {
	m, err := open(dsn)
	if err != nil {
		return err
	}
	defer closeM(m)
	current, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("read schema version: %w", err)
	}
	if dirty {
		return fmt.Errorf("schema is in a dirty state at version %d; manual intervention required", current)
	}
	latest, err := LatestVersion()
	if err != nil {
		return err
	}
	if uint(current) < latest {
		return fmt.Errorf("schema at version %d; binary expects %d (run `toggle-monitor migrate`)", current, latest)
	}
	if uint(current) > latest {
		return fmt.Errorf("schema at version %d but binary only knows about version %d (binary is older than the DB)", current, latest)
	}
	return nil
}

// PendingNames returns the migration filenames (without `.up.sql`
// extension) that would be applied to move the schema from `current`
// to `latest`. Order matches the on-disk filename ordering.
func PendingNames(current, latest uint) ([]string, error) {
	if current >= latest {
		return nil, nil
	}
	entries, err := fs.ReadDir(Migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		under := strings.IndexByte(name, '_')
		if under <= 0 {
			continue
		}
		v, err := strconv.ParseUint(name[:under], 10, 32)
		if err != nil {
			continue
		}
		if uint(v) <= current {
			continue
		}
		names = append(names, strings.TrimSuffix(name, ".up.sql"))
	}
	return names, nil
}

func open(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(Migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("open migration source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, "pgx5://"+strings.TrimPrefix(dsn, "postgres://"))
	if err != nil {
		return nil, fmt.Errorf("open migrate: %w", err)
	}
	return m, nil
}

// closeM closes the migrate handle and discards the close errors —
// golang-migrate returns one error per source and one per database
// driver on close, which is noisy and rarely actionable.
func closeM(m *migrate.Migrate) {
	_, _ = m.Close()
}

// writerLogger adapts an io.Writer to golang-migrate's Logger
// interface so per-step library output ("Read and execute 8/u ...",
// "Finished after ...") lands in the same stream as our summary.
type writerLogger struct {
	w io.Writer
}

func (l *writerLogger) Printf(format string, v ...interface{}) {
	_, _ = fmt.Fprintf(l.w, "  "+format, v...)
}

func (l *writerLogger) Verbose() bool { return true }
