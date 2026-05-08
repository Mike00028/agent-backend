package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mike00028/golang-backend/services/api/internal/agentstore"
	"github.com/mike00028/golang-backend/services/api/internal/dag"
	"github.com/mike00028/golang-backend/services/api/internal/memory"
)

// ── mock DB (satisfies memory.DB) ────────────────────────────────────────────

type memMockResult struct{}

func (memMockResult) RowsAffected() int64 { return 1 }

type memMockRows struct{}

func (m *memMockRows) Next() bool             { return false }
func (m *memMockRows) Close()                 {}
func (m *memMockRows) Err() error             { return nil }
func (m *memMockRows) Scan(_ ...any) error    { return nil }

type memMockDB struct {
	execErr    error
	onExec     func() // called when Exec is invoked
	execCalled bool
}

func (m *memMockDB) Query(_ context.Context, _ string, _ ...any) (memory.Rows, error) {
	return &memMockRows{}, nil
}

func (m *memMockDB) Exec(_ context.Context, _ string, _ ...any) (interface{ RowsAffected() int64 }, error) {
	m.execCalled = true
	if m.onExec != nil {
		m.onExec()
	}
	return memMockResult{}, m.execErr
}

// ── fake Ollama ───────────────────────────────────────────────────────────────

type embedResp struct {
	Embedding []float32 `json:"embedding"`
}

func fakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(embedResp{Embedding: []float32{0.1, 0.2, 0.3}}) //nolint:errcheck
	}))
}

// ── helper to build a minimal ChatHandler with memory ────────────────────────

func newHandlerWithMemory(db *memMockDB, ollamaURL string) *ChatHandler {
	svc := memory.New(db, ollamaURL, "nomic-embed-text")
	return &ChatHandler{memorySvc: svc}
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestMaybeFlushMemory_NilSvc verifies no panic and no write when memorySvc is nil.
func TestMaybeFlushMemory_NilSvc(t *testing.T) {
	h := &ChatHandler{memorySvc: nil}
	result := &dag.RunResult{EvalOK: true, ConfidenceScore: 0.9, FinalOutput: "answer"}
	spec := &agentstore.AgentSpec{
		MemoryPolicy: agentstore.MemoryPolicy{WriteOnEvalOK: true, MinScoreToWrite: 0.7},
	}
	// Must not panic.
	h.maybeFlushMemory("u1", "sess1", result, spec)
}

// TestMaybeFlushMemory_WritesOnSuccess verifies WriteEntry is called when
// eval_ok=true and score >= MinScoreToWrite.
// maybeFlushMemory fires in a goroutine, so we synchronise via a channel.
func TestMaybeFlushMemory_WritesOnSuccess(t *testing.T) {
	srv := fakeOllama(t)
	defer srv.Close()

	wrote := make(chan struct{}, 1)
	db := &memMockDB{onExec: func() { wrote <- struct{}{} }}
	h := newHandlerWithMemory(db, srv.URL)

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

// TestMaybeFlushMemory_SkipsOnLowScore verifies no write when score < threshold.
func TestMaybeFlushMemory_SkipsOnLowScore(t *testing.T) {
	srv := fakeOllama(t)
	defer srv.Close()

	db := &memMockDB{}
	h := newHandlerWithMemory(db, srv.URL)

	result := &dag.RunResult{EvalOK: true, ConfidenceScore: 0.3, FinalOutput: "answer"}
	spec := &agentstore.AgentSpec{
		MemoryPolicy: agentstore.MemoryPolicy{WriteOnEvalOK: true, MinScoreToWrite: 0.7},
	}
	h.maybeFlushMemory("u1", "sess1", result, spec)

	// Give a goroutine time to fire if it incorrectly does so.
	time.Sleep(100 * time.Millisecond)
	if db.execCalled {
		t.Fatal("expected WriteEntry to be skipped for score 0.3 < 0.7")
	}
}

// TestMaybeFlushMemory_SkipsOnEvalNotOK verifies no write when EvalOK=false.
func TestMaybeFlushMemory_SkipsOnEvalNotOK(t *testing.T) {
	srv := fakeOllama(t)
	defer srv.Close()

	db := &memMockDB{}
	h := newHandlerWithMemory(db, srv.URL)

	result := &dag.RunResult{EvalOK: false, ConfidenceScore: 0.9, FinalOutput: "answer"}
	spec := &agentstore.AgentSpec{
		MemoryPolicy: agentstore.MemoryPolicy{WriteOnEvalOK: true, MinScoreToWrite: 0.7},
	}
	h.maybeFlushMemory("u1", "sess1", result, spec)

	time.Sleep(100 * time.Millisecond)
	if db.execCalled {
		t.Fatal("expected WriteEntry to be skipped when EvalOK=false")
	}
}

// TestMaybeFlushMemory_SkipsWhenPolicyDisabled verifies no write when
// WriteOnEvalOK=false regardless of score.
func TestMaybeFlushMemory_SkipsWhenPolicyDisabled(t *testing.T) {
	srv := fakeOllama(t)
	defer srv.Close()

	db := &memMockDB{}
	h := newHandlerWithMemory(db, srv.URL)

	result := &dag.RunResult{EvalOK: true, ConfidenceScore: 1.0, FinalOutput: "answer"}
	spec := &agentstore.AgentSpec{
		MemoryPolicy: agentstore.MemoryPolicy{WriteOnEvalOK: false, MinScoreToWrite: 0.0},
	}
	h.maybeFlushMemory("u1", "sess1", result, spec)

	time.Sleep(100 * time.Millisecond)
	if db.execCalled {
		t.Fatal("expected WriteEntry to be skipped when WriteOnEvalOK=false")
	}
}
