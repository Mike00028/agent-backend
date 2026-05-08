# TASK-002 — Validate `EMBED_MODEL` and `OLLAMA_BASE_URL` in Config

**Severity:** Medium  
**Component:** `golang/services/api/config/config.go`  
**Depends on:** TASK-001

---

## Problem

`EMBED_MODEL` is used by:
1. `memory.Service` — embedding queries and writes
2. `mcptools.NewOllamaEmbedder` — MCP tool discovery

If the env var is missing or empty the service silently uses `""` as the model name, which causes Ollama to return a 400 error on every embedding call. Because `Retrieve` and `WriteEntry` swallow errors and return `""`, the failure is invisible in logs.

---

## What to Do

### 1. Add a `Validate()` method to `Config`

In `config.go`, after `Load()`:

```go
// Validate returns an error if any required combination of settings is invalid.
func (c Config) Validate() error {
    if c.PostgresDSN != "" {
        if c.OllamaBaseURL == "" {
            return fmt.Errorf("OLLAMA_BASE_URL is required when POSTGRES_DSN is set (needed for embeddings)")
        }
        if c.EmbedModel == "" {
            return fmt.Errorf("EMBED_MODEL is required when POSTGRES_DSN is set (needed for embeddings)")
        }
    }
    return nil
}
```

### 2. Call `Validate()` in `main.go`

```go
cfg, err := config.Load()
if err != nil {
    slog.Error("config error", "error", err)
    os.Exit(1)
}
if err := cfg.Validate(); err != nil {
    slog.Error("config validation failed", "error", err)
    os.Exit(1)
}
```

### 3. Add `EMBED_MODEL` to `.env.example`

```
EMBED_MODEL=nomic-embed-text
```

---

## Definition of Done

- [ ] Server fails fast at startup with a clear message if `EMBED_MODEL` is empty and Postgres is configured
- [ ] Server starts normally without `POSTGRES_DSN` (local dev mode)
- [ ] `.env.example` documents `EMBED_MODEL`
- [ ] `go test ./config/...` passes

---

## Files to Edit

| File | Change |
|------|--------|
| `golang/services/api/config/config.go` | Add `Validate()` method |
| `golang/services/api/cmd/main.go` | Call `cfg.Validate()` after `config.Load()` |
| `.env.example` | Document `EMBED_MODEL=nomic-embed-text` |
