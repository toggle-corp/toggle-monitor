// Package migrate drives schema migrations via golang-migrate over an
// embedded migration set.
package migrate

import (
	"embed"

	// golang-migrate core + iofs source + pgx5 database driver are
	// locked here; the migration runner lands in Issue 2.
	_ "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/iofs"
)

// Migrations holds the embedded SQL migration set used by the migrate
// subcommand. Files are loaded as a virtual filesystem (golang-migrate's
// iofs source).
//
//go:embed migrations/*
var Migrations embed.FS
