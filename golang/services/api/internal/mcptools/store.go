package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tool is a persisted MCP tool record.
type Tool struct {
	ID          string          `json:"id"`
	ServerID    string          `json:"server_id"`
	ServerName  string          `json:"server_name"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	SchemaHash  string          `json:"schema_hash"`
	IsEnabled   bool            `json:"is_enabled"`
	LastSynced  time.Time       `json:"last_synced_at"`
}

// Server is a persisted MCP server record.
type Server struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Transport Transport  `json:"transport"`
	URL       string     `json:"url,omitempty"`
	Command   string     `json:"command,omitempty"`
	Args      []string   `json:"args,omitempty"`
	AuthType  string     `json:"auth_type,omitempty"`
	AuthToken string     `json:"auth_secret,omitempty"`
	IsEnabled bool       `json:"is_enabled"`
	LastSync  *time.Time `json:"last_sync,omitempty"`
	SyncError string     `json:"sync_error,omitempty"`
}

// Store persists MCP servers and tools in PostgreSQL with hybrid search.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates a new MCP tool store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// UpsertServer inserts or updates an MCP server config.
func (s *Store) UpsertServer(ctx context.Context, srv Server) (string, error) {
	if srv.ID == "" {
		srv.ID = uuid.NewString()
	}
	argsJSON, _ := json.Marshal(srv.Args)

	_, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_servers (id, name, transport, url, command, args, auth_type, auth_secret, is_enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (name) DO UPDATE SET
			transport   = EXCLUDED.transport,
			url         = EXCLUDED.url,
			command     = EXCLUDED.command,
			args        = EXCLUDED.args,
			auth_type   = EXCLUDED.auth_type,
			auth_secret = EXCLUDED.auth_secret,
			is_enabled  = EXCLUDED.is_enabled,
			updated_at  = now()
	`, srv.ID, srv.Name, srv.Transport, srv.URL, srv.Command, argsJSON, srv.AuthType, srv.AuthToken, srv.IsEnabled)
	if err != nil {
		return "", fmt.Errorf("upsert mcp server: %w", err)
	}
	return srv.ID, nil
}

// ListServers returns all enabled MCP servers.
func (s *Store) ListServers(ctx context.Context) ([]Server, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, transport, coalesce(url,''), coalesce(command,''),
		       coalesce(args,'[]'::jsonb), coalesce(auth_type,''), coalesce(auth_secret,''),
		       is_enabled, last_sync, coalesce(sync_error,'')
		FROM mcp_servers
		WHERE is_enabled = true
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var servers []Server
	for rows.Next() {
		var srv Server
		var argsJSON []byte
		if err := rows.Scan(&srv.ID, &srv.Name, &srv.Transport, &srv.URL, &srv.Command,
			&argsJSON, &srv.AuthType, &srv.AuthToken, &srv.IsEnabled, &srv.LastSync, &srv.SyncError); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(argsJSON, &srv.Args)
		servers = append(servers, srv)
	}
	return servers, rows.Err()
}

// SetServerSyncStatus updates the last sync time and error for a server.
func (s *Store) SetServerSyncStatus(ctx context.Context, serverID string, syncErr string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE mcp_servers SET last_sync = now(), sync_error = $2, updated_at = now()
		WHERE id = $1
	`, serverID, syncErr)
	return err
}

// UpsertTool inserts or updates an MCP tool. Returns true if the tool was new or changed.
func (s *Store) UpsertTool(ctx context.Context, serverID string, tool ToolDef) (changed bool, err error) {
	hash := schemaHash(tool.InputSchema)

	// Check if tool exists with same hash (no change)
	var existingHash string
	err = s.pool.QueryRow(ctx, `
		SELECT coalesce(schema_hash, '') FROM mcp_tools
		WHERE server_id = $1 AND name = $2
	`, serverID, tool.Name).Scan(&existingHash)

	if err == nil && existingHash == hash {
		// Update last_synced_at but no embedding needed
		_, _ = s.pool.Exec(ctx, `
			UPDATE mcp_tools SET last_synced_at = now(), updated_at = now()
			WHERE server_id = $1 AND name = $2
		`, serverID, tool.Name)
		return false, nil
	}

	inputSchema := tool.InputSchema
	if len(inputSchema) == 0 {
		inputSchema = json.RawMessage("{}")
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO mcp_tools (id, server_id, name, description, input_schema, schema_hash, last_synced_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (server_id, name) DO UPDATE SET
			description    = EXCLUDED.description,
			input_schema   = EXCLUDED.input_schema,
			schema_hash    = EXCLUDED.schema_hash,
			last_synced_at = now(),
			updated_at     = now()
	`, uuid.NewString(), serverID, tool.Name, tool.Description, inputSchema, hash)
	if err != nil {
		return false, fmt.Errorf("upsert mcp tool: %w", err)
	}
	return true, nil
}

// SetToolEmbedding stores the vector embedding for a tool.
func (s *Store) SetToolEmbedding(ctx context.Context, serverID, toolName string, embedding []float32) error {
	embJSON, err := json.Marshal(embedding)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE mcp_tools SET embedding = $3::vector, updated_at = now()
		WHERE server_id = $1 AND name = $2
	`, serverID, toolName, string(embJSON))
	return err
}

// RemoveStaleTools deletes tools not seen in the latest sync for a given server.
func (s *Store) RemoveStaleTools(ctx context.Context, serverID string, currentNames []string) error {
	if len(currentNames) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		DELETE FROM mcp_tools
		WHERE server_id = $1 AND name != ALL($2)
	`, serverID, currentNames)
	return err
}

// SearchTools performs hybrid search: keyword (tsvector) + vector similarity.
// Returns up to `limit` matching tools ranked by combined score.
func (s *Store) SearchTools(ctx context.Context, query string, queryEmbedding []float32, limit int) ([]Tool, error) {
	embJSON, err := json.Marshal(queryEmbedding)
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		WITH keyword_match AS (
			SELECT id, ts_rank(to_tsvector('english', search_text), plainto_tsquery('english', $1)) AS kw_score
			FROM mcp_tools
			WHERE is_enabled = true
			  AND to_tsvector('english', search_text) @@ plainto_tsquery('english', $1)
		),
		vector_match AS (
			SELECT id, 1 - (embedding <=> $2::vector) AS vec_score
			FROM mcp_tools
			WHERE is_enabled = true
			  AND embedding IS NOT NULL
		),
		combined AS (
			SELECT
				coalesce(k.id, v.id) AS id,
				coalesce(k.kw_score, 0) * 0.3 + coalesce(v.vec_score, 0) * 0.7 AS score
			FROM keyword_match k
			FULL OUTER JOIN vector_match v ON k.id = v.id
		)
		SELECT t.id, t.server_id, s.name AS server_name, t.name, t.description,
		       t.input_schema, coalesce(t.schema_hash,''), t.is_enabled, t.last_synced_at
		FROM combined c
		JOIN mcp_tools t ON t.id = c.id
		JOIN mcp_servers s ON s.id = t.server_id
		WHERE c.score > 0.1
		ORDER BY c.score DESC
		LIMIT $3
	`, query, string(embJSON), limit)
	if err != nil {
		return nil, fmt.Errorf("mcp search tools: %w", err)
	}
	defer rows.Close()

	var tools []Tool
	for rows.Next() {
		var t Tool
		if err := rows.Scan(&t.ID, &t.ServerID, &t.ServerName, &t.Name, &t.Description,
			&t.InputSchema, &t.SchemaHash, &t.IsEnabled, &t.LastSynced); err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, rows.Err()
}

// ListAllTools returns all enabled tools (no search filter).
func (s *Store) ListAllTools(ctx context.Context) ([]Tool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.server_id, s.name AS server_name, t.name, t.description,
		       t.input_schema, coalesce(t.schema_hash,''), t.is_enabled, t.last_synced_at
		FROM mcp_tools t
		JOIN mcp_servers s ON s.id = t.server_id
		WHERE t.is_enabled = true
		ORDER BY s.name, t.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tools []Tool
	for rows.Next() {
		var t Tool
		if err := rows.Scan(&t.ID, &t.ServerID, &t.ServerName, &t.Name, &t.Description,
			&t.InputSchema, &t.SchemaHash, &t.IsEnabled, &t.LastSynced); err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, rows.Err()
}

// LogToolCall records a tool invocation for auditing.
func (s *Store) LogToolCall(ctx context.Context, toolID, sessionID, taskID string,
	input json.RawMessage, output json.RawMessage, status string, errMsg string, durationMs int) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_tool_calls (id, tool_id, session_id, task_id, input_json, output_json, status, error_msg, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, uuid.NewString(), toolID, sessionID, taskID, input, output, status, errMsg, durationMs)
	return err
}

func schemaHash(schema json.RawMessage) string {
	if len(schema) == 0 {
		return ""
	}
	h := sha256.Sum256(schema)
	return fmt.Sprintf("%x", h)
}
