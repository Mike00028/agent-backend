package agentstore

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrNotFound is returned when the agent_id does not exist.
var ErrNotFound = errors.New("agent not found")

// SubAgentDef describes a custom (DB-loaded) agent available to the planner.
type SubAgentDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// AgentSpec is the canonical agent configuration.
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
	SubAgents []SubAgentDef // custom agents available in addition to the built-in registry

	ApprovalRequiredTools []string
	EvaluatorEnabled      bool
	MaxIterations         int
	MemoryPolicy          MemoryPolicy

	IsPublic    bool
	OwnerUserID string
}

// MemoryPolicy controls when memory is read and written for this agent.
type MemoryPolicy struct {
	WriteOnEvalOK   bool    `json:"write_on_eval_ok"`
	MinScoreToWrite float64 `json:"min_score_to_write"`
	TopKRead        int     `json:"top_k_read"`
}

// DefaultMemoryPolicy is used when no explicit memory_policy is set.
var DefaultMemoryPolicy = MemoryPolicy{
	WriteOnEvalOK:   true,
	MinScoreToWrite: 0.7,
	TopKRead:        3,
}

// Store returns the configured AgentSpec for any agent ID.
// The spec is set once at startup via New() — no database required.
// Future: swap New() for a DB-backed constructor when agent registry is added.
type Store struct {
	spec *AgentSpec
}

// New creates a Store with the given spec.
func New(spec *AgentSpec) *Store {
	return &Store{spec: spec}
}

// Load returns a copy of the agent spec (agentID ignored — single-tenant for now).
func (s *Store) Load(_ context.Context, _, _ string) (*AgentSpec, error) {
	spec := *s.spec
	return &spec, nil
}

// ToSpecJSON serialises the AgentSpec to the JSON form consumed by the planner.
func (s *AgentSpec) ToSpecJSON() string {
	type specJSON struct {
		Tools        []string      `json:"tools"`
		SubAgents    []SubAgentDef `json:"sub_agents,omitempty"`
		SystemPrompt string        `json:"system_prompt"`
		AgentType    string        `json:"agent_type"`
	}
	b, _ := json.Marshal(specJSON{
		Tools:        s.Tools,
		SubAgents:    s.SubAgents,
		SystemPrompt: s.SystemPrompt,
		AgentType:    s.AgentType,
	})
	return string(b)
}
