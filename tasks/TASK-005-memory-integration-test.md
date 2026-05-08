# TASK-005 — Integration Test: Memory Read/Write through `/chat`

**Severity:** Medium  
**Component:** `golang/services/api/handler/`  
**Depends on:** TASK-001, TASK-003

---

## Problem

Even after wiring `memorySvc` in TASK-001, there is no automated test that verifies:
1. A completed DAG run triggers `maybeFlushMemory` and writes a memory entry
2. A subsequent request to the same `user_id` has memory context injected into `buildRunRequest`
3. The planner receives the memory context in its prompt

Without this test, the end-to-end memory read→write cycle can silently break on any refactor.

---

## What to Do

Create `golang/services/api/handler/memory_integration_test.go`:

### Scenario: memory written after eval_ok=true, read on next request

```go
func TestMemoryRoundTrip(t *testing.T) {
    // 1. Create a fake Ollama server that returns a fixed embedding
    //    and a fake pgpool (or use testcontainers-go with Postgres+pgvector)
    // 2. Create ChatHandler with real memory.Service backed by fake DB
    // 3. POST /chat with userID="u1", message="foo"
    //    - mock DAG result: eval_ok=true, score=0.9
    //    - assert: WriteEntry called with sessionID, userID="u1"
    // 4. POST /chat again with userID="u1", message="bar"
    //    - assert: buildRunRequest calls Retrieve(ctx, "u1", "bar", topK)
    //    - assert: dag.RunRequest.MemoryContext != ""
}
```

### Minimal test with mock DB (no Docker)

Use the same `mockDB` from TASK-003. Verify `maybeFlushMemory` calls `WriteEntry` by checking `mockDB.execCalled`.

> **Important:** `maybeFlushMemory` fires the DB write in a **background goroutine**. Tests must synchronise on it — use a channel in `mockDB.Exec` that signals when the write happens, then `select` with a timeout:

```go
func TestMaybeFlushMemory_WritesOnSuccess(t *testing.T) {
    wrote := make(chan struct{}, 1)
    db := &mockDB{onExec: func() { wrote <- struct{}{} }}
    svc := memory.New(db, fakeOllamaURL, "nomic-embed-text")
    h := &ChatHandler{memorySvc: svc}

    result := &dag.RunResult{EvalOK: true, ConfidenceScore: 0.9, FinalOutput: "answer"}
    spec := &agentstore.AgentSpec{
        MemoryPolicy: agentstore.MemoryPolicy{WriteOnEvalOK: true, MinScoreToWrite: 0.7},
    }
    h.maybeFlushMemory("u1", "sess1", result, spec)

    select {
    case <-wrote:
        // pass
    case <-time.After(2 * time.Second):
        t.Fatal("expected WriteEntry to INSERT into agent_memory_log within 2s")
    }
}

func TestMaybeFlushMemory_SkipsOnLowScore(t *testing.T) {
    db := &mockDB{}
    svc := memory.New(db, fakeOllamaURL, "nomic-embed-text")
    h := &ChatHandler{memorySvc: svc}

    result := &dag.RunResult{EvalOK: true, ConfidenceScore: 0.3}
    spec := &agentstore.AgentSpec{
        MemoryPolicy: agentstore.MemoryPolicy{WriteOnEvalOK: true, MinScoreToWrite: 0.7},
    }
    h.maybeFlushMemory("u1", "sess1", result, spec)

    // Give goroutine time to fire if it incorrectly does so.
    time.Sleep(100 * time.Millisecond)
    if db.execCalled {
        t.Fatal("expected WriteEntry to be skipped for low score")
    }
}
```

---

## Definition of Done

- [ ] `TestMaybeFlushMemory_WritesOnSuccess` passes
- [ ] `TestMaybeFlushMemory_SkipsOnLowScore` passes
- [ ] `TestMemoryRoundTrip` passes (mock DB acceptable; Docker optional)
- [ ] `go test ./handler/...` exits 0
- [ ] Memory not written when `memorySvc == nil` (guard tested)

---

## Files to Create

| File | Content |
|------|---------|
| `golang/services/api/handler/memory_integration_test.go` | 3 test functions above |

---

## Notes

- `maybeFlushMemory` is currently unexported — it can be tested by calling it directly since the test is in the same package (`package handler`)
- Write threshold field is `agentstore.MemoryPolicy.MinScoreToWrite` (not `MinScoreWrite`)
