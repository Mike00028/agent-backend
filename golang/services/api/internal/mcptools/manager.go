package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Embedder computes vector embeddings for tool descriptions.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// OllamaEmbedder calls Ollama's /api/embeddings endpoint.
type OllamaEmbedder struct {
	url    string
	model  string
	client *http.Client
}

// NewOllamaEmbedder creates an embedder that calls Ollama.
func NewOllamaEmbedder(ollamaURL, model string) *OllamaEmbedder {
	return &OllamaEmbedder{
		url:    ollamaURL,
		model:  model,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (e *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	payload, _ := json.Marshal(map[string]string{"model": e.model, "prompt": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/api/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embedding: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ollama embedding HTTP %d: %s", resp.StatusCode, b)
	}

	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode embedding: %w", err)
	}
	return result.Embedding, nil
}

// Manager coordinates MCP server syncing, tool retrieval, and tool execution.
// It holds a pool of MCP clients keyed by server name.
type Manager struct {
	store    *Store
	embedder Embedder
	mu       sync.RWMutex
	clients  map[string]*Client // keyed by server name
}

// NewManager creates a new MCP tool manager.
func NewManager(store *Store, embedder Embedder) *Manager {
	return &Manager{
		store:    store,
		embedder: embedder,
		clients:  make(map[string]*Client),
	}
}

// SyncAll discovers tools from all enabled MCP servers, persists them, and
// embeds new/changed tool descriptions. Servers that fail are logged and skipped.
func (m *Manager) SyncAll(ctx context.Context) error {
	servers, err := m.store.ListServers(ctx)
	if err != nil {
		return fmt.Errorf("list mcp servers: %w", err)
	}

	for _, srv := range servers {
		if err := m.syncServer(ctx, srv); err != nil {
			slog.Warn("mcp sync failed", "server", srv.Name, "error", err)
			_ = m.store.SetServerSyncStatus(ctx, srv.ID, err.Error())
			continue
		}
		_ = m.store.SetServerSyncStatus(ctx, srv.ID, "")
	}
	return nil
}

// syncServer syncs tools from a single MCP server.
func (m *Manager) syncServer(ctx context.Context, srv Server) error {
	client := m.getOrCreateClient(srv)

	tools, err := client.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("list tools from %s: %w", srv.Name, err)
	}

	slog.Info("mcp sync", "server", srv.Name, "tools", len(tools))

	var currentNames []string
	for _, tool := range tools {
		currentNames = append(currentNames, tool.Name)

		changed, err := m.store.UpsertTool(ctx, srv.ID, tool)
		if err != nil {
			slog.Warn("mcp upsert tool failed", "server", srv.Name, "tool", tool.Name, "error", err)
			continue
		}

		// Only embed new or changed tools
		if changed && m.embedder != nil {
			text := tool.Name + ": " + tool.Description
			vec, err := m.embedder.Embed(ctx, text)
			if err != nil {
				slog.Warn("mcp embed failed", "tool", tool.Name, "error", err)
				continue
			}
			if err := m.store.SetToolEmbedding(ctx, srv.ID, tool.Name, vec); err != nil {
				slog.Warn("mcp save embedding failed", "tool", tool.Name, "error", err)
			}
		}
	}

	// Remove tools no longer advertised by the server
	if err := m.store.RemoveStaleTools(ctx, srv.ID, currentNames); err != nil {
		slog.Warn("mcp remove stale tools failed", "server", srv.Name, "error", err)
	}

	return nil
}

// SearchTools performs hybrid search for tools matching a query.
func (m *Manager) SearchTools(ctx context.Context, query string, limit int) ([]Tool, error) {
	if m.embedder == nil {
		return m.store.ListAllTools(ctx)
	}
	vec, err := m.embedder.Embed(ctx, query)
	if err != nil {
		slog.Warn("mcp search embed failed, falling back to keyword", "error", err)
		// Fallback: keyword-only search stub — just list all
		return m.store.ListAllTools(ctx)
	}
	return m.store.SearchTools(ctx, query, vec, limit)
}

// CallTool finds the right MCP client and calls the tool.
func (m *Manager) CallTool(ctx context.Context, serverName, toolName string, args json.RawMessage) (*ToolCallResult, error) {
	m.mu.RLock()
	client, ok := m.clients[serverName]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("mcp server %q not connected", serverName)
	}
	return client.CallTool(ctx, toolName, args)
}

// RegisterServer adds a server config to the DB and creates a client.
func (m *Manager) RegisterServer(ctx context.Context, cfg ServerConfig) error {
	srv := Server{
		Name:      cfg.Name,
		Transport: cfg.Transport,
		URL:       cfg.URL,
		Command:   cfg.Command,
		Args:      cfg.Args,
		AuthType:  cfg.AuthType,
		AuthToken: cfg.AuthToken,
		IsEnabled: true,
	}
	id, err := m.store.UpsertServer(ctx, srv)
	if err != nil {
		return err
	}
	srv.ID = id
	m.getOrCreateClient(srv)
	return nil
}

// Close shuts down all MCP clients.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		c.Close()
	}
	m.clients = make(map[string]*Client)
}

// StartPeriodicSync runs SyncAll every interval until ctx is cancelled.
func (m *Manager) StartPeriodicSync(ctx context.Context, interval time.Duration) {
	go func() {
		// Initial sync on startup
		if err := m.SyncAll(ctx); err != nil {
			slog.Warn("mcp initial sync failed", "error", err)
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.SyncAll(ctx); err != nil {
					slog.Warn("mcp periodic sync failed", "error", err)
				}
			}
		}
	}()
}

// getOrCreateClient returns a cached client or creates a new one.
func (m *Manager) getOrCreateClient(srv Server) *Client {
	m.mu.Lock()
	defer m.mu.Unlock()

	if c, ok := m.clients[srv.Name]; ok {
		return c
	}

	cfg := ServerConfig{
		Name:      srv.Name,
		Transport: srv.Transport,
		URL:       srv.URL,
		Command:   srv.Command,
		Args:      srv.Args,
		AuthType:  srv.AuthType,
		AuthToken: srv.AuthToken,
	}
	c := NewClient(cfg)
	m.clients[srv.Name] = c
	return c
}
