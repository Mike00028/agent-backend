package subagent

import (
	"encoding/json"
	"time"
)

// SubAgentType discriminates agent execution mode
type SubAgentType string

const (
	TypeSimple   SubAgentType = "simple"   // single LLM call, no tools
	TypeReact    SubAgentType = "react"    // ReAct loop: think → tool → observe
	TypeWorkflow SubAgentType = "workflow" // pre-built DAG from steps[]
)

// SourceFormat indicates input format
type SourceFormat string

const (
	FormatAgentsMD SourceFormat = "agents.md" // YAML frontmatter + markdown
	FormatFlowise  SourceFormat = "flowise"   // Flowise JSON export
)

// MemoryConfig controls memory read/write for this agent
type MemoryConfig struct {
	Type            string   `json:"type"`               // "shortterm" | "longterm" | "hybrid"
	RetentionDays   *int     `json:"retention_days"`     // how long to keep memories
	MinScoreToRead  *float64 `json:"min_score_to_read"`  // semantic threshold for retrieval
	MinScoreToWrite *float64 `json:"min_score_to_write"` // semantic threshold to persist
	MaxTokens       *int     `json:"max_tokens"`         // max tokens to include in context
}

// GuardrailConfig for input/output validation
type GuardrailConfig struct {
	Enabled           bool     `json:"enabled"`
	Rules             []string `json:"rules"`              // human-readable rules
	BlacklistPatterns []string `json:"blacklist_patterns"` // regex patterns to reject
	WhitelistPatterns []string `json:"whitelist_patterns"` // regex patterns to allow
	MaxRetries        *int     `json:"max_retries"`        // max retry attempts on guardrail violation
	RetryDelay        *string  `json:"retry_delay"`        // duration format (e.g., "1s", "100ms")
}

// WorkflowStep defines a single DAG task in a workflow agent
type WorkflowStep struct {
	Name    string   `json:"name"`    // step name (human-readable)
	Agent   string   `json:"agent"`   // agent name or type to invoke
	Inputs  []string `json:"inputs"`  // input variable names (what feeds into this step)
	Outputs []string `json:"outputs"` // output variable names (what this step produces)
}

// RetryPolicy configures backoff for a step or tool
type RetryPolicy struct {
	MaxRetries        int     `json:"max_retries"`
	BackoffMultiplier float64 `json:"backoff_multiplier"`
	InitialDelayMs    int     `json:"initial_delay_ms"`
	MaxDelayMs        int     `json:"max_delay_ms"`
}

// SubAgentConfig is the parsed JSONB config stored in DB
type SubAgentConfig struct {
	// Core execution
	Model          string  `json:"model"`           // LLM identifier
	Temperature    float64 `json:"temperature"`     // 0.0 - 2.0
	MaxTokens      int     `json:"max_tokens"`      // per-call cap
	MaxIterations  int     `json:"max_iterations"`  // ReAct loop limit
	TimeoutSeconds int     `json:"timeout_seconds"` // hard wall-clock limit
	ExecutionMode  string  `json:"execution_mode"`  // "sequential" | "parallel" (workflow default)

	// Tool surface
	Tools            []string `json:"tools"`             // allowed tool names
	ApprovalRequired []string `json:"approval_required"` // tools that need HITL approval
	DelegatesTo      []string `json:"delegates_to"`      // agents this agent can sub-task

	// Output
	OutputFormat string          `json:"output_format"` // "text" | "json" | "markdown"
	OutputSchema json.RawMessage `json:"output_schema"` // JSON Schema for structured output

	// Memory
	Memory MemoryConfig `json:"memory"`

	// Guardrails
	Guardrails GuardrailConfig `json:"guardrails"`

	// Observability
	TraceTags map[string]string `json:"trace_tags"`

	// Workflow steps (for type: workflow)
	Steps []WorkflowStep `json:"steps"`


	// User-facing hints
	InputHints string `json:"input_hints"`           // what input format the agent expects
	Notes      string `json:"notes"`                 // deployment/limitation notes
	Deprecated bool   `json:"deprecated"`            // if true, agent is deprecated
	Version    int    `json:"config_schema_version"` // version of this config schema

	// Internal use
	SystemPrompt string `json:"system_prompt"` // computed from markdown body during parse
}

// SubAgent is the database row + computed fields
type SubAgent struct {
	ID                string
	OwnerID           *string // nil for system agents
	Name              string
	Description       string
	Type              SubAgentType
	SourceFormat      SourceFormat
	Content           string // full original upload
	Config            SubAgentConfig
	SchemaHash        string // SHA-256 of content
	Embedding         []float32
	Tags              []string
	Category          string
	Version           int
	IsShared          bool
	IsSystem          bool
	IsEnabled         bool
	DeprecatedAt      *time.Time
	DeprecationNotice string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// CreateSubAgentRequest is the incoming form for POST /agents/upload
type CreateSubAgentRequest struct {
	Content  string   `form:"file"` // multipart file upload or raw body
	Shared   bool     `form:"shared"`
	Tags     []string `form:"tags"`
	Category string   `form:"category"`
}

// SubAgentResponse is the API response for get/create
type SubAgentResponse struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Description       string     `json:"description"`
	Type              string     `json:"type"`
	SourceFormat      string     `json:"source_format"`
	IsShared          bool       `json:"is_shared"`
	IsEnabled         bool       `json:"is_enabled"`
	Tags              []string   `json:"tags"`
	Category          string     `json:"category"`
	DeprecatedAt      *time.Time `json:"deprecated_at,omitempty"`
	DeprecationNotice string     `json:"deprecation_notice,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	// Content + Config omitted from list; included in get/:name
	Content *string         `json:"content,omitempty"`
	Config  *SubAgentConfig `json:"config,omitempty"`
}

// SubAgentListResponse is for GET /agents
type SubAgentListResponse struct {
	Agents []SubAgentResponse `json:"agents"`
	Total  int                `json:"total"`
}

// UpdateSubAgentRequest is the payload for PATCH /agents/:name
type UpdateSubAgentRequest struct {
	IsShared          *bool      `json:"is_shared"`
	IsEnabled         *bool      `json:"is_enabled"`
	Description       *string    `json:"description"`
	Tags              []string   `json:"tags"`
	Category          *string    `json:"category"`
	DeprecatedAt      *time.Time `json:"deprecated_at"`
	DeprecationNotice *string    `json:"deprecation_notice"`
}

// ValidationError groups field validation failures
type ValidationError struct {
	Field   string
	Message string
	Code    string // "required" | "invalid_format" | "conflict" | "out_of_range"
}

// ParseResult is returned after parsing YAML/Flowise
type ParseResult struct {
	Name        string
	Description string
	Type        SubAgentType
	Config      SubAgentConfig
	Content     string
	Errors      []ValidationError
}

// SearchQuery drives hybrid search at query time
type SearchQuery struct {
	UserID string       // current user for access control
	Query  string       // search text
	Vector []float32    // pre-computed embedding
	Tags   []string     // filter by tags (AND)
	Type   SubAgentType // filter by type
	Shared bool         // only shared agents
	Limit  int
}

// SearchResult is returned from hybrid search
type SearchResult struct {
	Agent SubAgent
	Score float64 // 0.3*keyword + 0.7*vector
}
