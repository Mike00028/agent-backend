package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mike00028/golang-backend/services/api/internal/agentstore"
	"github.com/mike00028/golang-backend/services/api/internal/memory"
)

// ── local DB mock (supports returning rows) ───────────────────────────────────

type chRows struct {
	data []string
	idx  int
}

func (r *chRows) Next() bool { r.idx++; return r.idx <= len(r.data) }
func (r *chRows) Close()     {}
func (r *chRows) Err() error { return nil }
func (r *chRows) Scan(dst ...any) error {
	*dst[0].(*string) = r.data[r.idx-1]
	return nil
}

type chDB struct{ rows *chRows }

func (d *chDB) Query(_ context.Context, _ string, _ ...any) (memory.Rows, error) {
	return d.rows, nil
}
func (d *chDB) Exec(_ context.Context, _ string, _ ...any) (interface{ RowsAffected() int64 }, error) {
	return chResult{}, nil
}

type chResult struct{}

func (chResult) RowsAffected() int64 { return 1 }

func TestNewChatHandler_NotNil(t *testing.T) {
	h := NewChatHandler(nil, nil, agentstore.New(&agentstore.AgentSpec{ID: "x"}), nil, nil, nil, nil, "plan", "chat", "eval")
	if h == nil {
		t.Fatal("expected non-nil ChatHandler")
	}
	if h.planModel != "plan" || h.chatModel != "chat" || h.evalModel != "eval" {
		t.Errorf("model fields not set correctly: plan=%q chat=%q eval=%q", h.planModel, h.chatModel, h.evalModel)
	}
}

func TestBuildRunRequest_HappyPath(t *testing.T) {
	spec := &agentstore.AgentSpec{
		ID:           "default",
		SystemPrompt: "you are helpful",
		Tools:        []string{"chat_agent"},
		MemoryPolicy: agentstore.MemoryPolicy{TopKRead: 0}, // memory disabled
	}
	h := &ChatHandler{
		agentStore: agentstore.New(spec),
		memorySvc:  nil, // memory disabled
	}

	req := chatRequest{
		Message:   "hello",
		SessionID: "sess-1",
		AgentID:   "default",
		UserID:    "u1",
	}

	runReq, gotSpec, err := h.buildRunRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runReq.Message != "hello" {
		t.Errorf("Message = %q, want %q", runReq.Message, "hello")
	}
	if runReq.SessionID != "sess-1" {
		t.Errorf("SessionID = %q", runReq.SessionID)
	}
	if runReq.UserID != "u1" {
		t.Errorf("UserID = %q", runReq.UserID)
	}
	if runReq.MemoryContext != "" {
		t.Errorf("expected empty MemoryContext when memorySvc=nil, got %q", runReq.MemoryContext)
	}
	if gotSpec.ID != "default" {
		t.Errorf("spec.ID = %q, want %q", gotSpec.ID, "default")
	}
	if runReq.AgentSpecJSON == "" {
		t.Error("expected non-empty AgentSpecJSON")
	}
}

func TestBuildRunRequest_WithMemory(t *testing.T) {
	// Fake Ollama returns a valid embedding so Retrieve doesn't fail
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"embedding":[0.1,0.2,0.3]}`)) //nolint:errcheck
	}))
	defer srv.Close()

	// DB returns one memory chunk
	db := &chDB{rows: &chRows{data: []string{"prior context"}}}

	spec := &agentstore.AgentSpec{
		ID:           "default",
		MemoryPolicy: agentstore.MemoryPolicy{TopKRead: 3},
	}

	memorySvc := memory.New(db, srv.URL, "nomic-embed-text")
	h := &ChatHandler{
		agentStore: agentstore.New(spec),
		memorySvc:  memorySvc,
	}

	runReq, _, err := h.buildRunRequest(context.Background(), chatRequest{
		Message: "what did I say before?", AgentID: "default", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runReq.MemoryContext == "" {
		t.Error("expected MemoryContext to be populated when memorySvc is set and TopKRead > 0")
	}
}
