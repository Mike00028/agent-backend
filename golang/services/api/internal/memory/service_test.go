package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── mock DB ───────────────────────────────────────────────────────────────────

type mockResult struct{}

func (mockResult) RowsAffected() int64 { return 1 }

type mockRows struct {
	data []string
	idx  int
	err  error
}

func (m *mockRows) Next() bool { m.idx++; return m.idx <= len(m.data) }
func (m *mockRows) Close()     {}
func (m *mockRows) Err() error { return m.err }
func (m *mockRows) Scan(dst ...any) error {
	if len(dst) == 0 {
		return nil
	}
	*dst[0].(*string) = m.data[m.idx-1]
	return nil
}

type mockDB struct {
	rows       *mockRows
	queryErr   error
	execErr    error
	execCalled bool
}

func (m *mockDB) Query(_ context.Context, _ string, _ ...any) (Rows, error) {
	if m.queryErr != nil {
		return nil, m.queryErr
	}
	return m.rows, nil
}

func (m *mockDB) Exec(_ context.Context, _ string, _ ...any) (interface{ RowsAffected() int64 }, error) {
	m.execCalled = true
	return mockResult{}, m.execErr
}

// ── fake Ollama helpers ───────────────────────────────────────────────────────

// goodOllama returns a test server that yields a fixed 3-element embedding.
func goodOllama(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(embedResponse{Embedding: []float32{0.1, 0.2, 0.3}}) //nolint:errcheck
	}))
}

// badOllama returns a test server that always responds 500.
func badOllama(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
}

// ── Retrieve tests ────────────────────────────────────────────────────────────

func TestRetrieve_HappyPath(t *testing.T) {
	srv := goodOllama(t)
	defer srv.Close()

	db := &mockDB{rows: &mockRows{data: []string{"chunk1", "chunk2"}}}
	svc := New(db, srv.URL, "nomic-embed-text")

	got := svc.Retrieve(context.Background(), "u1", "hello", 3)
	if got != "chunk1\n---\nchunk2" {
		t.Fatalf("unexpected result: %q", got)
	}
}

func TestRetrieve_EmbedError(t *testing.T) {
	srv := badOllama(t)
	defer srv.Close()

	db := &mockDB{rows: &mockRows{data: []string{"chunk1"}}}
	svc := New(db, srv.URL, "nomic-embed-text")

	got := svc.Retrieve(context.Background(), "u1", "hello", 3)
	if got != "" {
		t.Fatalf("expected empty string on embed error, got %q", got)
	}
	if db.execCalled {
		t.Fatal("DB should not be queried when embed fails")
	}
}

func TestRetrieve_DBError(t *testing.T) {
	srv := goodOllama(t)
	defer srv.Close()

	db := &mockDB{queryErr: errors.New("db down")}
	svc := New(db, srv.URL, "nomic-embed-text")

	got := svc.Retrieve(context.Background(), "u1", "hello", 3)
	if got != "" {
		t.Fatalf("expected empty string on DB error, got %q", got)
	}
}

func TestRetrieve_Empty(t *testing.T) {
	srv := goodOllama(t)
	defer srv.Close()

	db := &mockDB{rows: &mockRows{data: []string{}}}
	svc := New(db, srv.URL, "nomic-embed-text")

	got := svc.Retrieve(context.Background(), "u1", "hello", 3)
	if got != "" {
		t.Fatalf("expected empty string when no rows, got %q", got)
	}
}

// ── WriteEntry tests ──────────────────────────────────────────────────────────

func TestWriteEntry_HappyPath(t *testing.T) {
	srv := goodOllama(t)
	defer srv.Close()

	db := &mockDB{}
	svc := New(db, srv.URL, "nomic-embed-text")

	err := svc.WriteEntry(context.Background(), "u1", "sess1", "content", "workflow")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !db.execCalled {
		t.Fatal("expected Exec to be called for INSERT")
	}
}

func TestWriteEntry_EmbedFail(t *testing.T) {
	srv := badOllama(t)
	defer srv.Close()

	db := &mockDB{}
	svc := New(db, srv.URL, "nomic-embed-text")

	err := svc.WriteEntry(context.Background(), "u1", "sess1", "content", "workflow")
	if err == nil {
		t.Fatal("expected error on embed failure")
	}
	if db.execCalled {
		t.Fatal("DB Exec should not be called when embed fails")
	}
}

func TestWriteEntry_DBFail(t *testing.T) {
	srv := goodOllama(t)
	defer srv.Close()

	db := &mockDB{execErr: errors.New("insert failed")}
	svc := New(db, srv.URL, "nomic-embed-text")

	err := svc.WriteEntry(context.Background(), "u1", "sess1", "content", "workflow")
	if err == nil {
		t.Fatal("expected error when DB Exec fails")
	}
}

// ── vectorLiteral tests ───────────────────────────────────────────────────────

func TestVectorLiteral(t *testing.T) {
	cases := []struct {
		in   []float32
		want string
	}{
		{[]float32{0.1, 0.2, 0.3}, "[0.1,0.2,0.3]"},
		{[]float32{1}, "[1]"},
		{[]float32{}, "[]"},
		{nil, "[]"},
	}
	for _, tc := range cases {
		got := vectorLiteral(tc.in)
		if got != tc.want {
			t.Errorf("vectorLiteral(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── embed request format test ─────────────────────────────────────────────────

func TestEmbed_SendsCorrectPayload(t *testing.T) {
	var gotModel, gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		gotModel = req.Model
		gotPrompt = req.Prompt
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(embedResponse{Embedding: []float32{0.5}}) //nolint:errcheck
	}))
	defer srv.Close()

	db := &mockDB{rows: &mockRows{data: []string{}}}
	svc := New(db, srv.URL, "my-model")
	svc.Retrieve(context.Background(), "u1", "test query", 1)

	if gotModel != "my-model" {
		t.Errorf("embed model = %q, want %q", gotModel, "my-model")
	}
	if gotPrompt != "test query" {
		t.Errorf("embed prompt = %q, want %q", gotPrompt, "test query")
	}
}

// ── multi-chunk join separator test ──────────────────────────────────────────

func TestRetrieve_JoinsSeparator(t *testing.T) {
	srv := goodOllama(t)
	defer srv.Close()

	db := &mockDB{rows: &mockRows{data: []string{"a", "b", "c"}}}
	svc := New(db, srv.URL, "nomic-embed-text")

	got := svc.Retrieve(context.Background(), "u1", "q", 5)
	parts := strings.Split(got, "\n---\n")
	if len(parts) != 3 {
		t.Fatalf("expected 3 chunks separated by ---; got %q", got)
	}
	for i, want := range []string{"a", "b", "c"} {
		if parts[i] != want {
			t.Errorf("chunk[%d] = %q, want %q", i, parts[i], want)
		}
	}
}

// Ensure the package compiles — catches unused imports.
var _ = fmt.Sprintf
