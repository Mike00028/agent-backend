# TASK-001 — Wire `memory.Service` in `cmd/main.go`

**Severity:** High  
**Component:** `golang/services/api/cmd/main.go`  
**Blocks:** TASK-002, TASK-003

---

## Problem

`NewChatHandler` is called with `nil` as the `memorySvc` argument:

```go
// main.go line ~134
chatHandler := handler.NewChatHandler(
    pool, cp, agentStore, nil,   // ← always nil
    hitlStore, mcpMgr, llmClient,
    cfg.PlannerModel, cfg.ChatModel, cfg.EvalModel,
)
```

Both the memory-read path (`Retrieve`) and the memory-write path (`maybeFlushMemory`) are guarded with `if h.memorySvc != nil`, so they silently no-op on every request. The entire semantic memory subsystem is dead code at runtime.

---

## What to Do

Replace the `nil` argument with a constructed `*memory.Service` **when Postgres is available and `EMBED_MODEL` is set**:

```go
// After pgPool is confirmed non-nil, before NewChatHandler:
var memorySvc *memory.Service
if pgPool != nil && cfg.EmbedModel != "" && cfg.OllamaBaseURL != "" {
    memorySvc = memory.New(pgPool, cfg.OllamaBaseURL, cfg.EmbedModel)
    slog.Info("memory service ready",
        "embed_model", cfg.EmbedModel,
        "ollama_url", cfg.OllamaBaseURL,
    )
}

chatHandler := handler.NewChatHandler(
    pool, cp, agentStore, memorySvc,   // ← wired
    hitlStore, mcpMgr, llmClient,
    cfg.PlannerModel, cfg.ChatModel, cfg.EvalModel,
)
```

Add the import:
```go
"github.com/mike00028/golang-backend/services/api/internal/memory"
```

---

## Definition of Done

- [ ] `memorySvc` is non-nil when `PostgresDSN`, `OllamaBaseURL`, and `EMBED_MODEL` are all set
- [ ] `memorySvc` is `nil` (and server starts cleanly) when any of the three are missing
- [ ] Memory is nil-guarded gracefully when Postgres is unavailable (local dev without DB)
- [ ] `go build ./...` passes with no errors
- [ ] Startup log shows `"memory service ready"` when configured

---

## Files to Edit

| File | Change |
|------|--------|
| `golang/services/api/cmd/main.go` | Construct `memory.New(...)`, pass to `NewChatHandler` |

---

## Notes

- `memory.New` signature: `New(db DB, ollamaURL, embedModel string) *Service`
- `pgPool` (the `*pgAdapter`) satisfies `memory.DB` — **not** `pgPool.pool` (raw `*pgxpool.Pool` does not satisfy the interface)
- `cfg.EmbedModel` maps to env var `EMBED_MODEL` (already in config.go)
- `cfg.OllamaBaseURL` maps to env var `OLLAMA_BASE_URL` (already in config.go)
- Guard `cfg.EmbedModel != ""` prevents constructing a `Service` that silently fails every embed call
