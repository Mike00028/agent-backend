package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── fake store ────────────────────────────────────────────────────────────────

type fakeStore struct {
	mu            sync.Mutex
	servers       []Server
	upsertToolFn  func(serverID string, tool ToolDef) (bool, error)
	setEmbedCalls int
	listAllTools  []Tool
	searchTools   []Tool
}

func (f *fakeStore) ListServers(_ context.Context) ([]Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.servers, nil
}

func (f *fakeStore) UpsertServer(_ context.Context, srv Server) (string, error) {
	if srv.ID == "" {
		return "fake-id", nil
	}
	return srv.ID, nil
}

func (f *fakeStore) SetServerSyncStatus(_ context.Context, _ string, _ string) error {
	return nil
}

func (f *fakeStore) UpsertTool(_ context.Context, serverID string, tool ToolDef) (bool, error) {
	if f.upsertToolFn != nil {
		return f.upsertToolFn(serverID, tool)
	}
	return true, nil
}

func (f *fakeStore) SetToolEmbedding(_ context.Context, _, _ string, _ []float32) error {
	f.mu.Lock()
	f.setEmbedCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) RemoveStaleTools(_ context.Context, _ string, _ []string) error {
	return nil
}

func (f *fakeStore) SearchTools(_ context.Context, _ string, _ []float32, _ int) ([]Tool, error) {
	return f.searchTools, nil
}

func (f *fakeStore) ListAllTools(_ context.Context) ([]Tool, error) {
	return f.listAllTools, nil
}

func (f *fakeStore) LogToolCall(_ context.Context, _, _, _ string, _ json.RawMessage, _ json.RawMessage, _, _ string, _ int) error {
	return nil
}

// ── fake embedder ─────────────────────────────────────────────────────────────

type fakeEmbedder struct {
	mu    sync.Mutex
	calls int
	vec   []float32
	err   error
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.vec, nil
}

// ── fake client ───────────────────────────────────────────────────────────────

type fakeClient struct {
	tools  []ToolDef
	result *ToolCallResult
	err    error
}

func (f *fakeClient) ListTools(_ context.Context) ([]ToolDef, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.tools, nil
}

func (f *fakeClient) CallTool(_ context.Context, _ string, _ json.RawMessage) (*ToolCallResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeClient) Close() {}

// ── helper ────────────────────────────────────────────────────────────────────

// signalReady manually closes syncReady without running StartPeriodicSync.
// Used to put the manager into the "post-sync" state for tests that only
// care about behaviour after the first sync is done.
func signalReady(m *Manager) {
	m.syncOnce.Do(func() { close(m.syncReady) })
}

// injectClient places a fake client into the manager's client map.
func injectClient(m *Manager, serverName string, c mcpClient) {
	m.mu.Lock()
	m.clients[serverName] = c
	m.mu.Unlock()
}

// ── Ready channel tests ───────────────────────────────────────────────────────

// TestNewManager_SyncNotReady verifies that a fresh Manager's Ready() channel
// is open (i.e. syncReady is NOT yet closed).
func TestNewManager_SyncNotReady(t *testing.T) {
	m := NewManager(&fakeStore{}, nil)
	select {
	case <-m.Ready():
		t.Fatal("Ready() should not be closed before StartPeriodicSync")
	default:
		// expected: channel still open
	}
}

// TestReady_ClosesAfterStartPeriodicSync verifies that Ready() is eventually
// closed once StartPeriodicSync runs its first SyncAll (with no servers).
func TestReady_ClosesAfterStartPeriodicSync(t *testing.T) {
	m := NewManager(&fakeStore{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Large interval so only the initial sync runs during the test.
	m.StartPeriodicSync(ctx, time.Hour)

	select {
	case <-m.Ready():
		// expected
	case <-ctx.Done():
		t.Fatal("Ready() not closed within 2 s")
	}
}

// TestReady_IdempotentAfterMultipleSyncAll verifies that calling SyncAll
// multiple times does not panic or deadlock (syncOnce protects close).
func TestReady_IdempotentAfterMultipleSyncAll(t *testing.T) {
	m := NewManager(&fakeStore{}, nil)
	ctx := context.Background()

	// Two concurrent SyncAll calls must not panic.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = m.SyncAll(ctx) }()
	go func() { defer wg.Done(); _ = m.SyncAll(ctx) }()
	wg.Wait()

	// Close the ready channel via syncOnce (simulates StartPeriodicSync).
	m.syncOnce.Do(func() { close(m.syncReady) })

	select {
	case <-m.Ready():
		// expected
	default:
		t.Fatal("Ready() should be closed after syncOnce.Do")
	}
}

// ── SearchTools tests ─────────────────────────────────────────────────────────

// TestSearchTools_BlocksOnContextCancel verifies that SearchTools returns
// the context error when the context is cancelled before syncReady closes.
func TestSearchTools_BlocksOnContextCancel(t *testing.T) {
	m := NewManager(&fakeStore{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := m.SearchTools(ctx, "query", 5)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

// TestSearchTools_ReturnsAllToolsWhenNoEmbedder verifies that with a nil
// embedder, SearchTools falls back to ListAllTools.
func TestSearchTools_ReturnsAllToolsWhenNoEmbedder(t *testing.T) {
	want := []Tool{{Name: "read_file", ServerName: "fs"}}
	store := &fakeStore{listAllTools: want}
	m := NewManager(store, nil)
	signalReady(m)

	got, err := m.SearchTools(context.Background(), "any query", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Name != want[0].Name {
		t.Fatalf("expected %+v, got %+v", want, got)
	}
}

// TestSearchTools_UsesEmbedderAndStore verifies that with an embedder,
// SearchTools calls Embed and then store.SearchTools (not ListAllTools).
func TestSearchTools_UsesEmbedderAndStore(t *testing.T) {
	want := []Tool{{Name: "web_search", ServerName: "web"}}
	store := &fakeStore{searchTools: want}
	emb := &fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	m := NewManager(store, emb)
	signalReady(m)

	got, err := m.SearchTools(context.Background(), "find something", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if emb.calls != 1 {
		t.Fatalf("expected Embed called once, got %d", emb.calls)
	}
	if len(got) != 1 || got[0].Name != want[0].Name {
		t.Fatalf("expected %+v, got %+v", want, got)
	}
}

// TestSearchTools_EmbedFailureFallsBack verifies that when Embed returns an
// error, SearchTools falls back to ListAllTools.
func TestSearchTools_EmbedFailureFallsBack(t *testing.T) {
	want := []Tool{{Name: "fallback_tool", ServerName: "srv"}}
	store := &fakeStore{listAllTools: want}
	emb := &fakeEmbedder{err: errors.New("ollama unreachable")}
	m := NewManager(store, emb)
	signalReady(m)

	got, err := m.SearchTools(context.Background(), "query", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Name != want[0].Name {
		t.Fatalf("expected fallback result %+v, got %+v", want, got)
	}
}

// ── CallTool tests ────────────────────────────────────────────────────────────

// TestCallTool_BlocksOnContextCancel verifies that CallTool returns the
// context error when cancelled before syncReady closes.
func TestCallTool_BlocksOnContextCancel(t *testing.T) {
	m := NewManager(&fakeStore{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := m.CallTool(ctx, "fs", "read_file", json.RawMessage(`{}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

// TestCallTool_UnknownServer verifies that calling a tool on an unregistered
// server returns a "not connected" error.
func TestCallTool_UnknownServer(t *testing.T) {
	m := NewManager(&fakeStore{}, nil)
	signalReady(m)

	_, err := m.CallTool(context.Background(), "nonexistent", "tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for unknown server, got nil")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("expected 'not connected' error, got: %v", err)
	}
}

// TestCallTool_RoutesToCorrectClient verifies that CallTool routes to the
// client registered under the given server name.
func TestCallTool_RoutesToCorrectClient(t *testing.T) {
	m := NewManager(&fakeStore{}, nil)
	signalReady(m)

	fc := &fakeClient{result: &ToolCallResult{
		Content: []ContentBlock{{Type: "text", Text: "hello"}},
	}}
	injectClient(m, "myserver", fc)

	result, err := m.CallTool(context.Background(), "myserver", "greet", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Content) == 0 || result.Content[0].Text != "hello" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

// TestCallTool_ClientError propagates client errors.
func TestCallTool_ClientError(t *testing.T) {
	m := NewManager(&fakeStore{}, nil)
	signalReady(m)

	fc := &fakeClient{err: errors.New("subprocess died")}
	injectClient(m, "broken", fc)

	_, err := m.CallTool(context.Background(), "broken", "any_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error from client, got nil")
	}
	if !strings.Contains(err.Error(), "subprocess died") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// ── SyncAll tests ─────────────────────────────────────────────────────────────

// TestSyncAll_NoServers verifies SyncAll returns nil when no servers exist.
func TestSyncAll_NoServers(t *testing.T) {
	m := NewManager(&fakeStore{}, nil)
	if err := m.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSyncAll_ToolChanged_CallsEmbedder verifies that when UpsertTool reports
// changed=true, the embedder is called and the embedding is stored.
func TestSyncAll_ToolChanged_CallsEmbedder(t *testing.T) {
	srv := Server{ID: "srv-1", Name: "myserver", Transport: TransportStdio}
	store := &fakeStore{
		servers: []Server{srv},
		upsertToolFn: func(_ string, _ ToolDef) (bool, error) {
			return true, nil // always report changed
		},
	}
	emb := &fakeEmbedder{vec: []float32{0.5, 0.6}}
	m := NewManager(store, emb)

	// Inject a fake client so SyncAll doesn't try to start a real subprocess.
	injectClient(m, srv.Name, &fakeClient{
		tools: []ToolDef{{Name: "greet", Description: "says hello"}},
	})

	if err := m.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if emb.calls != 1 {
		t.Fatalf("expected Embed called once, got %d", emb.calls)
	}
	if store.setEmbedCalls != 1 {
		t.Fatalf("expected SetToolEmbedding called once, got %d", store.setEmbedCalls)
	}
}

// TestSyncAll_ToolUnchanged_SkipsEmbedder verifies that when UpsertTool
// reports changed=false (hash match), the embedder is NOT called.
func TestSyncAll_ToolUnchanged_SkipsEmbedder(t *testing.T) {
	srv := Server{ID: "srv-2", Name: "myserver2", Transport: TransportStdio}
	store := &fakeStore{
		servers: []Server{srv},
		upsertToolFn: func(_ string, _ ToolDef) (bool, error) {
			return false, nil // always unchanged
		},
	}
	emb := &fakeEmbedder{vec: []float32{0.5}}
	m := NewManager(store, emb)
	injectClient(m, srv.Name, &fakeClient{
		tools: []ToolDef{{Name: "greet", Description: "says hello"}},
	})

	if err := m.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if emb.calls != 0 {
		t.Fatalf("expected Embed not called (unchanged), got %d calls", emb.calls)
	}
	if store.setEmbedCalls != 0 {
		t.Fatalf("expected SetToolEmbedding not called, got %d calls", store.setEmbedCalls)
	}
}

// TestSyncAll_ClientListToolsError_ContinuesOtherServers verifies that a
// failing server is skipped but SyncAll still returns nil.
func TestSyncAll_ClientListToolsError_ContinuesOtherServers(t *testing.T) {
	goodSrv := Server{ID: "good", Name: "good-server", Transport: TransportStdio}
	badSrv := Server{ID: "bad", Name: "bad-server", Transport: TransportStdio}
	store := &fakeStore{
		servers: []Server{badSrv, goodSrv},
		upsertToolFn: func(_ string, _ ToolDef) (bool, error) {
			return true, nil
		},
	}
	emb := &fakeEmbedder{vec: []float32{0.1}}
	m := NewManager(store, emb)

	injectClient(m, badSrv.Name, &fakeClient{err: errors.New("connection refused")})
	injectClient(m, goodSrv.Name, &fakeClient{
		tools: []ToolDef{{Name: "ok_tool", Description: "works"}},
	})

	if err := m.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll should not return an error even when one server fails: %v", err)
	}
	// Good server's tool was embedded.
	if emb.calls != 1 {
		t.Fatalf("expected Embed called once (for good-server), got %d", emb.calls)
	}
}

// TestSyncAll_NoEmbedder_SkipsEmbedding verifies that nil embedder means no
// embedding is attempted even for changed tools.
func TestSyncAll_NoEmbedder_SkipsEmbedding(t *testing.T) {
	srv := Server{ID: "srv-3", Name: "embed-less", Transport: TransportStdio}
	store := &fakeStore{
		servers: []Server{srv},
		upsertToolFn: func(_ string, _ ToolDef) (bool, error) {
			return true, nil
		},
	}
	m := NewManager(store, nil) // no embedder
	injectClient(m, srv.Name, &fakeClient{
		tools: []ToolDef{{Name: "noembed_tool", Description: "desc"}},
	})

	if err := m.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.setEmbedCalls != 0 {
		t.Fatalf("expected SetToolEmbedding not called with nil embedder, got %d calls", store.setEmbedCalls)
	}
}

// ── Close tests ───────────────────────────────────────────────────────────────

// TestClose_ClearsClientMap verifies that Close() empties the client map.
func TestClose_ClearsClientMap(t *testing.T) {
	m := NewManager(&fakeStore{}, nil)
	injectClient(m, "srv1", &fakeClient{})
	injectClient(m, "srv2", &fakeClient{})

	m.Close()

	m.mu.RLock()
	count := len(m.clients)
	m.mu.RUnlock()

	if count != 0 {
		t.Fatalf("expected empty client map after Close(), got %d clients", count)
	}
}

// ── RegisterServer tests ──────────────────────────────────────────────────────

// TestRegisterServer_AddsClientToMap verifies that RegisterServer upserts the
// server in the store and registers a client under the server's name.
func TestRegisterServer_AddsClientToMap(t *testing.T) {
	m := NewManager(&fakeStore{}, nil)

	cfg := ServerConfig{
		Name:      "test-server",
		Transport: TransportStdio,
		Command:   "echo",
		Args:      []string{"hello"},
	}
	if err := m.RegisterServer(context.Background(), cfg); err != nil {
		t.Fatalf("RegisterServer failed: %v", err)
	}

	m.mu.RLock()
	_, ok := m.clients["test-server"]
	m.mu.RUnlock()

	if !ok {
		t.Fatal("expected client registered for 'test-server'")
	}
}
