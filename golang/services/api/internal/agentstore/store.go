package agentstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotFound is returned when the agent_id does not exist.
var ErrNotFound = errors.New("agent not found")

// ErrForbidden is returned when the requesting user does not own the agent.
var ErrForbidden = errors.New("agent owned by another user")

// AgentSpec is the canonical spec loaded from the agents table.
// All fields map directly to the migration schema.
type AgentSpec struct {
	ID          string
	Name        string
	Description string

	SystemPrompt string
	AgentType    string // "react" | "simple"

	Model        string
	PlannerModel string
	EvalModel    string

	Tools     []string
	SubAgents []string

	ApprovalRequiredTools []string
	EvaluatorEnabled      bool
	MaxIterations         int
	MemoryPolicy          MemoryPolicy

	SandboxBackend string
	IsPublic       bool
	OwnerUserID    string
}

// MemoryPolicy controls when memory is read and written for this agent.
type MemoryPolicy struct {
	// WriteOnEvalOK writes a memory entry only after a successful eval.
	WriteOnEvalOK bool `json:"write_on_eval_ok"`
	// MinScoreToWrite is the minimum confidence score required to persist memory.
	MinScoreToWrite float64 `json:"min_score_to_write"`
	// TopKRead is how many memory chunks to inject at plan time.
	TopKRead int `json:"top_k_read"`
}

// DefaultMemoryPolicy is used when the agent has no explicit memory_policy.
var DefaultMemoryPolicy = MemoryPolicy{
	WriteOnEvalOK:   true,
	MinScoreToWrite: 0.7,
	TopKRead:        3,
}

// DB is the minimal pgx interface needed by Store.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

// Row is the minimal row interface (matches pgx.Row).
type Row interface {
	Scan(dest ...any) error
}

// Store loads agent specs from Postgres.
type Store struct {
	db DB
}

// New creates a Store.
func New(db DB) *Store {
	return &Store{db: db}
}

// Load fetches the AgentSpec for the given agentID and validates that
// requestingUserID owns it (or the agent is public).
// Returns ErrNotFound or ErrForbidden on access violations.
func (s *Store) Load(ctx context.Context, agentID, requestingUserID string) (*AgentSpec, error) {
	row := s.db.QueryRow(ctx, `
		SELECT
			id, name, description,
			system_prompt, agent_type,
			model, planner_model,
			tools, sub_agents, approval_required_tools,
			evaluator_enabled, max_iterations, memory_policy,
			sandbox_backend, is_public, owner_user_id
		FROM agents
		WHERE id = $1`,
		agentID,
	)

	var (
		toolsJSON     []byte
		subAgentsJSON []byte
		approvalJSON  []byte
		policyJSON    []byte
		description   *string
		plannerModel  *string
	)

	spec := &AgentSpec{}
	err := row.Scan(
		&spec.ID, &spec.Name, &description,
		&spec.SystemPrompt, &spec.AgentType,
		&spec.Model, &plannerModel,
		&toolsJSON, &subAgentsJSON, &approvalJSON,
		&spec.EvaluatorEnabled, &spec.MaxIterations, &policyJSON,
		&spec.SandboxBackend, &spec.IsPublic, &spec.OwnerUserID,
	)
	if err != nil {
		// pgx returns pgx.ErrNoRows; we use a string match to stay import-free
		if err.Error() == "no rows in result set" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("agentstore.Load: %w", err)
	}

	if description != nil {
		spec.Description = *description
	}
	if plannerModel != nil {
		spec.PlannerModel = *plannerModel
	}

	// Ownership check: agent must be public OR owned by the requesting user.
	if !spec.IsPublic && spec.OwnerUserID != requestingUserID {
		return nil, ErrForbidden
	}

	// Unmarshal JSON arrays
	_ = json.Unmarshal(toolsJSON, &spec.Tools)
	_ = json.Unmarshal(subAgentsJSON, &spec.SubAgents)
	_ = json.Unmarshal(approvalJSON, &spec.ApprovalRequiredTools)

	// Memory policy — fall back to defaults for missing fields
	spec.MemoryPolicy = DefaultMemoryPolicy
	if len(policyJSON) > 0 {
		_ = json.Unmarshal(policyJSON, &spec.MemoryPolicy)
		if spec.MemoryPolicy.TopKRead == 0 {
			spec.MemoryPolicy.TopKRead = DefaultMemoryPolicy.TopKRead
		}
		if spec.MemoryPolicy.MinScoreToWrite == 0 {
			spec.MemoryPolicy.MinScoreToWrite = DefaultMemoryPolicy.MinScoreToWrite
		}
	}

	return spec, nil
}

// ToSpecJSON serialises the AgentSpec back to the JSON form consumed by the planner.
func (s *AgentSpec) ToSpecJSON() string {
	type specJSON struct {
		Tools        []string `json:"tools"`
		SubAgents    []string `json:"sub_agents,omitempty"`
		SystemPrompt string   `json:"system_prompt"`
		AgentType    string   `json:"agent_type"`
	}
	b, _ := json.Marshal(specJSON{
		Tools:        s.Tools,
		SubAgents:    s.SubAgents,
		SystemPrompt: s.SystemPrompt,
		AgentType:    s.AgentType,
	})
	return string(b)
}
