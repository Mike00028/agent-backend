package db

import "embed"

// MigrationsFS holds all SQL migration files embedded at compile time.
// golang-migrate reads them via the iofs source driver.
//
//go:embed sql
var MigrationsFS embed.FS
