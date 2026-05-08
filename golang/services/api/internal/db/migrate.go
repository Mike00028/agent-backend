package db

import (
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// pgx5DSN converts a postgres:// DSN to the pgx5:// scheme golang-migrate requires.
// The DSN is expected to already contain sslmode=disable and a numeric host
// (set in .env) — no further manipulation needed.
func pgx5DSN(raw string) string {
	s := strings.TrimPrefix(raw, "postgresql://")
	s = strings.TrimPrefix(s, "postgres://")
	return "pgx5://" + s
}

// newMigrate builds a golang-migrate instance backed by the embedded sql/ dir.
func newMigrate(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(MigrationsFS, "sql")
	if err != nil {
		return nil, fmt.Errorf("db: iofs source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, pgx5DSN(dsn))
	if err != nil {
		return nil, fmt.Errorf("db: migrate init: %w", err)
	}
	return m, nil
}

// Migrate applies all pending up-migrations.
// Safe to call on every startup — already-applied versions are skipped.
func Migrate(dsn string) error {
	m, err := newMigrate(dsn)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db.Migrate: %w", err)
	}
	return nil
}

// Rollback rolls back the last applied migration.
func Rollback(dsn string) error {
	m, err := newMigrate(dsn)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Steps(-1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db.Rollback: %w", err)
	}
	return nil
}

// RollbackAll rolls back every applied migration (down to zero).
func RollbackAll(dsn string) error {
	m, err := newMigrate(dsn)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db.RollbackAll: %w", err)
	}
	return nil
}
