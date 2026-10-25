package store

// Test bootstrap for the Postgres-gated tests.
//
// The PG contract/concurrency/dual tests open ENCODE_TEST_PG_DSN directly and
// assume the target database exists. Without this bootstrap a fresh PG server
// (no encode_test yet) fails those tests with "database does not exist" unless
// TestMigratePGFresh happens to run first — a test-order dependency. TestMain
// removes it: when the env var is set, the database is created (if missing)
// before ANY test runs. It is created, not dropped — TestMigratePGFresh still
// owns its own drop+recreate cycle to prove migration-from-nothing.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if dsn := os.Getenv("ENCODE_TEST_PG_DSN"); dsn != "" {
		if err := ensurePGTestDB(dsn); err != nil {
			fmt.Fprintf(os.Stderr, "store tests: ensure PG test database: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// parseTestDBName extracts the database name and a maintenance DSN (same
// user/host/port, "postgres" database) from a postgres:// DSN. Shared by
// ensurePGTestDB and dropAndRecreateTestDB so the parsing exists once.
func parseTestDBName(dsn string) (dbName, maintDSN string, err error) {
	rest := dsn
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	var userinfo, hostport, pathPart string
	if atIdx := strings.Index(rest, "@"); atIdx >= 0 {
		userinfo = rest[:atIdx]
		hostport = rest[atIdx+1:]
	} else {
		hostport = rest
	}
	if slashIdx := strings.Index(hostport, "/"); slashIdx >= 0 {
		pathPart = hostport[slashIdx+1:]
		hostport = hostport[:slashIdx]
	}
	dbName = pathPart
	if q := strings.Index(dbName, "?"); q >= 0 {
		dbName = dbName[:q]
	}
	if dbName == "" {
		return "", "", fmt.Errorf("could not parse database name from DSN")
	}
	// dbName/userinfo come from our own test DSN, not user input; the
	// maintenance DSN is only ever used against a local test server.
	maintDSN = fmt.Sprintf("postgres://%s@%s/postgres?sslmode=disable", userinfo, hostport)
	return dbName, maintDSN, nil
}

// ensurePGTestDB creates the target database if it does not exist yet.
// Unlike dropAndRecreateTestDB it never drops: tests that TRUNCATE in cleanup
// keep their data-isolation contract, and a mid-suite create can't destroy
// another test's fixture.
func ensurePGTestDB(dsn string) error {
	if !isPostgresDSN(dsn) {
		return fmt.Errorf("ENCODE_TEST_PG_DSN must be a postgres:// DSN, got %q", dsn)
	}
	dbName, maintDSN, err := parseTestDBName(dsn)
	if err != nil {
		return err
	}
	maintDB, err := sql.Open("pgx", maintDSN)
	if err != nil {
		return fmt.Errorf("open maintenance: %w", err)
	}
	defer maintDB.Close()
	ctx := context.Background()
	var exists int
	err = maintDB.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_database WHERE datname = $1`, dbName).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check database: %w", err)
	}
	if exists > 0 {
		return nil
	}
	// Quoted identifier built from our own parsed DSN (not user input).
	if _, err := maintDB.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, dbName)); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	return nil
}
