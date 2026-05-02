package main

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mike00028/golang-backend/services/api/internal/dag"
)

// pgAdapter wraps *pgxpool.Pool and satisfies dag.PgxDB.
// Will also satisfy future conversation/tool-registry DB interfaces.
type pgAdapter struct {
	pool *pgxpool.Pool
}

func openPgPool(ctx context.Context, dsn string) (*pgAdapter, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
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
