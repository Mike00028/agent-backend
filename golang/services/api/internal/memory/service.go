package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mike00028/golang-backend/services/api/internal/dag"
)

// ── Ollama embedding client ───────────────────────────────────────────────────

type embedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type embedResponse struct {
	Embedding []float32 `json:"embedding"`
}

func embed(ctx context.Context, ollamaURL, model, text string, hc *http.Client) ([]float32, error) {
	payload, _ := json.Marshal(embedRequest{Model: model, Prompt: text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaURL+"/api/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding HTTP %d: %s", resp.StatusCode, b)
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode embedding: %w", err)
	}
	return result.Embedding, nil
}

// ── DB interface (pgx-compatible) ────────────────────────────────────────────

// Rows is an alias of dag.PgxRows so pgAdapter can satisfy both interfaces.
type Rows = dag.PgxRows

// DB is the minimal Postgres interface required by Service.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (interface{ RowsAffected() int64 }, error)
}

// ── Service ───────────────────────────────────────────────────────────────────

// Service provides semantic memory read and write for agents.
type Service struct {
	db         DB
	ollamaURL  string
	embedModel string
	httpClient *http.Client
}

// New creates a Service.
func New(db DB, ollamaURL, embedModel string) *Service {
	return &Service{
		db:         db,
		ollamaURL:  ollamaURL,
		embedModel: embedModel,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// ── Read: semantic context retrieval ─────────────────────────────────────────

// Retrieve returns the top-k most relevant memory chunks for the given text
// and user, formatted as plain text ready to inject into the planner prompt.
// Returns an empty string (no error) if the DB or embedding call fails — a
// memory miss should never block a request.
func (s *Service) Retrieve(ctx context.Context, userID, text string, topK int) string {
	vec, err := embed(ctx, s.ollamaURL, s.embedModel, text, s.httpClient)
	if err != nil {
		return "" // soft failure
	}

	// pgvector cosine distance query — <=> operator, ascending = most similar first
	rows, err := s.db.Query(ctx, `
		SELECT content
		FROM agent_memory_log
		WHERE user_id = $1
		ORDER BY embedding <=> $2::vector
		LIMIT $3`,
		userID, vectorLiteral(vec), topK,
	)
	if err != nil {
		return ""
	}
	defer rows.Close()

	var chunks []string
	for rows.Next() {
		var chunk string
		if err := rows.Scan(&chunk); err != nil {
			continue
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) == 0 {
		return ""
	}
	return strings.Join(chunks, "\n---\n")
}

// ── Write: memory flush after successful invocation ───────────────────────────

// WriteEntry persists a new memory entry.
// Only call this after eval_ok=true and score >= minScore.
func (s *Service) WriteEntry(ctx context.Context, userID, sessionID, content, memoryType string) error {
	vec, err := embed(ctx, s.ollamaURL, s.embedModel, content, s.httpClient)
	if err != nil {
		return fmt.Errorf("embed for write: %w", err)
	}

	_, err = s.db.Exec(ctx, `
		INSERT INTO agent_memory_log
			(session_id, user_id, memory_type, content, embedding)
		VALUES ($1, $2, $3, $4, $5::vector)`,
		sessionID, userID, memoryType, content, vectorLiteral(vec),
	)
	return err
}

// ── Helper ────────────────────────────────────────────────────────────────────

// vectorLiteral converts a float32 slice to the pgvector literal '[0.1,0.2,...]'.
func vectorLiteral(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(fmt.Sprintf("%g", f))
	}
	sb.WriteByte(']')
	return sb.String()
}
