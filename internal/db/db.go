// Package db owns the Postgres connection lifecycle: startup backoff,
// runtime retry, schema-version checks.
package db

// pgx is the Postgres driver. Imported here so the dependency is locked
// in go.mod before the connection layer lands in Issue 2.
import _ "github.com/jackc/pgx/v5"
