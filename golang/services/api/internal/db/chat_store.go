package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ── Domain types ──────────────────────────────────────────────────────────────

// Conversation is a long-lived chat thread that may span many agent sessions.
type Conversation struct {
	ID            string     `json:"id"`
	UserID        string     `json:"user_id"`
	AgentID       string     `json:"agent_id"`
	Title         string     `json:"title"`
	Summary       string     `json:"summary"`
	Status        string     `json:"status"`
	MessageCount  int        `json:"message_count"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
}

// Message is one turn in a conversation.
type Message struct {
	ID              string    `json:"id"`
	ConversationID  string    `json:"conversation_id"`
	SessionID       string    `json:"session_id,omitempty"`
	Role            string    `json:"role"`
	Content         string    `json:"content"`
	ContentType     string    `json:"content_type"`
	ToolName        string    `json:"tool_name,omitempty"`
	EvalOK          *bool     `json:"eval_ok,omitempty"`
	ConfidenceScore *float64  `json:"confidence_score,omitempty"`
	Model           string    `json:"model,omitempty"`
	UsageTokens     int       `json:"usage_tokens,omitempty"`
	LatencyMs       int       `json:"latency_ms,omitempty"`
	TraceID         string    `json:"trace_id,omitempty"`
	Sequence        int       `json:"sequence"`
	CreatedAt       time.Time `json:"created_at"`
}

// AddMessageParams are the input fields for ChatStore.AddMessage.
type AddMessageParams struct {
	ConversationID  string
	SessionID       string
	Role            string
	Content         string
	ContentType     string // defaults to "text"
	ToolName        string
	EvalOK          *bool
	ConfidenceScore *float64
	Model           string
	UsageTokens     int
	LatencyMs       int
	TraceID         string
}

// ── Store ─────────────────────────────────────────────────────────────────────

// QueryRower is the minimal pgx interface needed by ChatStore.
type QueryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) interface {
		Scan(dest ...any) error
	}
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (interface{ RowsAffected() int64 }, error)
}

// Rows is the minimal scan interface (subset of pgx.Rows).
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

// ChatStore persists conversations and messages in PostgreSQL.
type ChatStore struct {
	db QueryRower
}

// NewChatStore creates a ChatStore backed by the given QueryRower.
func NewChatStore(db QueryRower) *ChatStore {
	return &ChatStore{db: db}
}

// CreateConversation inserts a new conversation row and returns it.
func (s *ChatStore) CreateConversation(ctx context.Context, userID, agentID string) (*Conversation, error) {
	if agentID == "" {
		agentID = "default"
	}
	id := uuid.NewString()
	row := s.db.QueryRow(ctx, `
		INSERT INTO conversations (id, user_id, agent_id)
		VALUES ($1, $2, $3)
		RETURNING id, user_id, agent_id,
		          COALESCE(title,'')   AS title,
		          COALESCE(summary,'') AS summary,
		          status, message_count, created_at, updated_at, last_message_at`,
		id, userID, agentID,
	)
	return scanConversation(row)
}

// GetConversation returns a single conversation by ID.
func (s *ChatStore) GetConversation(ctx context.Context, id string) (*Conversation, error) {
	row := s.db.QueryRow(ctx, `
		SELECT id, user_id, agent_id,
		       COALESCE(title,'')   AS title,
		       COALESCE(summary,'') AS summary,
		       status, message_count, created_at, updated_at, last_message_at
		FROM   conversations
		WHERE  id = $1`, id)
	return scanConversation(row)
}

// ListConversations returns conversations for a user, newest first.
func (s *ChatStore) ListConversations(ctx context.Context, userID string, limit, offset int) ([]*Conversation, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, user_id, agent_id,
		       COALESCE(title,'')   AS title,
		       COALESCE(summary,'') AS summary,
		       status, message_count, created_at, updated_at, last_message_at
		FROM   conversations
		WHERE  user_id = $1 AND status != 'deleted'
		ORDER  BY last_message_at DESC NULLS LAST
		LIMIT  $2 OFFSET $3`,
		userID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddMessage inserts one message and returns it with the auto-set sequence.
func (s *ChatStore) AddMessage(ctx context.Context, p AddMessageParams) (*Message, error) {
	if p.ContentType == "" {
		p.ContentType = "text"
	}
	id := uuid.NewString()
	row := s.db.QueryRow(ctx, `
		INSERT INTO messages
		    (id, conversation_id, session_id, role, content, content_type,
		     tool_name, eval_ok, confidence_score, model, usage_tokens,
		     latency_ms, trace_id)
		VALUES
		    ($1,$2,NULLIF($3,''),$4,$5,$6,
		     NULLIF($7,''),$8,$9,NULLIF($10,''),$11,
		     $12,NULLIF($13,''))
		RETURNING id, conversation_id,
		          COALESCE(session_id::text,'') AS session_id,
		          role, content, content_type,
		          COALESCE(tool_name,'') AS tool_name,
		          eval_ok, confidence_score,
		          COALESCE(model,'') AS model,
		          usage_tokens, latency_ms,
		          COALESCE(trace_id,'') AS trace_id,
		          sequence, created_at`,
		id, p.ConversationID, p.SessionID, p.Role, p.Content, p.ContentType,
		p.ToolName, p.EvalOK, p.ConfidenceScore, p.Model, p.UsageTokens,
		p.LatencyMs, p.TraceID,
	)
	return scanMessage(row)
}

// ListMessages returns all messages for a conversation in order.
func (s *ChatStore) ListMessages(ctx context.Context, conversationID string, limit, offset int) ([]*Message, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, conversation_id,
		       COALESCE(session_id::text,'') AS session_id,
		       role, content, content_type,
		       COALESCE(tool_name,'') AS tool_name,
		       eval_ok, confidence_score,
		       COALESCE(model,'') AS model,
		       usage_tokens, latency_ms,
		       COALESCE(trace_id,'') AS trace_id,
		       sequence, created_at
		FROM   messages
		WHERE  conversation_id = $1
		ORDER  BY sequence
		LIMIT  $2 OFFSET $3`,
		conversationID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ── Scan helpers ──────────────────────────────────────────────────────────────

type scanner interface {
	Scan(dest ...any) error
}

func scanConversation(s scanner) (*Conversation, error) {
	var c Conversation
	err := s.Scan(
		&c.ID, &c.UserID, &c.AgentID,
		&c.Title, &c.Summary,
		&c.Status, &c.MessageCount,
		&c.CreatedAt, &c.UpdatedAt, &c.LastMessageAt,
	)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func scanMessage(s scanner) (*Message, error) {
	var m Message
	err := s.Scan(
		&m.ID, &m.ConversationID, &m.SessionID,
		&m.Role, &m.Content, &m.ContentType,
		&m.ToolName, &m.EvalOK, &m.ConfidenceScore,
		&m.Model, &m.UsageTokens, &m.LatencyMs,
		&m.TraceID, &m.Sequence, &m.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &m, nil
}
