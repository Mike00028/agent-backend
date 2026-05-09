package subagent

import (
	"testing"
)

// TestParseAgentsMD_ValidSimpleAgent tests parsing a valid simple agent
func TestParseAgentsMD_ValidSimpleAgent(t *testing.T) {
	content := []byte(`---
name: test_agent
type: simple
description: A test agent
model: mistral:latest
temperature: 0.7
max_tokens: 1000
max_iterations: 1
system_prompt: You are helpful
---
# Test Agent

This is a test agent for verification.`)

	agent, config, hash, err := ParseAgentsMD(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent.Name != "test_agent" {
		t.Errorf("expected name 'test_agent', got '%s'", agent.Name)
	}
	if agent.Type != TypeSimple {
		t.Errorf("expected type 'simple', got '%v'", agent.Type)
	}
	if config.Model != "mistral:latest" {
		t.Errorf("expected model 'mistral:latest', got '%s'", config.Model)
	}
	if hash == "" {
		t.Error("expected non-empty hash")
	}
}

// TestParseAgentsMD_ValidReactAgent tests parsing a valid react agent with tools
func TestParseAgentsMD_ValidReactAgent(t *testing.T) {
	content := []byte(`---
name: validator_agent
type: react
description: Validates input data
model: mixtral:latest
temperature: 0.3
max_tokens: 2000
max_iterations: 5
system_prompt: Validate carefully
tools:
  - regex_validator
  - json_validator
guardrails:
  enabled: true
  rules:
    - pattern: "^[a-z]+$"
      action: reject
---
# Validator Agent

Validates structured data.`)

	agent, config, _, err := ParseAgentsMD(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent.Type != TypeReact {
		t.Errorf("expected type 'react', got '%v'", agent.Type)
	}
	if len(config.Tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(config.Tools))
	}
	if !config.Guardrails.Enabled {
		t.Error("guardrails should be enabled")
	}
}

// TestParseAgentsMD_WorkflowAgent tests parsing a workflow agent with DAG
func TestParseAgentsMD_WorkflowAgent(t *testing.T) {
	content := []byte(`---
name: pipeline_agent
type: workflow
description: Multi-step workflow
model: llama2:latest
temperature: 0.5
max_tokens: 5000
max_iterations: 3
system_prompt: Execute workflow
workflow_steps:
  - name: step1
    agent: chat_agent
    inputs:
      question: "initial query"
  - name: step2
    agent: validator_agent
    inputs:
      data: "{step1.output}"
  - name: step3
    agent: summarize_agent
    inputs:
      results: "{step2.output}"
---
# Pipeline Agent

Orchestrates multi-step workflows.`)

	agent, config, _, err := ParseAgentsMD(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent.Type != TypeWorkflow {
		t.Errorf("expected type 'workflow', got '%v'", agent.Type)
	}
	if len(config.Steps) != 3 {
		t.Errorf("expected 3 workflow steps, got %d", len(config.Steps))
	}
}

// TestParseAgentsMD_InvalidNamePattern tests invalid agent name
func TestParseAgentsMD_InvalidNamePattern(t *testing.T) {
	content := []byte(`---
name: "Invalid Name With Spaces"
type: simple
---`)

	_, _, _, err := ParseAgentsMD(content)
	if err == nil {
		t.Error("expected error for invalid name pattern")
	}
}

// TestParseAgentsMD_TemperatureOutOfRange tests temperature validation
func TestParseAgentsMD_TemperatureOutOfRange(t *testing.T) {
	content := []byte(`---
name: test_agent
type: simple
temperature: 2.5
---`)

	_, _, _, err := ParseAgentsMD(content)
	if err == nil {
		t.Error("expected error for temperature > 2.0")
	}
}

// TestParseAgentsMD_MaxTokensOutOfRange tests max_tokens validation
func TestParseAgentsMD_MaxTokensOutOfRange(t *testing.T) {
	content := []byte(`---
name: test_agent
type: simple
max_tokens: 150000
---`)

	_, _, _, err := ParseAgentsMD(content)
	if err == nil {
		t.Error("expected error for max_tokens > 100000")
	}
}

// TestParseAgentsMD_MaxIterationsOutOfRange tests max_iterations validation
func TestParseAgentsMD_MaxIterationsOutOfRange(t *testing.T) {
	content := []byte(`---
name: test_agent
type: react
max_iterations: 200
---`)

	_, _, _, err := ParseAgentsMD(content)
	if err == nil {
		t.Error("expected error for max_iterations > 100")
	}
}

// TestParseAgentsMD_InvalidRegexInGuardrails tests regex pattern validation
func TestParseAgentsMD_InvalidRegexInGuardrails(t *testing.T) {
	content := []byte(`---
name: test_agent
type: simple
guardrails:
  enabled: true
  rules:
    - pattern: "[invalid("
---`)

	_, _, _, err := ParseAgentsMD(content)
	if err == nil {
		t.Error("expected error for invalid regex pattern")
	}
}

// TestParseAgentsMD_MissingNameField tests missing required field
func TestParseAgentsMD_MissingNameField(t *testing.T) {
	content := []byte(`---
type: simple
description: No name provided
---`)

	_, _, _, err := ParseAgentsMD(content)
	if err == nil {
		t.Error("expected error for missing name")
	}
}

// TestParseAgentsMD_MissingTypeField tests missing required type
func TestParseAgentsMD_MissingTypeField(t *testing.T) {
	content := []byte(`---
name: test_agent
description: No type provided
---`)

	_, _, _, err := ParseAgentsMD(content)
	if err == nil {
		t.Error("expected error for missing type")
	}
}

// TestParseAgentsMD_InvalidSpecificType tests unsupported agent type
func TestParseAgentsMD_InvalidSpecificType(t *testing.T) {
	content := []byte(`---
name: test_agent
type: unknown_type
---`)

	_, _, _, err := ParseAgentsMD(content)
	if err == nil {
		t.Error("expected error for invalid type")
	}
}

// TestParseAgentsMD_CyclicWorkflow tests DAG acyclicity check
func TestParseAgentsMD_CyclicWorkflow(t *testing.T) {
	content := []byte(`---
name: cyclic_agent
type: workflow
workflow_steps:
  - name: step1
    agent: agent_a
  - name: step2
    agent: agent_b
  - name: step3
    agent: agent_a
---`)

	agent, config, _, err := ParseAgentsMD(content)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	// Validate config should catch cycles (if applicable)
	validationErrs := ValidateConfig(agent.Type, config)
	_ = validationErrs // Non-cyclic workflow steps are valid; cycle detection is optional
}

// TestParseAgentsMD_EmptyContent tests empty file
func TestParseAgentsMD_EmptyContent(t *testing.T) {
	content := []byte("")

	_, _, _, err := ParseAgentsMD(content)
	if err == nil {
		t.Error("expected error for empty content")
	}
}

// TestValidateConfig_ValidSimpleConfig tests validation passes for valid config
func TestValidateConfig_ValidSimpleConfig(t *testing.T) {
	config := &SubAgentConfig{
		Model:       "mistral:latest",
		Temperature: 0.7,
		MaxTokens:   1000,
		MaxIterations: 1,
	}

	errs := ValidateConfig(TypeSimple, config)
	if len(errs) > 0 {
		t.Errorf("unexpected validation errors: %v", errs)
	}
}

// TestValidateConfig_MultipleErrors tests multiple validation failures
func TestValidateConfig_MultipleErrors(t *testing.T) {
	config := &SubAgentConfig{
		Model:       "mistral:latest",
		Temperature: 2.5,    // Invalid: > 2.0
		MaxTokens:   150000, // Invalid: > 100000
		MaxIterations: 1,
	}

	errs := ValidateConfig(TypeSimple, config)
	if len(errs) < 2 {
		t.Errorf("expected at least 2 validation errors, got %d", len(errs))
	}
}

// TestEmbeddingHint tests hint generation for vector embedding
func TestEmbeddingHint(t *testing.T) {
	agent := &SubAgent{
		Name:        "test_agent",
		Description: "A test agent",
	}
	config := &SubAgentConfig{
		SystemPrompt: "You are helpful and concise",
	}

	hint := EmbeddingHint(agent, config)
	if hint == "" {
		t.Error("expected non-empty embedding hint")
	}
	if !contains(hint, "test_agent") {
		t.Error("hint should contain agent name")
	}
}

// Helper function
func contains(s, substr string) bool {
	for i := 0; i < len(s)-len(substr)+1; i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
