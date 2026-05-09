package subagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// ParseFlowise parses a Flowise agent export (JSON format).
// Returns: SubAgent (metadata), SubAgentConfig (parsed config), schema_hash, error.
func ParseFlowise(content []byte) (*SubAgent, *SubAgentConfig, string, error) {
	// Parse JSON
	var flowisDef struct {
		Name        string                 `json:"name"`
		Type        string                 `json:"type"` // "agent" or "flow"
		Description string                 `json:"description"`
		Nodes       []FlowiseNode          `json:"nodes"`
		Edges       []FlowiseEdge          `json:"edges"`
		Data        map[string]interface{} `json:"data"`
		Metadata    map[string]interface{} `json:"metadata"`
	}

	if err := json.Unmarshal(content, &flowisDef); err != nil {
		return nil, nil, "", fmt.Errorf("failed to parse Flowise JSON: %w", err)
	}

	if flowisDef.Name == "" {
		return nil, nil, "", fmt.Errorf("validation_error: name is required in Flowise definition")
	}

	if err := validateAgentName(flowisDef.Name); err != nil {
		return nil, nil, "", err
	}

	// Determine agent type by analyzing node graph
	// Simple: single ChatMessage/OpenAI node with no tools
	// React: OpenAI agent with tool nodes
	// Workflow: explicit DAG with multiple steps
	agentType, config, err := parseFlowiseNodes(flowisDef.Nodes, flowisDef.Edges)
	if err != nil {
		return nil, nil, "", err
	}

	// Extract model from LLM node if found
	if config.Model == "" {
		// Try to infer from metadata or scan nodes
		config.Model = "gpt-4-turbo" // Default fallback
	}

	// Compute schema hash
	hash := sha256.Sum256(content)
	schemaHash := hex.EncodeToString(hash[:])

	// Build SubAgent metadata
	agent := &SubAgent{
		Name:         flowisDef.Name,
		Type:         agentType,
		Description:  flowisDef.Description,
		SourceFormat: FormatFlowise,
		IsEnabled:    true,
		IsShared:     false,
		IsSystem:     false,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
		Version:      1,
	}

	// Extract tags and category from metadata
	if flowisDef.Metadata != nil {
		if tags, ok := flowisDef.Metadata["tags"].([]interface{}); ok {
			for _, t := range tags {
				if ts, ok := t.(string); ok {
					agent.Tags = append(agent.Tags, ts)
				}
			}
		}
		if cat, ok := flowisDef.Metadata["category"].(string); ok {
			agent.Category = cat
		}
	}

	return agent, config, schemaHash, nil
}

// FlowiseNode represents a Flowise node (component).
type FlowiseNode struct {
	ID    string                 `json:"id"`
	Type  string                 `json:"type"` // "ChatOpenAI", "Tool", "Agent", etc.
	Label string                 `json:"label"`
	Data  map[string]interface{} `json:"data"`
	Pos   struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	} `json:"position"`
}

// FlowiseEdge represents a connection between nodes.
type FlowiseEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Label  string `json:"label"`
}

// parseFlowiseNodes analyzes nodes and edges to infer agent type and config.
func parseFlowiseNodes(nodes []FlowiseNode, edges []FlowiseEdge) (SubAgentType, *SubAgentConfig, error) {
	if len(nodes) == 0 {
		return "", nil, fmt.Errorf("validation_error: Flowise definition must contain at least one node")
	}

	config := &SubAgentConfig{
		Temperature:   0.7,
		MaxTokens:     4096,
		MaxIterations: 10,
		Tools:         []string{},
	}

	// Build node map
	nodeMap := make(map[string]*FlowiseNode)
	for i := range nodes {
		nodeMap[nodes[i].ID] = &nodes[i]
	}

	// Build adjacency list
	outgoing := make(map[string][]string)
	for _, edge := range edges {
		outgoing[edge.Source] = append(outgoing[edge.Source], edge.Target)
	}

	// Scan for LLM node (ChatOpenAI, ChatAnthropic, etc.)
	var llmNodeID string
	var agentNodeID string
	toolNodes := []string{}

	for _, node := range nodes {
		switch node.Type {
		case "ChatOpenAI", "ChatAnthropic", "ChatGoogle", "ChatMistral":
			llmNodeID = node.ID

			// Extract model
			if model, ok := node.Data["model"].(string); ok {
				config.Model = model
			}

			// Extract temperature
			if temp, ok := node.Data["temperature"].(float64); ok {
				config.Temperature = temp
			}

			// Extract max_tokens / maxOutputTokens
			if mt, ok := node.Data["max_tokens"].(float64); ok {
				config.MaxTokens = int(mt)
			} else if mt, ok := node.Data["maxOutputTokens"].(float64); ok {
				config.MaxTokens = int(mt)
			}

			// Extract system prompt
			if sp, ok := node.Data["system_prompt"].(string); ok {
				config.SystemPrompt = sp
			}

		case "Agent", "ReActAgent", "OpenAIAgent":
			agentNodeID = node.ID

		case "Tool", "RetrieverTool", "MongoDBANGSTool":
			toolNodes = append(toolNodes, node.ID)

			// Extract tool name
			if label, ok := node.Data["name"].(string); ok {
				if label != "" {
					config.Tools = append(config.Tools, label)
				} else {
					config.Tools = append(config.Tools, node.Label)
				}
			}
		}
	}

	// Infer agent type based on node composition
	var inferredType SubAgentType

	if agentNodeID != "" {
		// Explicit agent node -> React or Workflow depending on structure
		if len(toolNodes) > 0 {
			inferredType = TypeReact
		} else {
			inferredType = TypeWorkflow
		}
	} else if len(toolNodes) > 0 {
		// Tools present with LLM -> React
		inferredType = TypeReact
	} else if llmNodeID != "" {
		// Just LLM node -> Simple
		inferredType = TypeSimple
	} else {
		// Fallback
		inferredType = TypeReact
	}

	// If workflow-like structure, extract steps from DAG
	if inferredType == TypeWorkflow && len(nodes) > 1 {
		steps := extractFlowiseSteps(nodeMap, outgoing, nodes)
		config.Steps = steps
	}

	return inferredType, config, nil
}

// extractFlowiseSteps converts Flowise nodes/edges into WorkflowStep structs.
func extractFlowiseSteps(nodeMap map[string]*FlowiseNode, outgoing map[string][]string, nodes []FlowiseNode) []WorkflowStep {
	var steps []WorkflowStep

	// Create a step for each meaningful node
	for _, node := range nodes {
		// Skip visual/configuration-only nodes
		if isVisualNodeOnly(node.Type) {
			continue
		}

		// Determine inputs/outputs based on edges
		var inputs, outputs []string

		// Incoming edges = inputs
		for _, otherNode := range nodes {
			for _, target := range outgoing[otherNode.ID] {
				if target == node.ID {
					inputs = append(inputs, otherNode.Label)
				}
			}
		}

		// Outgoing edges = outputs
		for _, target := range outgoing[node.ID] {
			if targetNode, ok := nodeMap[target]; ok {
				outputs = append(outputs, targetNode.Label)
			}
		}

		step := WorkflowStep{
			Name:    node.Label,
			Agent:   node.Type, // Or map to known agent name
			Inputs:  inputs,
			Outputs: outputs,
		}

		steps = append(steps, step)
	}

	return steps
}

// isVisualNodeOnly checks if node type is purely visual (no execution).
func isVisualNodeOnly(nodeType string) bool {
	switch nodeType {
	case "Note", "Group", "Reroute", "Text", "CustomJs":
		return true
	default:
		return false
	}
}

// FlowiseFormat handles parsing generic Flowise JSON with flexible node structure.
// This is a more robust version that handles various Flowise export formats.
func FlowiseFormat(content []byte) (*SubAgent, *SubAgentConfig, string, error) {
	var rawDef map[string]interface{}
	if err := json.Unmarshal(content, &rawDef); err != nil {
		return nil, nil, "", fmt.Errorf("invalid JSON: %w", err)
	}

	// Try standard ParseFlowise first
	agent, config, parsedHash, err := ParseFlowise(content)
	if err == nil {
		return agent, config, parsedHash, nil
	}

	// Fallback: try minimal format with just name + nodes
	name, _ := rawDef["name"].(string)
	if name == "" {
		return nil, nil, "", fmt.Errorf("name field is required")
	}

	if err := validateAgentName(name); err != nil {
		return nil, nil, "", err
	}

	// Extract description
	description, _ := rawDef["description"].(string)

	// Default config
	config = &SubAgentConfig{
		Model:         "gpt-4-turbo",
		Temperature:   0.7,
		MaxTokens:     4096,
		MaxIterations: 10,
		Tools:         []string{},

	}

	agent = &SubAgent{
		Name:         name,
		Type:         TypeReact, // Default to react for unknown formats
		Description:  description,
		SourceFormat: FormatFlowise,
		IsEnabled:    true,
		IsShared:     false,
		IsSystem:     false,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
		Version:      1,
	}

	hash := sha256.Sum256(content)
	schemaHash := hex.EncodeToString(hash[:])

	return agent, config, schemaHash, nil
}
