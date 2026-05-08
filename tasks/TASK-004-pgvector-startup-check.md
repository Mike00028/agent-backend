# TASK-004 — Validate pgvector Extension at Startup

**Severity:** Low  
**Component:** `golang/services/api/internal/db/migrate.go`  
**Depends on:** TASK-001

---

## Problem

`memory.Service` requires the pgvector extension (`CREATE EXTENSION IF NOT EXISTS vector`) to be installed in Postgres. If it is missing, every `INSERT` and `SELECT` using `::vector` casting will fail with a Postgres error like:

```
ERROR: type "vector" does not exist (SQLSTATE 42704)
```

Because `Retrieve` and `WriteEntry` both swallow errors, this failure is completely silent — memory reads return `""` and memory writes silently drop data. There is no startup warning that pgvector is unavailable.

---

## What to Do

### Add a pgvector probe in the migration code

In `golang/services/api/internal/db/migrate.go` (or `main.go` after `Migrate` succeeds), add:

```go
// CheckPgvector verifies the vector extension is installed and the
// agent_memory_log.embedding column exists. Returns a descriptive error if not.
func CheckPgvector(ctx context.Context, db *pgxpool.Pool) error {
    var exists bool
    err := db.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM pg_extension WHERE extname = 'vector'
        )
    `).Scan(&exists)
    if err != nil {
        return fmt.Errorf("pgvector probe failed: %w", err)
    }
    if !exists {
        return fmt.Errorf("pgvector extension is not installed; run: CREATE EXTENSION IF NOT EXISTS vector")
    }
    return nil
}
```

### Call it in `main.go` after migrations succeed

```go
if err := internaldb.Migrate(cfg.PostgresDSN); err != nil {
    slog.Error("db migration failed", "error", err)
    os.Exit(1)
}
if err := internaldb.CheckPgvector(context.Background(), pgPool.pool); err != nil {
    slog.Warn("pgvector not available — memory service will be disabled", "error", err)
    // Don't exit — allow the server to run without memory
}
```

---

## Definition of Done

- [ ] `CheckPgvector` function exists and is tested
- [ ] Startup log shows a `WARN` if pgvector is missing (does **not** crash the server)
- [ ] When pgvector is missing, `memorySvc` is set to `nil` so memory accesses degrade gracefully
- [ ] `go build ./...` passes

---

## Files to Edit

| File | Change |
|------|--------|
| `golang/services/api/internal/db/migrate.go` | Add `CheckPgvector(ctx, pool)` |
| `golang/services/api/cmd/main.go` | Call `CheckPgvector` after migrations; nil-out `memorySvc` on failure |
