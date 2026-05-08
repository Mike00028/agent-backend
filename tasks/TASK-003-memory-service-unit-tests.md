# TASK-003 — Unit Tests for `memory.Service`

**Severity:** High  
**Component:** `golang/services/api/internal/memory/`  
**Depends on:** TASK-001

---

## Problem

`memory/service.go` has zero test coverage. Both `Retrieve` and `WriteEntry` silently swallow embedding and DB errors, making regressions invisible. There is no way to know if the pgvector query or the Ollama embedding call is broken without a running end-to-end environment.

---

## What to Do

Create `golang/services/api/internal/memory/service_test.go` with the following test cases:

### Test cases required

| Test | What it covers |
|------|----------------|
| `TestRetrieve_HappyPath` | DB returns rows → `Retrieve` returns joined content |
| `TestRetrieve_EmbedError` | Ollama returns non-200 → `Retrieve` returns `""` (soft failure) |
| `TestRetrieve_DBError` | DB `Query` fails → `Retrieve` returns `""` (soft failure) |
| `TestRetrieve_Empty` | DB returns 0 rows → `Retrieve` returns `""` |
| `TestWriteEntry_HappyPath` | Embed + DB insert succeed → no error |
| `TestWriteEntry_EmbedFail` | Ollama returns non-200 → error returned |
| `TestWriteEntry_DBFail` | DB `Exec` fails → error returned |
| `TestVectorLiteral` | Verifies `[0.1,0.2,0.3]` formatting for pgvector |

### Approach

Use `httptest.NewServer` for the fake Ollama endpoint and a hand-rolled `mockDB` that satisfies `memory.DB`:

```go
type mockRows struct {
    rows []string
    idx  int
}
func (m *mockRows) Next() bool        { m.idx++; return m.idx <= len(m.rows) }
func (m *mockRows) Scan(dst ...any) error { *dst[0].(*string) = m.rows[m.idx-1]; return nil }
func (m *mockRows) Close()            {}
func (m *mockRows) Err() error        { return nil }

type mockDB struct {
    queryRows *mockRows
    queryErr  error
    execErr   error
}
func (m *mockDB) Query(_ context.Context, _ string, _ ...any) (memory.Rows, error) {
    return m.queryRows, m.queryErr
}
func (m *mockDB) Exec(_ context.Context, _ string, _ ...any) (interface{ RowsAffected() int64 }, error) {
    return mockResult{}, m.execErr
}
```

---

## Definition of Done

- [ ] All 8 test cases listed above are implemented and pass
- [ ] `go test ./internal/memory/...` exits 0
- [ ] Coverage on `service.go` ≥ 80% (`go test -cover`)
- [ ] No real HTTP or DB connections used in tests (all mocked)

---

## Files to Create

| File | Content |
|------|---------|
| `golang/services/api/internal/memory/service_test.go` | All 8 unit tests |
