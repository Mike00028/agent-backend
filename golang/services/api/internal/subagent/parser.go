package subagent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ParseAgentsMD parses an agents.md file (YAML frontmatter + markdown body).
// Returns: SubAgent (metadata), SubAgentConfig (parsed config), schema_hash, error.
func ParseAgentsMD(content []byte) (*SubAgent, *SubAgentConfig, string, error) {
	// Split YAML frontmatter from markdown body
	const delimiter = "---"

	parts := bytes.SplitN(content, []byte("\n"+delimiter+"\n"), 2)
	if len(parts) < 2 {
		return nil, nil, "", fmt.Errorf("malformed agents.md: missing YAML frontmatter (must start with --- and end with --- on separate lines)")
	}

	yamlContent := parts[0]
	if bytes.HasPrefix(yamlContent, []byte(delimiter)) {
		yamlContent = bytes.TrimPrefix(yamlContent, []byte(delimiter))
	}
	yamlContent = bytes.TrimPrefix(yamlContent, []byte("\n"))

	restParts := bytes.SplitN(parts[1], []byte("\n"), 2)
	var description string
	if len(restParts) > 1 {
		description = strings.TrimSpace(string(restParts[1]))
	}

	// Parse YAML frontmatter
	var frontmatter struct {
		Name        string          `yaml:"name"`
		Type        string          `yaml:"type"`
		Model       string          `yaml:"model"`
		Temperature *float64        `yaml:"temperature"`
		MaxTokens   *int            `yaml:"max_tokens"`
		MaxIter     *int            `yaml:"max_iterations"`
		Tools       []string        `yaml:"tools"`
		Approval    []string        `yaml:"approval_required"`
		DelegatesTo []string        `yaml:"delegates_to"`
		OutputFmt   string          `yaml:"output_format"`
		OutputSch   json.RawMessage `yaml:"output_schema"`
		Memory      struct {
			Type           string   `yaml:"type"`
			Retention      *int     `yaml:"retention_days"`
			MinScoreToRead *float64 `yaml:"min_score_to_read"`
			MinScoreWrite  *float64 `yaml:"min_score_to_write"`
			MaxTokens      *int     `yaml:"max_tokens"`
		} `yaml:"memory"`
		Guardrails struct {
			Enabled    *bool    `yaml:"enabled"`
			Rules      []string `yaml:"rules"`
			Blacklist  []string `yaml:"blacklist_patterns"`
			Whitelist  []string `yaml:"whitelist_patterns"`
			MaxRetries *int     `yaml:"max_retries"`
			RetryDelay *string  `yaml:"retry_delay"`
		} `yaml:"guardrails"`
		SystemPrompt string          `yaml:"system_prompt"`
		Steps        []interface{}   `yaml:"steps"`
		Tags         []string        `yaml:"tags"`
		Category     string                   `yaml:"category"`
		Version      *int                     `yaml:"version"`
	}

	if err := yaml.Unmarshal(yamlContent, &frontmatter); err != nil {
		return nil, nil, "", fmt.Errorf("failed to parse YAML frontmatter: %w", err)
	}

	// Validate required fields
	if frontmatter.Name == "" {
		return nil, nil, "", fmt.Errorf("validation_error: name is required")
	}
	if err := validateAgentName(frontmatter.Name); err != nil {
		return nil, nil, "", err
	}

	if frontmatter.Type == "" {
		return nil, nil, "", fmt.Errorf("validation_error: type is required (must be 'simple', 'react', or 'workflow')")
	}
	agentType := SubAgentType(frontmatter.Type)
	if err := validateAgentType(agentType); err != nil {
		return nil, nil, "", err
	}

	if frontmatter.Model == "" {
		return nil, nil, "", fmt.Errorf("validation_error: model is required")
	}

	// Set defaults
	temperature := 0.7
	if frontmatter.Temperature != nil {
		temperature = *frontmatter.Temperature
	}
	if temperature < 0.0 || temperature > 2.0 {
		return nil, nil, "", fmt.Errorf("validation_error: temperature must be between 0.0 and 2.0")
	}

	maxTokens := 4096
	if frontmatter.MaxTokens != nil {
		maxTokens = *frontmatter.MaxTokens
	}
	if maxTokens < 100 || maxTokens > 100000 {
		return nil, nil, "", fmt.Errorf("validation_error: max_tokens must be between 100 and 100000")
	}

	maxIterations := 10
	if frontmatter.MaxIter != nil {
		maxIterations = *frontmatter.MaxIter
	}
	if maxIterations < 1 || maxIterations > 100 {
		return nil, nil, "", fmt.Errorf("validation_error: max_iterations must be between 1 and 100")
	}

	// Build config
	config := &SubAgentConfig{
		Model:            frontmatter.Model,
		Temperature:      temperature,
		MaxTokens:        maxTokens,
		MaxIterations:    maxIterations,
		Tools:            frontmatter.Tools,
		ApprovalRequired: frontmatter.Approval,
		DelegatesTo:      frontmatter.DelegatesTo,
		OutputFormat:     frontmatter.OutputFmt,
		OutputSchema:     frontmatter.OutputSch,
		SystemPrompt:     frontmatter.SystemPrompt,
	}

	// Parse memory config
	if frontmatter.Memory.Type != "" {
		config.Memory = MemoryConfig{
			Type:            frontmatter.Memory.Type,
			RetentionDays:   frontmatter.Memory.Retention,
			MinScoreToRead:  frontmatter.Memory.MinScoreToRead,
			MinScoreToWrite: frontmatter.Memory.MinScoreWrite,
			MaxTokens:       frontmatter.Memory.MaxTokens,
		}
	}

	// Parse guardrails config
	if frontmatter.Guardrails.Enabled != nil && *frontmatter.Guardrails.Enabled {
		config.Guardrails = GuardrailConfig{
			Enabled:           *frontmatter.Guardrails.Enabled,
			Rules:             frontmatter.Guardrails.Rules,
			BlacklistPatterns: frontmatter.Guardrails.Blacklist,
			WhitelistPatterns: frontmatter.Guardrails.Whitelist,
			MaxRetries:        frontmatter.Guardrails.MaxRetries,
			RetryDelay:        frontmatter.Guardrails.RetryDelay,
		}

		// Validate regex patterns
		for _, pattern := range frontmatter.Guardrails.Blacklist {
			if _, err := regexp.Compile(pattern); err != nil {
				return nil, nil, "", fmt.Errorf("validation_error: invalid regex in guardrails.blacklist_patterns: %w", err)
			}
		}
		for _, pattern := range frontmatter.Guardrails.Whitelist {
			if _, err := regexp.Compile(pattern); err != nil {
				return nil, nil, "", fmt.Errorf("validation_error: invalid regex in guardrails.whitelist_patterns: %w", err)
			}
		}
	}

	// Parse workflow steps (for type=workflow)
	if frontmatter.Steps != nil {
		steps := make([]WorkflowStep, 0, len(frontmatter.Steps))
		for i, rawStep := range frontmatter.Steps {
			stepMap, ok := rawStep.(map[string]interface{})
			if !ok {
				return nil, nil, "", fmt.Errorf("validation_error: step %d must be a YAML object", i)
			}

			stepName, _ := stepMap["name"].(string)
			if stepName == "" {
				return nil, nil, "", fmt.Errorf("validation_error: step %d missing name", i)
			}

			stepAgent, _ := stepMap["agent"].(string)
			if stepAgent == "" && frontmatter.Type == "workflow" {
				return nil, nil, "", fmt.Errorf("validation_error: step %d missing agent reference", i)
			}

			outputs := []string{}
			if outList, ok := stepMap["outputs"].([]interface{}); ok {
				for _, o := range outList {
					if os, ok := o.(string); ok {
						outputs = append(outputs, os)
					}
				}
			}

			inputs := []string{}
			if inList, ok := stepMap["inputs"].([]interface{}); ok {
				for _, o := range inList {
					if is, ok := o.(string); ok {
						inputs = append(inputs, is)
					}
				}
			}

			step := WorkflowStep{
				Name:    stepName,
				Agent:   stepAgent,
				Inputs:  inputs,
				Outputs: outputs,
			}

			// Validate step.Agent references a known agent or is a built-in tool
			steps = append(steps, step)
		}
		config.Steps = steps
	}

	// Compute schema hash (hash of YAML frontmatter only, excluding markdown body)
	hash := sha256.Sum256(yamlContent)
	schemaHash := hex.EncodeToString(hash[:])

	// Build SubAgent metadata
	agent := &SubAgent{
		Name:         frontmatter.Name,
		Type:         agentType,
		Description:  strings.TrimSpace(description),
		SourceFormat: FormatAgentsMD,
		IsEnabled:    true,
		IsShared:     false,
		IsSystem:     false,
		Tags:         frontmatter.Tags,
		Category:     frontmatter.Category,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	if frontmatter.Version != nil {
		agent.Version = *frontmatter.Version
	} else {
		agent.Version = 1
	}

	return agent, config, schemaHash, nil
}

// ValidateAgentName ensures name matches pattern: lowercase alphanumeric + underscore/hyphen.
func validateAgentName(name string) error {
	if name == "" {
		return fmt.Errorf("validation_error: name is required")
	}

	// Pattern: lowercase alphanumeric, underscore, hyphen; 1-100 chars; cannot start with number
	pattern := regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,99}$`)
	if !pattern.MatchString(name) {
		return fmt.Errorf("validation_error: name must match pattern: ^[a-z_][a-z0-9_-]{0,99}$")
	}

	return nil
}

// ValidateAgentType checks if type is valid.
func validateAgentType(t SubAgentType) error {
	switch t {
	case TypeSimple, TypeReact, TypeWorkflow:
		return nil
	default:
		return fmt.Errorf("validation_error: type must be 'simple', 'react', or 'workflow' (got '%s')", t)
	}
}

// ValidateConfig performs deep validation on a SubAgentConfig.
func ValidateConfig(agentType SubAgentType, config *SubAgentConfig) []ValidationError {
	var errs []ValidationError

	// Model format check
	if config.Model == "" {
		errs = append(errs, ValidationError{
			Field:   "model",
			Code:    "required",
			Message: "model is required",
		})
	}

	// Temperature range
	if config.Temperature < 0.0 || config.Temperature > 2.0 {
		errs = append(errs, ValidationError{
			Field:   "temperature",
			Code:    "out_of_range",
			Message: "must be between 0.0 and 2.0",
		})
	}

	// Max tokens range
	if config.MaxTokens < 100 || config.MaxTokens > 100000 {
		errs = append(errs, ValidationError{
			Field:   "max_tokens",
			Code:    "out_of_range",
			Message: "must be between 100 and 100000",
		})
	}

	// Max iterations range
	if config.MaxIterations < 1 || config.MaxIterations > 100 {
		errs = append(errs, ValidationError{
			Field:   "max_iterations",
			Code:    "out_of_range",
			Message: "must be between 1 and 100",
		})
	}

	// Output schema validation (if provided)
	if config.OutputSchema != nil && len(config.OutputSchema) > 0 {
		var schema map[string]interface{}
		if err := json.Unmarshal(config.OutputSchema, &schema); err != nil {
			errs = append(errs, ValidationError{
				Field:   "output_schema",
				Code:    "invalid_json",
				Message: fmt.Sprintf("must be valid JSON Schema: %v", err),
			})
		}
	}

	// Validate guardrail patterns (regex compilation)
	for i, pattern := range config.Guardrails.BlacklistPatterns {
		if _, err := regexp.Compile(pattern); err != nil {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("guardrails.blacklist_patterns[%d]", i),
				Code:    "invalid_regex",
				Message: fmt.Sprintf("invalid regex: %v", err),
			})
		}
	}
	for i, pattern := range config.Guardrails.WhitelistPatterns {
		if _, err := regexp.Compile(pattern); err != nil {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("guardrails.whitelist_patterns[%d]", i),
				Code:    "invalid_regex",
				Message: fmt.Sprintf("invalid regex: %v", err),
			})
		}
	}

	// Workflow-specific validation
	if agentType == TypeWorkflow {
		if len(config.Steps) == 0 {
			errs = append(errs, ValidationError{
				Field:   "steps",
				Code:    "required",
				Message: "workflow agents must define at least one step",
			})
		}

		// Check for DAG acyclicity
		if !isDAGAcyclic(config.Steps) {
			errs = append(errs, ValidationError{
				Field:   "steps",
				Code:    "cyclic_dependency",
				Message: "workflow steps form a cycle (dependency graph must be acyclic)",
			})
		}
	}

	return errs
}

// isDAGAcyclic checks if workflow steps form an acyclic DAG.
// Uses DFS with white/gray/black coloring.
func isDAGAcyclic(steps []WorkflowStep) bool {
	// Build adjacency list from outputs -> inputs
	adjList := make(map[string][]string)
	stepsByName := make(map[string]*WorkflowStep)

	for i := range steps {
		adjList[steps[i].Name] = []string{}
		stepsByName[steps[i].Name] = &steps[i]
	}

	// Add edges: step A -> step B if B uses A's outputs as inputs
	for i := range steps {
		for _, input := range steps[i].Inputs {
			// Find which step produced this input
			for j := range steps {
				if i != j {
					for _, output := range steps[j].Outputs {
						if output == input {
							adjList[steps[j].Name] = append(adjList[steps[j].Name], steps[i].Name)
						}
					}
				}
			}
		}
	}

	// DFS cycle detection
	state := make(map[string]int) // 0=white, 1=gray, 2=black
	for name := range adjList {
		state[name] = 0
	}

	for name := range adjList {
		if state[name] == 0 {
			if !dfsHasCycle(name, adjList, state) {
				return false
			}
		}
	}

	return true
}

// dfsHasCycle returns true if the graph is acyclic (no cycle found).
func dfsHasCycle(node string, adjList map[string][]string, state map[string]int) bool {
	state[node] = 1 // Mark as visiting (gray)

	for _, neighbor := range adjList[node] {
		if state[neighbor] == 1 {
			// Back edge found (cycle)
			return false
		}
		if state[neighbor] == 0 {
			if !dfsHasCycle(neighbor, adjList, state) {
				return false
			}
		}
	}

	state[node] = 2 // Mark as visited (black)
	return true
}

// EmbeddingHint generates a string for vector embedding: "name: description".
func EmbeddingHint(agent *SubAgent, config *SubAgentConfig) string {
	parts := []string{agent.Name}

	if agent.Description != "" {
		parts = append(parts, agent.Description)
	}

	if config.SystemPrompt != "" {
		// First 200 chars of system prompt
		sp := config.SystemPrompt
		if len(sp) > 200 {
			sp = sp[:200]
		}
		parts = append(parts, sp)
	}

	return strings.Join(parts, ": ")
}
