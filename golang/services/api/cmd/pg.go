package main

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mike00028/golang-backend/services/api/internal/dag"
	internaldb "github.com/mike00028/golang-backend/services/api/internal/db"
)

// pgAdapter wraps *pgxpool.Pool and satisfies dag.PgxDB.
// Will also satisfy future conversation/tool-registry DB interfaces.
type pgAdapter struct {
	pool *pgxpool.Pool
}

func openPgPool(ctx context.Context, dsn string) (*pgAdapter, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// Explicitly disable TLS and force IPv4 so local Docker Postgres works
	// regardless of how the DSN was written (localhost vs 127.0.0.1, sslmode or not).
	cfg.ConnConfig.TLSConfig = nil
	if cfg.ConnConfig.Host == "localhost" {
		cfg.ConnConfig.Host = "127.0.0.1"
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Ping verifies credentials immediately — pgxpool.New only parses config.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &pgAdapter{pool: pool}, nil
}

func (p *pgAdapter) Close() { p.pool.Close() }

// Exec satisfies dag.PgxDB and memory.DB.
func (p *pgAdapter) Exec(ctx context.Context, sql string, args ...any) (interface{ RowsAffected() int64 }, error) {
	tag, err := p.pool.Exec(ctx, sql, args...)
	return tag, err
}

// Query satisfies dag.PgxDB and memory.DB (memory.Rows = dag.PgxRows).
func (p *pgAdapter) Query(ctx context.Context, sql string, args ...any) (dag.PgxRows, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRowsAdapter{rows: rows}, nil
}

// QueryRow satisfies future DB interfaces (conversations, MCP tool registry).
func (p *pgAdapter) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.pool.QueryRow(ctx, sql, args...)
}

// -- Row adapters -------------------------------------------------------------

type pgxRowsAdapter struct{ rows pgx.Rows }

func (r *pgxRowsAdapter) Next() bool             { return r.rows.Next() }
func (r *pgxRowsAdapter) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *pgxRowsAdapter) Close()                 { r.rows.Close() }
func (r *pgxRowsAdapter) Err() error             { return r.rows.Err() }

type pgRowAdapter struct{ row pgx.Row }

func (r pgRowAdapter) Scan(dest ...any) error { return r.row.Scan(dest...) }

// -- Compile-time interface checks --------------------------------------------

var _ dag.PgxDB = (*pgAdapter)(nil)

// ── chatDB ────────────────────────────────────────────────────────────────────
// chatDB wraps *pgxpool.Pool and satisfies internaldb.QueryRower.
// It is separate from pgAdapter because Query must return (internaldb.Rows, error)
// rather than (dag.PgxRows, error) — Go interfaces are invariant in return types.
type chatDB struct{ pool *pgxpool.Pool }

// QueryRow returns a pgx.Row, which satisfies interface{ Scan(...any) error }.
func (c *chatDB) QueryRow(ctx context.Context, sql string, args ...any) interface{ Scan(dest ...any) error } {
	return c.pool.QueryRow(ctx, sql, args...)
}

// Query returns (internaldb.Rows, error) — satisfies internaldb.QueryRower.
func (c *chatDB) Query(ctx context.Context, sql string, args ...any) (internaldb.Rows, error) {
	rows, err := c.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRowsAdapter{rows: rows}, nil
}

// Exec satisfies internaldb.QueryRower.
func (c *chatDB) Exec(ctx context.Context, sql string, args ...any) (interface{ RowsAffected() int64 }, error) {
	return c.pool.Exec(ctx, sql, args...)
}

var _ internaldb.QueryRower = (*chatDB)(nil)
